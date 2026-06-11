# gocacheproxy

A tiny [`GOCACHEPROG`](https://pkg.go.dev/cmd/go/internal/cacheprog) helper for
`go generate` that keeps the Go build cache **fast** while stopping generators
from **polluting** it: it behaves as a normal write-through cache, except it
drops the single-use *bootstrap* artifacts that generators produce on every run.

## Why

Some code generators build a throwaway *bootstrap* program as part of
`go generate` — for example [easyjson](https://github.com/mailru/easyjson) and
[go-4devs/config](https://gitoa.ru/go-4devs/config). Each bootstrap is written
to a file with a unique temporary name, so its compiled object and linked binary
get a brand-new cache key on **every** run and are never hit again. Over many
runs these single-use entries pile up and bloat `GOCACHE` (easily 1000+ entries
and ~1 GB per `go generate ./...`), even though nothing in your code changed.

A naive "block all writes" proxy fixes the bloat but is catastrophically slow:
during `go generate ./...` hundreds of generator processes can no longer reuse
each other's compiled dependencies, so everything is recompiled from scratch
over and over (observed: ~3 min → ~27 min).

`gocacheproxy` keeps full caching speed and only discards the junk.

## How it works

It implements the `GOCACHEPROG` JSON protocol over stdin/stdout:

- **`get` — read-through.** Looks the action up in the underlying on-disk cache
  and returns the existing object's `DiskPath`.
- **`put` — write-through, except bootstrap junk.** Reusable outputs are written
  into the real cache using the exact same on-disk layout as the go command (an
  `-a` index entry plus the `-d` object), so they are reused for the rest of the
  run and on later runs. Outputs whose payload is a generator bootstrap artifact
  (detected by the embedded `easyjson-bootstrap` / `config-bootstrap` temp path)
  are written to a per-process temporary directory and discarded on exit, so they
  never enter the real cache.
- **`close`.** Removes the temporary directory and exits.

Writes are atomic (temp file + rename) and content-addressed, so it is safe to
run multiple builds against the same cache in parallel.

## Install

```sh
go install github.com/dmitriidunaev/gocacheproxy@latest
```

## Usage

Set `GOCACHEPROG` only for the command whose bootstrap junk you want to discard:

```sh
GOCACHEPROG="gocacheproxy -cache=$(go env GOCACHE)" go generate ./...
```

If `-cache` is omitted, the proxy falls back to the `GOCACHE` environment
variable.

## License

MIT
