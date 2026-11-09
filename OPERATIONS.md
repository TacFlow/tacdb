# Operations

This document explains how to run tacdb in production: memory, files, transactions, checkpoints and, in later milestones, backups and the cluster. It grows with each milestone.

## 1. Memory

### 1.1 The process governor

Every database handle in a process debits its memory from one governor (`internal/govern`, §8.5 of the specification). The default governor allows 1 GiB in total and splits it into categories:

| Category | Used for | Guaranteed floor (default) |
|---|---|---|
| cache | Page cache of every handle | 16 pages per handle, reserved when the handle opens |
| execution | Query operators, materialization, value decoding | 64 MiB |
| overlay | Private page copies of write transactions | none |
| sealed | Sealed commits waiting for durability | 128 MiB (`MaxPendingCommitBytes`) |
| transport | IPC and RPC buffers | 32 MiB |
| spool | Reassembly spool | none |

A category can always use its floor. Above it, every category shares what is left of the limit (here 1 GiB - 224 MiB). When a reservation does not fit, it waits for memory up to `GovernorWait` (1 s by default) and then fails with `RESOURCE_LIMIT`, whose details give `requested`, `available` and `limit`; a canceled or expired context ends the wait with `CANCELED` or `DEADLINE`. Sealed commits never wait for queries: a sealed reservation that does not fit fails at once.

In milestone M1 only the cache and value decoding debit the governor; the other categories are wired in by the milestones that create those resources (M2 for overlays and sealed commits, M4 for the execution of queries, M10 for transport).

### 1.2 GOMEMLIMIT

tacdb never changes `GOGC` or `GOMEMLIMIT` of the host process. Set the governor limit below `GOMEMLIMIT`, leaving room for the rest of the host application and for the garbage collector's overhead. A starting point is a governor limit of at most half of `GOMEMLIMIT` when the host does other work, and at most 70% when tacdb is most of the process; measure the resident set under load and adjust.

### 1.3 Page cache

Each handle has a page cache of 64 MiB by default, debited from the governor's cache category. When the handle opens, its cache reserves a quota of 16 pages that other handles cannot take; if the governor cannot give it within `GovernorWait`, opening the handle fails with `RESOURCE_LIMIT`. Above the quota, the cache grows into the governor's shared memory, up to its own budget. That memory is lent, not owned: before another reservation waits for shared memory, the governor asks every cache to give back unpinned pages above its quota.

The cache keeps verified page images and evicts with a CLOCK (second chance) in shards. It never waits for memory: when its budget or the governor has no room, a new page takes the place of an unpinned page of the same cache, and when every cached page is pinned, the new page fails with `RESOURCE_LIMIT`. One execution can pin at most 8 MiB of pages (`MaxPinnedBytesPerExecution`). Concurrent requests for the same page share one read.

## 2. Files

### 2.1 Durability on macOS

tacdb flushes files and directories with `F_FULLFSYNC`, because plain `fsync` does not make macOS write its caches to the medium. Keep databases on APFS or HFS+, which implement it. When `F_FULLFSYNC` fails on a file or a directory, as on network file systems that do not implement it, tacdb falls back to plain `fsync` and counts the fallback. On such file systems recent writes and renames may not survive a power loss. The counter is internal for now; it becomes visible with the metrics of a later milestone.

## 3. Transactions and durability

### 3.1 Durability modes

The mode is chosen per handle and applies to every commit.

| Mode | A commit returns | After a power loss |
|---|---|---|
| `GroupSync` (default) | once its group is synced, with `Durable=true` | every returned commit survives |
| `Sync` | once it is synced as its own group, with `Durable=true` | every returned commit survives |
| `Async` | once it is appended and published, with `Durable=false` | a suffix of returned commits may be lost; what survives is always a prefix |

`GroupSync` is adaptive: a commit alone pays one Sync and waits for no window, and commits that seal while a Sync runs share the next one (up to 64 commits or 8 MiB per group). `Async` syncs every 100 ms, after 16 MiB of appends, on `Flush`, or as soon as a commit waits for room in the commit queue. `Flush` waits until every commit sealed before the call is durable. Readers that must only see durable data use `BeginReadDurable`.

### 3.2 Admission and limits

One writer holds the writer slot at a time. `BeginWrite` waits in a FIFO queue of 128 candidates for at most `BusyTimeout` (5 s); `TryBeginWrite` never waits. The errors are distinct: `DB_BUSY` (the slot is taken and the caller would not wait), `WRITER_QUEUE_FULL`, `BUSY_TIMEOUT`, `CANCELED` and `DEADLINE`. At most `MaxActiveTransactions` (1024) read and write transactions are open; one more gets `TOO_MANY_TRANSACTIONS`. A write transaction older than `MaxWriteTxDuration` (30 s) is rolled back and its next call gets `WRITE_TX_TIMEOUT`; commits it already sealed are not affected. A transaction holds at most `MaxTxBytes` (64 MiB) of private pages; the statement that passes it fails alone. A group must fit one log segment, so `GroupMaxBytes` may not exceed 128 MiB nor `MaxTxBytes` 64 MiB, and `AsyncSyncBytes` may not exceed `MaxPendingCommitBytes`.

