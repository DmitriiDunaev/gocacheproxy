// Command gocacheproxy is a GOCACHEPROG helper that serves cache reads from an
// existing on-disk Go build cache but never writes anything back into it.
//
// It is meant to be used only for code generation (go generate), where the
// bootstrap programs of generators such as easyjson keep polluting the shared
// GOCACHE with single-use entries. With this proxy, "get" requests are answered
// from the real cache (so dependencies are reused and compilation stays fast),
// while "put" requests are written to a throwaway temporary directory and
// discarded on exit, so the real cache never grows.
//
// The JSON wire protocol and struct field names are dictated by the go command
// (see cmd/go/internal/cacheprog), so the JSON tags below intentionally use the
// PascalCase names the protocol requires.
package main

import (
	"bufio"
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

	// outputFileMode is the permission for the throwaway "put" object files.
	outputFileMode = 0o600
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
	cacheDir string
	tmpDir   string
	dec      *json.Decoder
	enc      *json.Encoder
	out      *bufio.Writer
}

func main() {
	cacheDir := flag.String(
		"cache",
		os.Getenv("GOCACHE"),
		"underlying go build cache dir to read from",
	)

	flag.Parse()

	err := run(*cacheDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gocacheproxy: %v\n", err)
		os.Exit(1)
	}
}

func run(cacheDir string) error {
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
		cacheDir: cacheDir,
		tmpDir:   tmpDir,
		dec:      json.NewDecoder(bufio.NewReader(os.Stdin)),
		enc:      json.NewEncoder(out),
		out:      out,
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

	path := filepath.Join(p.tmpDir, hex.EncodeToString(req.OutputID)+"-d")

	err = os.WriteFile(path, body, outputFileMode)
	if err != nil {
		return &response{ID: req.ID, Err: err.Error()}
	}

	return &response{ID: req.ID, DiskPath: path}
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
