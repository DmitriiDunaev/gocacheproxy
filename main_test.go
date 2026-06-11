package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCacheEntry(t *testing.T, dir string, actID, outID, body []byte) {
	t.Helper()

	outHex := hex.EncodeToString(outID)

	err := os.MkdirAll(filepath.Join(dir, outHex[:2]), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, outHex[:2], outHex+"-d"), body, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	actHex := hex.EncodeToString(actID)

	err = os.MkdirAll(filepath.Join(dir, actHex[:2]), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	line := fmt.Sprintf("v1 %s %s %20d %20d\n", actHex, outHex, len(body), time.Now().UnixNano())

	err = os.WriteFile(filepath.Join(dir, actHex[:2], actHex+"-a"), []byte(line), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func buildInput(t *testing.T, reqs []any) []byte {
	t.Helper()

	var in bytes.Buffer

	enc := json.NewEncoder(&in)

	for _, r := range reqs {
		err := enc.Encode(r)
		if err != nil {
			t.Fatal(err)
		}
	}

	return in.Bytes()
}

func decodeResponses(t *testing.T, data []byte, n int) []response {
	t.Helper()

	dec := json.NewDecoder(bytes.NewReader(data))
	out := make([]response, 0, n)

	for range n {
		var r response

		err := dec.Decode(&r)
		if err != nil {
			t.Fatalf("decode response: %v", err)
		}

		out = append(out, r)
	}

	return out
}

func TestProxy(t *testing.T) {
	t.Parallel()

	cacheDir := t.TempDir()
	tmpDir := t.TempDir()

	actID := bytes.Repeat([]byte{0xAA}, 32)
	outID := bytes.Repeat([]byte{0xBB}, 32)
	body := []byte("cached-object-bytes")
	writeCacheEntry(t, cacheDir, actID, outID, body)

	wtAct := bytes.Repeat([]byte{0xCC}, 32)
	wtOut := bytes.Repeat([]byte{0xDD}, 32)
	wtBody := []byte("freshly-produced-output")

	bootAct := bytes.Repeat([]byte{0xEE}, 32)
	bootOut := bytes.Repeat([]byte{0xFF}, 32)
	bootBody := []byte("ar header /tmp/easyjson-bootstrap123456/main.go more bytes")

	input := buildInput(t, []any{
		request{ID: 1, Command: cmdGet, ActionID: actID},
		request{ID: 2, Command: cmdGet, ActionID: bytes.Repeat([]byte{0x11}, 32)},
		request{ID: 3, Command: cmdPut, ActionID: wtAct, OutputID: wtOut, BodySize: int64(len(wtBody))},
		wtBody,
		request{ID: 4, Command: cmdGet, ActionID: wtAct},
		request{ID: 5, Command: cmdPut, ActionID: bootAct, OutputID: bootOut, BodySize: int64(len(bootBody))},
		bootBody,
		request{ID: 6, Command: cmdClose},
	})

	resps := runProxy(t, cacheDir, tmpDir, input, 7)

	assertCaps(t, resps[0])
	assertGetHit(t, resps[1], outID, body, cacheDir)
	assertGetMiss(t, resps[2])
	assertWriteThrough(t, resps[3], wtOut, wtBody, cacheDir)
	assertGetHit(t, resps[4], wtOut, wtBody, cacheDir)
	assertDrop(t, resps[5], tmpDir, bootOut, bootBody)
}

func TestParseMarkers(t *testing.T) {
	t.Parallel()

	got := parseMarkers(" easyjson-bootstrap , ,config-bootstrap,")
	if len(got) != 2 {
		t.Fatalf("parseMarkers: got %d markers, want 2: %q", len(got), got)
	}

	if string(got[0]) != "easyjson-bootstrap" || string(got[1]) != "config-bootstrap" {
		t.Fatalf("parseMarkers: got %q", got)
	}

	if len(parseMarkers("")) != 0 {
		t.Fatal("parseMarkers: empty input should yield no markers")
	}
}

func runProxy(t *testing.T, cacheDir, tmpDir string, input []byte, n int) []response {
	t.Helper()

	var outBuf bytes.Buffer

	bw := bufio.NewWriter(&outBuf)
	p := &proxy{
		cacheDir:    cacheDir,
		tmpDir:      tmpDir,
		dropMarkers: parseMarkers(defaultDropMarkers),
		dec:         json.NewDecoder(bytes.NewReader(input)),
		enc:         json.NewEncoder(bw),
		out:         bw,
	}

	err := p.serve()
	if err != nil {
		t.Fatalf("serve: %v", err)
	}

	return decodeResponses(t, outBuf.Bytes(), n)
}

func assertCaps(t *testing.T, r response) {
	t.Helper()

	if len(r.KnownCommands) != 3 {
		t.Fatalf("capabilities: got %v", r.KnownCommands)
	}
}

func assertGetHit(t *testing.T, r response, outID, body []byte, cacheDir string) {
	t.Helper()

	if r.Miss {
		t.Fatal("get hit: unexpected miss")
	}

	if !bytes.Equal(r.OutputID, outID) {
		t.Fatalf("get hit: outputID %x", r.OutputID)
	}

	if r.Size != int64(len(body)) {
		t.Fatalf("get hit: size %d", r.Size)
	}

	outHex := hex.EncodeToString(outID)
	want := filepath.Join(cacheDir, outHex[:2], outHex+"-d")

	if r.DiskPath != want {
		t.Fatalf("get hit: diskPath %q want %q", r.DiskPath, want)
	}
}

func assertGetMiss(t *testing.T, r response) {
	t.Helper()

	if !r.Miss {
		t.Fatal("get miss: expected miss")
	}
}

func assertWriteThrough(t *testing.T, r response, outID, body []byte, cacheDir string) {
	t.Helper()

	outHex := hex.EncodeToString(outID)
	want := filepath.Join(cacheDir, outHex[:2], outHex+"-d")

	if r.DiskPath != want {
		t.Fatalf("write-through: diskPath %q want %q", r.DiskPath, want)
	}

	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("write-through: read object: %v", err)
	}

	if !bytes.Equal(got, body) {
		t.Fatalf("write-through: body %q want %q", got, body)
	}
}

func assertDrop(t *testing.T, r response, tmpDir string, outID, body []byte) {
	t.Helper()

	want := filepath.Join(tmpDir, hex.EncodeToString(outID)+"-d")
	if r.DiskPath != want {
		t.Fatalf("drop: diskPath %q want %q", r.DiskPath, want)
	}

	got, err := os.ReadFile(r.DiskPath)
	if err != nil {
		t.Fatalf("drop: read body: %v", err)
	}

	if !bytes.Equal(got, body) {
		t.Fatalf("drop: body %q want %q", got, body)
	}
}