Commits sealed and not yet durable are bounded by `MaxPendingCommits` (256) and `MaxPendingCommitBytes` (128 MiB). `Commit` waits for room under the caller's deadline; `TryCommit` gets `COMMIT_QUEUE_FULL` at once. A commit that would take the log past `MaxWALBytes` (1 GiB) first runs the snapshot pressure policy and a checkpoint (§4.2), then gets `WAL_LIMIT` if the log is still too large; `TryCommit` gets it at once. In all these cases the transaction is not sealed and stays usable.

### 3.3 Pending commits and failures

A commit is sealed once its writer releases the slot; from then on it cannot be rolled back, because the next writer builds on it. Canceling the wait of a sealed commit returns `COMMIT_PENDING` with the commit's token; the commit still completes. `CommitStatus(token)` says `Pending`, `Volatile` (appended in `Async`), `Durable` or `Unknown`. Tokens of commits recovered from the log are known after a reopen. A checkpoint forgets the tokens of the commits it copied, and `Close` ends with a checkpoint; a token never seen, forgotten or lost in a failure is `Unknown`, and the application then checks its own idempotency key. Never repeat a mutation automatically.

When an append or a Sync of the log fails, the handle becomes `RECOVERY_REQUIRED`: pending commits get `COMMIT_UNKNOWN`, a writer that built on them cannot commit, and no transaction begins. The failed Sync is never retried, since the operating system's cache is then in an unknown state. Close the handle and open the database again: recovery decides which commits survived, and a `COMMIT_UNKNOWN` commit may turn out to be complete.

### 3.4 Read-only opening

A read-only open never changes a file. It recovers the log in memory and tries to make what it read durable with a Sync of a read-only handle. Where the platform refuses that, the recovered commits past the last checkpoint are served but not reported durable, and `BeginReadDurable` gives the checkpointed state.

## 4. Checkpoints and the log

### 4.1 The passive checkpoint

A maintenance goroutine per handle copies durable commits from the log to the main file and removes the log segments it no longer needs. It runs after every 64 MiB of log (`CheckpointBytes`), when more than four segments lie above the floor (`CheckpointSegments`), when a commit reaches `MaxWALBytes`, and on `Checkpoint(ctx)`. `Close` runs a last checkpoint, so a database closed cleanly keeps one segment. The checkpoint never blocks `BeginRead`, `BeginWrite`, readers, commits or the log's Syncs: it writes up to the oldest snapshot, and the snapshots newer than that keep reading their versions from the log.

With `ScrubFreedPages` (on by default), the checkpoint also overwrites freed pages with zeroed free pages, so deleted data does not linger in the main file.

A failure to write or sync the main file during a checkpoint makes the handle `RECOVERY_REQUIRED`, like a failure of the log (§3.3): the Sync is never retried. The log still holds everything, and the next open recovers from it.

### 4.2 Old snapshots

A snapshot keeps every log segment written after it. Two limits stop a forgotten one from pinning the log:

| Option | Default | Effect |
|---|---|---|
| `MaxReadTxAge` | 10 min | A read transaction older than this is revoked. `Unlimited` disables it. |
| `SnapshotPressurePolicy` | `ExpireOldest` | What happens when snapshots pin `WALPinnedSoftBytes` of log. |
| `WALPinnedSoftBytes` | 512 MiB | The log pinned by snapshots that starts the policy. |
| `MinSnapshotAgeForEviction` | 30 s | `ExpireOldest` never revokes a younger snapshot. |

Revocation is cooperative: an operation already running finishes and delivers all its results; the next one gets `SNAPSHOT_EXPIRED`. Close the transaction and begin a new one. `ExpireOldest` revokes from the oldest snapshot on, and only when that frees segments: it never revokes a snapshot that shares its floor with one too young to revoke. `RejectWrites` keeps every reader; commits then fail with `WAL_LIMIT` once the log reaches `MaxWALBytes`, and the error names how many snapshots keep the log and the age and origin of the oldest. Write transactions are never revoked; `MaxWriteTxDuration` bounds them.

Each revocation and each refused commit calls `OnEvent` with the kind of event, the number of snapshots, the age and origin of the oldest and the log they pin. The callback runs on the goroutine that found the event (maintenance, a commit or a reader), with no lock of the handle held, and must not block. `Stats` counts checkpoints, copied and scrubbed pages, removed segments, revoked snapshots and `WAL_LIMIT` refusals.

## 5. Indexes

### 5.1 Creating an index

