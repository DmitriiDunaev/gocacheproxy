// Command gocacheproxy is a GOCACHEPROG helper for code generation (go generate)
// that keeps the shared Go build cache fast while preventing it from being
// polluted by generators.
//
// Generators such as easyjson and gitoa.ru/go-4devs/config build a throwaway
// "bootstrap" program on every run. Each bootstrap has a unique temporary name,
// so its compiled object and linked binary get a unique cache key that is never
// hit again — they only accumulate as dead weight (well over a thousand entries
// per `go generate ./...`).
//
// This proxy works as a write-through cache with one exception: "get" requests
// are answered from the real cache, every "put" is written through to the real
// cache so dependencies are cached and reused across the whole run (and across
// runs), EXCEPT puts whose payload is bootstrap junk, which are written to a
// throwaway temp dir and discarded on exit. Reusable work stays cached; the
// per-run junk never lands in the cache.
//
// The JSON wire protocol and struct field names are dictated by the go command
// (see cmd/go/internal/cacheprog), so the JSON tags below intentionally use the
// PascalCase names the protocol requires.
package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	cmdGet   = "get"
	cmdPut   = "put"
	cmdClose = "close"

	// cacheSubdirLen is the length of the hex prefix the go build cache uses to
	// shard entries into subdirectories (e.g. "ab/abcd...-a").
	cacheSubdirLen = 2

	// outputFileMode is the permission for cache object files written by this
	// proxy (matching the go command's owner-writable default after umask).
	outputFileMode = 0o600

	// cacheDirMode is the permission for cache shard directories.
	cacheDirMode = 0o755

	// hashSize is the length in bytes of the action/output IDs (SHA-256). The
	// on-disk index format depends on this being exactly 32.
	hashSize = 32

	// dropEnv is the environment variable used as the default for -drop.
	dropEnv = "GOCACHEPROXY_DROP"

	// defaultDropMarkers lists the substrings that identify single-use generator
	// bootstrap artifacts. Puts whose payload contains any of them are dropped
	// instead of cached. Override with -drop or the GOCACHEPROXY_DROP env var.
	defaultDropMarkers = "easyjson-bootstrap,config-bootstrap"
)

var errNoCacheDir = errors.New("no cache dir (set -cache or GOCACHE)")

type request struct {
	ID       int64  `json:"ID"`
	Command  string `json:"Command"`
	ActionID []byte `json:"ActionID,omitempty"`
	OutputID []byte `json:"OutputID,omitempty"`
	BodySize int64  `json:"BodySize,omitempty"`
}

type response struct {
	ID            int64      `json:"ID"`
	Err           string     `json:"Err,omitempty"`
	KnownCommands []string   `json:"KnownCommands,omitempty"`
	Miss          bool       `json:"Miss,omitempty"`
	OutputID      []byte     `json:"OutputID,omitempty"`
	Size          int64      `json:"Size,omitempty"`
	Time          *time.Time `json:"Time,omitempty"`
	DiskPath      string     `json:"DiskPath,omitempty"`
}

type entry struct {
	outputID []byte
	size     int64
	modTime  time.Time
	diskPath string
}

type proxy struct {
	cacheDir    string
	tmpDir      string
	dropMarkers [][]byte
	dec         *json.Decoder
	enc         *json.Encoder
	out         *bufio.Writer
}

func main() {
	cacheDir := flag.String(
		"cache",
		os.Getenv("GOCACHE"),
		"underlying go build cache dir to read from",
	)

	drop := flag.String(
		"drop",
		dropDefault(),
		"comma-separated substrings; puts whose payload contains any of them are "+
			"dropped instead of cached (set to empty to cache everything)",
	)

	flag.Parse()

	err := run(*cacheDir, *drop)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gocacheproxy: %v\n", err)
		os.Exit(1)
	}
}

// dropDefault returns the default value for -drop: the GOCACHEPROXY_DROP env var
// when set, otherwise the built-in bootstrap markers.
func dropDefault() string {
	v := os.Getenv(dropEnv)
	if v != "" {
		return v
	}

	return defaultDropMarkers
}

