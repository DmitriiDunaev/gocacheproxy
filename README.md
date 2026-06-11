# gocacheproxy

A tiny [`GOCACHEPROG`](https://pkg.go.dev/cmd/go/internal/cacheprog) helper that
makes the Go build cache **read-through but write-blocked**: it serves cache
reads from an existing on-disk cache, and silently drops every write so the real
cache never grows.

## Why

Some code generators run a throwaway *bootstrap* program via `go run` as part of
`go generate` — for example [easyjson](https://github.com/mailru/easyjson). Each
bootstrap is written to a file with a random name, so its compiled output gets a
brand-new cache key on **every** run and is
never reused. Over many `go generate` runs these single-use entries pile up and
bloat `GOCACHE`, even though nothing in your code changed.

`gocacheproxy` lets you keep the speed benefit of the shared cache during
generation (dependencies are read from it and reused) while preventing the
generators from polluting it.

## How it works

It implements the `GOCACHEPROG` JSON protocol over stdin/stdout:

- **`get` — read-through.** Looks the action up in the underlying on-disk cache
  and returns the existing object's `DiskPath`. Dependencies stay cached, so
  compilation stays fast.
- **`put` — write-blocked.** The produced object is written to a per-process
  temporary directory (the protocol requires a real `DiskPath` because the
  current build links against it) and discarded when the proxy exits. Nothing is
  ever written into the real cache.
- **`close`.** Removes the temporary directory and exits.

Because each invocation uses its own `os.MkdirTemp` directory and only reads
(never writes) shared cache files, it is safe to run multiple builds in parallel.

## Install

```sh
go install github.com/dmitriidunaev/gocacheproxy@latest
```

## Usage

Set `GOCACHEPROG` only for the command whose writes you want to discard:

```sh
GOCACHEPROG="gocacheproxy -cache=$(go env GOCACHE)" go generate ./...
```

If `-cache` is omitted, the proxy falls back to the `GOCACHE` environment
variable.

## License

MIT
