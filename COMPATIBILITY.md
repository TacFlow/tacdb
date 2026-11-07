# Compatibility matrix

Policy (§25.1 and §28 of the specification): tacdb aims at functional and semantic parity with the reference engine (R0) in the inventoried capabilities. Every difference has an ID, a reason and a test in this matrix; no divergence disappears without a record.

## 1. Areas

| Area | Policy (§28) | Status |
|---|---|---|
| Property/graph model | Parity, including absence/Null, lists/maps and edge IDs. | Values and codec ready (M1a); nodes, edges, labels, properties, adjacency and degrees ready (M3b); property indexes with uniqueness, truncated and hashed keys ready (M3c); online BuildIndex ready (M3d). |
| Cypher dialect of the pinned revision | Parity on the inventoried features. | Parser of `tacdb-cypher/1` ready (M4a): every clause and the precedence table of the reference, with the divergences D-009 and D-010; semantics and execution pending (M4b to M4f). |
| FTS/BM25/fuzzy | Functional parity; deterministic scores across platforms. | Pending (M5). |
| HNSW | Same capabilities and metrics; topology is not a binary identity. | Pending (M8). |
| Transactions | One mutating writer; parallel readers; adaptive group commit. | Ready (M2): FIFO writer queue, snapshots, O(1) savepoints, GroupSync, Sync and Async; public API of handles and transactions (M3b). |
| Physical maintenance | Passive checkpoint, segmented WAL, bitmap allocator, scrub, compact. | Page format and superblock (M1c), allocator (M1e), segmented WAL, passive checkpoint and scrub (M2) ready; compact pending (M9). |
| Cluster/multiprocess | Mandatory in 1.0. | Pending (M10 to M12). |
| Streams | Sequences, offsets, trim and atomicity preserved. | Pending (M6). |
| Security | The §20 model, with no counterpart in R0. | File permissions, paths and locks ready (M1b, see SECURITY.md); the rest pending (M10). |
| R0 main file/WAL | Physically incompatible; migration through LTX1. | By decision (ADR-07). |
| Original Go API | Conformance adapter; new main API. | Pending. |
| C ABI / Python / Java | Outside the initial distribution. | By decision. |
| TypeScript | New SDK over a Go bridge. | Pending (M10, M13). |
| Neo4j-inspired improvements | tacdb's own Go implementation of the §33 contracts, clean room; no binary compatibility, Bolt or Enterprise extension. | Pending (M4 onwards). |
| Memory/serialize | Shared VFS; Serialize produces a consistent G2 snapshot without quiescence. | MemoryVFS ready (M1b); Serialize pending (M9). |
| Embedding helpers/services | Excluded from the runtime; external vectors are optionally accepted. | By decision (ADR-03). |
| TAC | Language 0.6 and IR 1.3 for the database; new constructs are not skill aliases. | Pending (M7). |
| LAYA/SLM constructs and implicit legacy memory | Absent from the profile; an error or an explicit source migration. | Pending (M7). |
| Performance | No promise of automatic improvement; compare and record; write amplification has a gate. | Concurrency and isolated-commit benchmarks and the REC-11 gate run in CI (M2); the rest pending. |
| G2 limits | IDs and sequences up to MaxInt64; fixed limits require a new `format_revision`. | Value limits (M1a) and page limits (M1c) ready. |

## 2. tacdb's own decisions