// parseMarkers splits a comma-separated list into trimmed, non-empty byte
// substrings used to detect droppable payloads.
func parseMarkers(s string) [][]byte {
	parts := strings.Split(s, ",")
	markers := make([][]byte, 0, len(parts))

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			markers = append(markers, []byte(part))
		}
	}

	return markers
}

func run(cacheDir, drop string) error {
	if cacheDir == "" {
		return errNoCacheDir
	}

	tmpDir, err := os.MkdirTemp("", "gocacheproxy-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}

	defer func() { _ = os.RemoveAll(tmpDir) }()

	out := bufio.NewWriter(os.Stdout)
	p := &proxy{
		cacheDir:    cacheDir,
		tmpDir:      tmpDir,
		dropMarkers: parseMarkers(drop),
		dec:         json.NewDecoder(bufio.NewReader(os.Stdin)),
		enc:         json.NewEncoder(out),
		out:         out,
	}

	return p.serve()
}

func (p *proxy) serve() error {
	caps := &response{KnownCommands: []string{cmdGet, cmdPut, cmdClose}}

	err := p.write(caps)
	if err != nil {
		return err
	}

	for {
		var req request

		err = p.dec.Decode(&req)
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("decode request: %w", err)
		}

		resp, done := p.handle(&req)

		err = p.write(resp)
		if err != nil {
			return err
		}

		if done {
			return nil
		}
	}
}

func (p *proxy) handle(req *request) (*response, bool) {
	switch req.Command {
	case cmdGet:
		return p.handleGet(req), false
	case cmdPut:
		return p.handlePut(req), false
	case cmdClose:
		return &response{ID: req.ID}, true
	default:
		return &response{ID: req.ID, Err: "unknown command: " + req.Command}, false
	}
}

func (p *proxy) handleGet(req *request) *response {
	e, ok := p.lookup(req.ActionID)
	if !ok {
		return &response{ID: req.ID, Miss: true}
	}

	return &response{
		ID:       req.ID,
		OutputID: e.outputID,
		Size:     e.size,
		Time:     &e.modTime,
		DiskPath: e.diskPath,
	}
}

func (p *proxy) handlePut(req *request) *response {
	body, err := p.readBody(req.BodySize)
	if err != nil {
		return &response{ID: req.ID, Err: err.Error()}
	}

	if validID(req.ActionID) && validID(req.OutputID) && !p.shouldDrop(body) {
		path, werr := p.writeThrough(req.ActionID, req.OutputID, body)
		if werr != nil {
			return &response{ID: req.ID, Err: werr.Error()}
		}

		return &response{ID: req.ID, DiskPath: path}
	}

	return p.drop(req, body)
}

// drop writes a put payload to the throwaway temp dir instead of the real cache.
// It is used for bootstrap junk (unique every run, never reused) so it never
// pollutes the shared cache, while still giving the go command a DiskPath to use
// for the current build.
func (p *proxy) drop(req *request, body []byte) *response {
	path := filepath.Join(p.tmpDir, hex.EncodeToString(req.OutputID)+"-d")

	err := os.WriteFile(path, body, outputFileMode)
	if err != nil {
		return &response{ID: req.ID, Err: err.Error()}
	}

	return &response{ID: req.ID, DiskPath: path}
}

// writeThrough stores an output and its index entry in the real cache using the
// same on-disk layout as the go command, then returns the path to the object so
// the entry can be found by subsequent get requests (from this or any other
// proxy instance sharing the cache).
func (p *proxy) writeThrough(actionID, outputID, body []byte) (string, error) {
	outHex := hex.EncodeToString(outputID)

	outPath := filepath.Join(p.cacheDir, outHex[:cacheSubdirLen], outHex+"-d")

	err := writeObject(outPath, body)
	if err != nil {
		return "", err
	}

	actHex := hex.EncodeToString(actionID)

	actPath := filepath.Join(p.cacheDir, actHex[:cacheSubdirLen], actHex+"-a")

	line := fmt.Sprintf(
		"v1 %x %x %20d %20d\n",
		actionID, outputID, len(body), time.Now().UnixNano(),
	)

	err = writeFileAtomic(actPath, []byte(line))
	if err != nil {
		return "", err
	}

	return outPath, nil
}