`Tx.CreateIndex` declares an index and fills it inside the write transaction, which publishes both at its commit. It suits a scope that fits `MaxTxBytes`; beyond that it fails with `LIMIT_EXCEEDED`, and the index must be built online.

### 5.2 Building an index online

`DB.BuildIndex` builds an index while the application keeps writing (§12.2 of the specification):

1. A first commit declares the index BUILDING. From then on every writer keeps it, so the index never misses a change.
2. Batches fill it by ascending entity ID. Each batch is a short write transaction in the same FIFO queue as the application, of at most `IndexBuildBatchBytes` (4 MiB by default, never more than half of `MaxTxBytes`); between batches the build waits while application writers wait for the writer slot.
3. A last commit compares the entry counter with the entries the batches and the writers counted, and publishes the index ONLINE, or FAILED.

Only ONLINE indexes answer queries. A unique index whose data, or whose writers during the build, hold equal values ends FAILED with `UNIQUE_VIOLATION` and the first pairs of entities in `IndexBuildStatus.Conflicts`; a value the index refuses, such as NaN in an ordered index, fails it with that error. A FAILED index keeps its name until `DropIndex`.

`BuildIndex` returns when the build ends, when its context does (the build goes on in the background and `IndexStatus` follows it), when the handle closes (`CLOSED`), or when the index is dropped (`INDEX_NOT_FOUND`). While transactions fill `MaxActiveTransactions`, background work waits and tries again. Calling `BuildIndex` again for the same declaration waits for the same build. The build keeps its progress in the database: after `Close` or a crash, the next handle opened for writing resumes it from its last batch.

### 5.3 Dropping an index

`DropIndex` removes the declaration in its transaction and cancels a build in progress. The entries are purged afterwards, in batches of 1,024 that yield to application writers like build batches, by the handle that committed the drop or by the next one opened for writing. A new index under the same name takes a new identifier, so entries left to purge never meet it.

`Close` stops the background builds and purges between batches; with transactions still open it fails with `BUSY` and they resume.

## 6. Queries

### 6.1 Limits

Each query runs under the limits of its handle (§15.4 of the specification). Going over one is an error, never a truncated result.

| Option | Bounds | Default | Error |
|---|---|---|---|
| `MaxQueryBytes` | the source text | 1 MiB | `LIMIT_EXCEEDED` |
| `MaxQueryDepth` | nesting of expressions, patterns, lists and maps | 128 | `LIMIT_EXCEEDED` |
| `MaxBindings` | variables in scope at once (at most 4096) | 256 | `LIMIT_EXCEEDED` |
| `MaxParams` | distinct parameters | 1024 | `LIMIT_EXCEEDED` |
| `MaxHops` | a variable-length relationship | 64 | `RESOURCE_LIMIT` |
| `MaxExpansions` | edges read by the variable-length relationships of a query | 1 million | `RESOURCE_LIMIT` |
| `QueryMemoryBytes` | memory one execution keeps: sorted rows, groups, DISTINCT keys, built lists, the current row, the rows of a clause that writes | 64 MiB | `RESOURCE_LIMIT` |
| `QueryWorkUnits` | rows produced by operators, entities scanned and edges read | 100 million | `RESOURCE_LIMIT` |
| `ResultMemoryBytes` | the rows `Exec` materializes | 64 MiB | `RESOURCE_LIMIT` |

The memory of a query also comes from the execution category of the governor (§1.1): with many queries at once, the ones that do not get it in time fail with `RESOURCE_LIMIT`, while the others finish. A query checks its context and its snapshot every 256 rows produced, entities scanned or edges read; give long reads a deadline.

### 6.2 Cursors

A `Rows` keeps its transaction's snapshot until its end, an error or `Close`. Close it as soon as it is no longer needed: while it is open, `Commit` of its transaction and writes in it fail with `BUSY`, and the snapshot it holds keeps old pages and log segments (§4.2). A `Rows` of `DB.Query` closes its own read transaction at its end.

### 6.3 Writes

`Exec` runs a query to its end and returns all its rows. `DB.Exec` takes the writer for a query that writes, so it waits in the writer queue like `BeginWrite` (§3.2), and commits with the durability of the handle; a failure leaves no effect. `Tx.Exec` is one statement of the transaction: a failure undoes that statement and the transaction goes on.

Each clause that writes holds all its rows in memory before its first effect, under `QueryMemoryBytes`: a write over millions of rows needs batches, each one its own `Exec` with a `LIMIT` before the write, such as `MATCH (n:Old) WITH n LIMIT 10000 DETACH DELETE n`. Its effects also count toward `MaxTxBytes` of the transaction. A `LIMIT` after a write never cuts its effects, so it does not bound that work.

When the context of `DB.Exec` ends after its commit is sealed, `Exec` returns `COMMIT_PENDING` with `Result.Pending` and `Result.Token`: the commit goes on (§3.3). Do not run the query again; ask `CommitStatus(token)`, or wait with `WaitDurable`.
