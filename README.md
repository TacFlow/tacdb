# tacdb

An embeddable graph database in Go, with full-text search, optional vector search, transactions and durable streams, integrated with the TAC language.

**Status:** under construction. The current milestone is M4 (query language): the storage engine, the graph API and queries that read and write work; the planner by cost and the plan cache do not exist yet. Nothing here is ready for production use.

## Documents

- [FORMAT.md](FORMAT.md): the G2 physical format, as implemented.
- [COMPATIBILITY.md](COMPATIBILITY.md): the compatibility matrix and tacdb's own decisions.
- [AUDIT-REGRESSIONS.md](AUDIT-REGRESSIONS.md): traceability of the AUD, REV and R13 items.
- [SECURITY.md](SECURITY.md): how the security model is implemented, per platform.
- [OPERATIONS.md](OPERATIONS.md): running tacdb in production: memory and durability on macOS.

Section references such as §7.3 point to the tacdb specification, version 1.3.

## Development

Requires Go 1.27. `go.mod` pins the 1.27.3 toolchain; with `GOTOOLCHAIN=auto` (the default) Go downloads it on first use.

```sh
go test ./...
CGO_ENABLED=1 go test -race ./...
go test ./internal/codec -fuzz=FuzzValue -fuzztime=60s
```

Production code builds with `CGO_ENABLED=0`.