| ID | Decision | Reason | Test |
|---|---|---|---|
| D-001 | Total order of values: Bool < number < String < Bytes < Vector < List < Map < Null. Int and Float compare by exact numeric value, without converting int64 to float64; NaN comes after every number and ties with any NaN; `-0` ties with `+0`. `Compare == 0` does not imply equal bits (`Int(1)` and `Float(1)` tie). | §5.3 fixes the families and asks for tacdb's own cases where R0 does not fix the behavior. | `value/compare_test.go` |
| D-002 | Canonical codec: the decoder rejects map keys out of order or repeated, and non-minimal uvarints, as `CORRUPT`. | Every value has a single form (§5.3 and §7.1 of specification 1.3); goldens and signatures depend on it. | `internal/codec/value_test.go`, `internal/codec/uvarint_test.go`, `FuzzValue` |
| D-003 | The REC-11 gate (an open after a crash over a database of 100 GiB) runs on Linux and macOS only. | It needs a sparse main file; on Windows that takes an explicit attribute, and the property it measures, a recovery that never reads the main file beyond its superblock, does not depend on the platform. | `internal/engine/gate_test.go` (`TestREC11`) |
| D-004 | The graph methods of `Tx` take no context, as §6 declares them; they are bounded by `MaxWriteTxDuration` and `MaxReadTxAge`. `Commit` of a read transaction closes it and returns its snapshot with a zero token. Inside `View` and `Update`, `Commit` and `Rollback` fail with `INVALID_ARGUMENT`: the function owns the work, not the transaction; a call `fn` left running ends before the transaction does. `Close` with transactions open returns `BUSY`; a transaction that begins while `Close` checks fails with `SHUTTING_DOWN`. | §6 leaves these cases open. | `db_test.go` (`TestUpdate`, `TestTransactions`) |
| D-005 | `EdgeCursor` reads the adjacency in batches of 64 entries, each one operation of its transaction that resumes after the last key it gave, and checks its transaction before each edge: after `Commit`, `Rollback` or an expired snapshot it fails at once (`TX_CLOSED`, `SNAPSHOT_EXPIRED`). Inside a write transaction it yields the edges that existed when it was opened and still exist when it reaches them, so writing during the walk never feeds it. Edges come grouped by direction (outgoing, then incoming) and type, in peer and edge order. | Without a live cursor in the engine, a resumable cursor keeps revocation cooperative (§10.1); the rule for writes does not depend on the batch size. | `internal/graph/graph_test.go` (`TestCursorBatches`, `TestCursorExpires`), `cursor_test.go` |
| D-006 | The canonical dump writes one line per node, then per edge, in ID order: labels in insertion order, properties by name, values typed and exact (`1` is Int, `float(1)` Float; `-0`, infinities and NaN bits kept). | The dump of §25.1 compares content and keeps the bits of properties. | `internal/graph/graph_test.go` (`TestDumpValues`) |
| D-007 | A seek on a property index applies expression semantics to the full value: equality finds Int 1 and Float 1.0 together, and never matches Null or NaN; a range only matches values of the family of its bounds (numbers are one family); `STARTS WITH` only matches strings. Hits come by value, then by entity ID. Indexes hold an entry for a stored Null, never for an absent property. | §12.2 fixes the keys and uniqueness, §15.2 the semantics of comparisons; ties need a stable order. | `internal/index/index_test.go` (`TestSeekAgainstScan`) |
| D-008 | `BuildIndex` waits for the end of the build or of its context, and the build goes on after the context ends; a close of the handle returns `CLOSED` and a drop `INDEX_NOT_FOUND`. `IndexBuildStatus.Conflicts` adds the first pairs of entities with equal values to the status of §12.2. A batch takes at most half of `MaxTxBytes`. `DropIndex` purges entries after its commit, in batches, for every index; `CreateIndex` and `BuildIndex` of an identical declaration return at once whatever its state, so a FAILED index must be dropped before it is built again. | §12.2 leaves the waiting, the shape of the conflicting keys and the purge open. | `build_test.go` |
| D-009 | Lexical divergences from the reference: one statement per query, with an optional final `;` (the reference reads `;` as whitespace and merges statements); names between backticks, with a doubled backtick for a backtick, so that `QuoteIdentifier` output parses; keywords allowed as labels, types, property keys and map keys; an invalid `\u` escape, a float literal out of range, a number glued to letters, a bare `$`, a float literal that rounds to zero and a NUL inside a quoted name are `QUERY_LEX` errors (the reference keeps text, gives +Inf or 0.0, or splits the token); `-9223372036854775808` is an Int literal. Errors carry their stage and line and column, the column in code points. | SPEC §15.1, §20.7; nothing accepted in part. | `internal/query/parser/parser_test.go` (`TestLiterals`, `TestErrors`), `query/query_test.go` |
| D-010 | Clause structure follows Cypher instead of the reference's free order: reading clauses, then updates, in parts separated by WITH; WHERE only after MATCH, OPTIONAL MATCH, WITH or CALL; ORDER BY, SKIP and LIMIT only inside RETURN or WITH, in that order; RETURN last; a query ends in RETURN or an update, or is a single CALL; `RETURN 1` alone is valid and an empty query is an error. Constructions outside the dialect (UNION, CASE, list indexing and comprehensions, `RETURN *`, `=~`, path variables, pattern predicates, map projection, label predicates in expressions, subqueries, schema commands) are `QUERY_UNSUPPORTED`, naming the construction. Relationship type alternation `:A\|B` is accepted in patterns; a variable-length relationship takes neither a variable nor properties. Expressions keep the reference's precedence table, including prefix NOT as any operand (`a = NOT b`) and operators after `IS NULL` (`a IS NULL + b`). | SPEC §15.1. | `internal/query/parser/parser_test.go` (`TestClauses`, `TestUnsupported`) |

## 3. Recorded divergences from R0

None so far.