func (p *proxy) readBody(size int64) ([]byte, error) {
	if size <= 0 {
		return nil, nil
	}

	var body []byte

	err := p.dec.Decode(&body)
	if err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}

	return body, nil
}

func (p *proxy) lookup(actionID []byte) (*entry, bool) {
	idHex := hex.EncodeToString(actionID)
	if len(idHex) < cacheSubdirLen {
		return nil, false
	}

	data, err := os.ReadFile(filepath.Join(p.cacheDir, idHex[:cacheSubdirLen], idHex+"-a"))
	if err != nil {
		return nil, false
	}

	fields := strings.Fields(string(data))
	if len(fields) < 5 || fields[0] != "v1" {
		return nil, false
	}

	outID, err := hex.DecodeString(fields[2])
	if err != nil {
		return nil, false
	}

	diskPath := p.outputPath(outID)

	info, err := os.Stat(diskPath)
	if err != nil {
		return nil, false
	}

	nano, err := strconv.ParseInt(fields[4], 10, 64)
	if err != nil {
		return nil, false
	}

	return &entry{
		outputID: outID,
		size:     info.Size(),
		modTime:  time.Unix(0, nano),
		diskPath: diskPath,
	}, true
}

func (p *proxy) outputPath(outID []byte) string {
	outHex := hex.EncodeToString(outID)

	return filepath.Join(p.cacheDir, outHex[:cacheSubdirLen], outHex+"-d")
}

func (p *proxy) write(resp *response) error {
	err := p.enc.Encode(resp)
	if err != nil {
		return fmt.Errorf("encode response: %w", err)
	}

	err = p.out.Flush()
	if err != nil {
		return fmt.Errorf("flush response: %w", err)
	}

	return nil
}

// validID reports whether id has the exact length the on-disk index format
// requires. Malformed ids are never written through, to avoid corrupting the
// cache index.
func validID(id []byte) bool {
	return len(id) == hashSize
}

// shouldDrop reports whether a put payload matches any configured drop marker.
// Matching payloads are generator bootstrap artifacts that are unique on every
// run (their cache key is never hit again), so writing them through would only
// bloat the cache.
func (p *proxy) shouldDrop(body []byte) bool {
	for _, m := range p.dropMarkers {
		if bytes.Contains(body, m) {
			return true
		}
	}

	return false
}

// writeObject writes a cache object, skipping the write when an identical one is
// already present (cache objects are content-addressed, so equal size means
// equal content).
func writeObject(path string, body []byte) error {
	info, err := os.Stat(path)
	if err == nil && info.Size() == int64(len(body)) {
		return nil
	}

	return writeFileAtomic(path, body)
}

// writeFileAtomic writes data to path via a temp file and rename, so concurrent
// proxy instances sharing the cache never observe a partially written file.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)

	err := os.MkdirAll(dir, cacheDirMode)
	if err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	f, err := os.CreateTemp(dir, ".gocacheproxy-*")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}

	tmp := f.Name()

	err = writeAndClose(f, data)
	if err != nil {
		_ = os.Remove(tmp)

		return err
	}

	err = os.Chmod(tmp, outputFileMode)
	if err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("chmod %s: %w", tmp, err)
	}

	err = os.Rename(tmp, path)
	if err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("rename %s: %w", tmp, err)
	}

	return nil
}

func writeAndClose(f *os.File, data []byte) error {
	_, err := f.Write(data)
	if err != nil {
		_ = f.Close()

		return fmt.Errorf("write %s: %w", f.Name(), err)
	}

	err = f.Close()
	if err != nil {
		return fmt.Errorf("close %s: %w", f.Name(), err)
	}

	return nil
}
