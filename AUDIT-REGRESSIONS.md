# Traceability of AUD, REV and R13 items

Every item of tables §32.1, §32.4 and §32.5 of the specification, with the milestone by which it must be done and the evidence that closes it. An item is **done** only when a test or measurement is cited; partial evidence is recorded as **partial**.

| ID | Problem | Milestone | Status | Evidence |
|---|---|---|---|---|
| AUD-01 | 200 reads caused 200 fsyncs; commit/abort had no read-only gate | wave 1 | done | M3b: read transactions never touch the log, and a write transaction without pages ends without a COMMIT record; 200 `Commit`, `Rollback`, `View` and empty `Update` calls append and sync nothing (`TestReadsNeverSync`) |
| AUD-02 | Planner k=100; LIMIT 500 returned 54–63 rows | wave 1 | pending | — |
| AUD-03 | 5% label, LIMIT 10: 3 rows on average, sometimes 1 | wave 1/3 | pending | — |
| AUD-04 | The C API ignores the key; a second vector overwrites the first | wave 1/3 | pending | — |
| AUD-05 | Transaction 1025 returns out-of-memory instead of the 1024 limit | wave 1 | done | M2: `MaxActiveTransactions` with `TOO_MANY_TRANSACTIONS` in the engine; M3b: through the public API, with limits 1024 and 1500, the details and the count back at zero (`TestTransactionLimit`) |
| AUD-06 | Immediate "LockTimeout", reported at 26 µs | wave 1/2 | pending | — |
| AUD-07 | Promise/async in the TS binding blocks the event loop with a synchronous native call | wave 2 | pending | — |
| AUD-08 | The design promises zero WAL on reads; the docs describe an HNSW rebuild and a 20k-vector reopen in 0.4 s | wave 1/3 | pending | — |
| AUD-09 | 4× nodes made commits 7–8× slower; a read waited 11.5 s during a 12k-node commit | wave 2 | pending | — |
| AUD-10 | Vector Cypher walks the MATCH: 91/315 ms for 5k/20k vs 1–1.5 ms direct | wave 1/2 | pending | — |
| AUD-11 | `<=>` inside AND loses the index; 1.2 s with 20k | wave 1 | pending | — |
| AUD-12 | Reported Recall@10 of 54% with 20k; ef not tunable from Cypher | wave 1/3 | pending | — |
| AUD-13 | Vector inserts dropped from 180/s to 127/s between 5k and 20k | wave 2/3 | pending | — |
| AUD-14 | >~2500 unique terms send update/delete to a global scan; warning only on stderr | wave 1/3 | pending | — |
| REV-01 | A quiescent checkpoint never runs under continuous reads; the WAL hits its cap and writes fail; read replicas stop applying | M2 | done | M2d: passive checkpoint up to the oldest snapshot in `internal/engine`, segments removed below the floor (OPS-04: `TestCheckpointUnderLoad`; REC-04: `TestCheckpointCrashPoints`, `TestCheckpointFloorMidSegment`); REC-10 in M2a |
| REV-02 | Checkpoint blocked new Begins while copying and syncing | M2 | partial | M2d: the checkpoint takes no admission lock and runs beside readers and writers (`TestCheckpointUnderLoad`, `TestCheckpointConcurrentReaders`); M2e: gate 10 measured by `BenchmarkMaintenance` in the gates job. Reads meet it (p99 ratio 0.95 on the development machine, macOS); commits do not yet (p99 ratio 1.2 to 2.0): the Sync of the main file during a checkpoint delays the log Syncs that overlap it. Lower I/O priority for maintenance is open |
| REV-03 | Backup and Serialize blocked the database for the whole copy | M9 | pending | — |
| REV-04 | A Raft snapshot paused apply; on the leader it froze receipts | M11 | pending | — |
| REV-05 | A forgotten snapshot pinned WAL and memory indefinitely | M2 | done | M2d: `MaxReadTxAge` with cooperative revocation and both pressure policies, with diagnostic events (OPS-05: `TestReadTxAge`, `TestCooperativeRevocation`, `TestPressureExpireOldest`, `TestPressureRejectWrites`) |
| REV-06 | Hot pages were re-emitted by every intent of a group | M2 | done | M2c: the coordinator appends one PAGE per PageID with the final version of the group (TX-11: `TestGroupCommit` scans the log; TX-16: `TestGroupCrash`) |
| REV-07 | A fixed 2 ms window penalized an isolated commit | M2 | done | M2c: adaptive GroupSync with no window, one Sync per isolated commit (TX-17: `TestIsolatedWriter`); M2e: gate 12 reported by `BenchmarkIsolatedCommit` in the gates job (129 µs over the Sync on the development machine) |
| REV-08 | One dedicated Sync for every 1024 IDs | M2 | done | M2c: adaptive blocks reserved at half of the current one, durable with the next group (TX-18: `TestIdentifiers`); M2d: watermarks across checkpoints and crashes (`TestWatermarksAcrossCheckpoints`, `TestWatermarkOfEarlierReservation`) |
| REV-09 | SHA-256 over every image in the serial coordinator | M2 | done | M2a: the manifest digest covers PageID and CRC, not images (`wal.Manifest`); M2c: PAGE records appended equal the distinct final pages of the group (gate 3 count: `TestGroupCommit`). The CPU target of gate 3 is a design target, not measured |
| REV-10 | FTS touched hundreds of pages per document | M5 | pending | — |
| REV-11 | Fuzzy and prefix search without an expansion limit | M5 | pending | — |
| REV-12 | Ranking with LIMIT scored every match | M5 | pending | — |
| REV-13 | Vectors in overflow and a seek per distance | M8 | pending | — |
| REV-14 | No global memory budget | M1 | partial | M1d: `internal/govern` with floors, shared pool, bounded wait and `RESOURCE_LIMIT` details; the page cache (a quota of 16 pages per handle) and value decoding debit it; M2c: overlays and sealed commits debit it; M2e: GOV-01 partial gate, 1024 transactions open at once over four handles fill the governor to its limit with their caches, and the live heap stays within the limit plus 10% (`TestGOV01`); refusals with details arrive with the execution memory of queries; the resident set follows GOGC without GOMEMLIMIT and is reported, not gated; the full GOV-01 with queries in M4 |
| REV-15 | The single writer held by long operations | M2/M10 | partial | M2c: `MaxWriteTxDuration` rolls back a write transaction held too long, the queue moves on and sealed intents stay (TX-19: `TestWriteTxTimeout`); the idle remote transaction in M10 |
| REV-16 | DDL could not index a large database online | M3 | pending | — |
| REV-17 | A consumer wrote one WriteTx per skipped event | M7 | pending | — |
| REV-18 | Metadata and properties in distant regions of the tree | M3 | done | M3b: a property is its own entry `(entity, 1, property)` next to the record `(entity, 0)` (FORMAT.md §13.3); setting one writes one entry, and the keys of an entity stay one contiguous run whatever the order of the writes (`TestColocated`) |
| REV-19 | Low fanout with long keys; wide slots | M1 | done | M1c: prefix compression, 4-byte slots and uvarints in `internal/page`; M1e: truncated separators in `internal/btree` and BT-03 (`TestAgainstModel` with long common prefixes; `TestSeparatorsAreTruncated`, where full separators would need a third level; `TestVerifyRejectsBadSeparators`) |
| REV-20 | Chained overflow without readahead; large values polluted the cache | M1 | done | M1c: extents in the page format; M1e: one sequential `ReadOverflow` per extent (BT-04, `TestLargeValues`, `TestForgedDescriptorExtents`); M2b: the production page reader in `internal/versions` reads extents above `LargeValueBypassBytes` without the cache (`TestLargeValueBypass`) |
| REV-21 | A LIFO freelist scattered pages and defeated compact | M1 | done | M1e: bitmap groups in `internal/alloc`, lowest address first, with a locality hint (BT-05 without scrub: `TestLowestAddressFirst`, `TestRandomAllocFreeWithReopen`); M2d: the checkpoint scrubs freed pages, also after an interrupted checkpoint (`TestScrub`, `TestScrubAfterInterruptedCheckpoint`) |
| REV-22 | Long strings lost range and prefix scans in the index | M3 | pending | — |
| REV-23 | MERGE without an index cost O(N) per call | M3/M7 | pending | — |
| REV-24 | Plan cache polluted by literals | M4 | pending | — |
| REV-25 | A per-statement savepoint could copy the overlay | M2 | done | M2b: persistent overlay with O(1) savepoints (TX-20: `TestStatementSavepoints`, `TestRollbackTo`); M2c: statements of a transaction (`TestRollbackAndStatements`) |
| REV-26 | Contention on the RWMutex, the snapshot registry and global LRUs | M2 | done | M1d: CLOCK with second chance in shards for the page cache; M2c: heads behind atomic pointers and a registry in 16 shards, so Begin and the end of a read take no global lock; M2e: `BenchmarkConcurrency`, the concurrency row of §26.1, runs in the gates job |
| REV-27 | The catalog mixed declarations and hot counters; ambiguous schemaEpoch | M3 | pending | — |
| REV-28 | A cluster receipt required two fsyncs in series | M11 | pending | — |
| REV-29 | Bundles of up to 65 MiB went through Raft | M11 | pending | — |
| REV-30 | One ReadIndex round per read | M12 | pending | — |
| REV-31 | Deduplication retained whole replicated results | M12 | pending | — |
| REV-32 | A heavy typed JSON protocol for vectors and large results | M10 | pending | — |
| REV-33 | Recovery verified the whole database | M2 | done | M2c: recovery verifies only the pages of the log; M2e: REC-11 gate, the open with a log of 1 GiB takes the same time over databases of 1 GiB and 100 GiB (`TestREC11`, ratio 1.04 on the development machine) |
| REV-34 | Float results could differ across platforms | M4/M8 | pending | — |
| REV-35 | Client authentication undefined | M10 | pending | — |
| REV-36 | Authorization without a defined model | M10 | pending | — |
| REV-37 | Tenant isolation depended on WHERE | M10/M12 | pending | — |
| REV-38 | The changefeed, indexes and logs exposed sensitive data | M6/M10 | pending | — |
| REV-39 | Stored injection and confused deputy through events | M7 | pending | — |
| REV-40 | Dynamic identifiers built by concatenation | M4 | pending | — |
| REV-41 | DoS through queries without deadlines, symbols, decoding and sessions | M10 | pending | — |
| REV-42 | Permissions, symlinks and path traversal unspecified | M1 | done | M1b: `internal/vfs`; green on Linux, macOS and Windows (CI run 41250873068, SEC-08) |
| REV-43 | Deleted data persisted in the changefeed, WAL, indexes and backups | M9 | pending | — |
| REV-44 | An unkeyed hash treated as a guarantee; forged files with valid CRCs | M9 | pending | — |
| REV-45 | Leaks through errors, EXPLAIN, tokens and argv | M10 | pending | — |
| REV-46 | Supply chain and bridge without hardening | M13 | pending | — |
| REV-47 | Fuzzing did not cover protocols, manifests and the catalog | M10–M13 | pending | — |
| REV-48 | GPL code cited as an implementation reference | M0 | pending | — |
| R13-01 | A split into two pages was impossible with cells of about 2 KiB (inline values up to 1024 bytes with 1024-byte keys) | M1 | done | M1c: inline rule and canonical size in `internal/page` (`TestTwoWaySplitAlwaysExists`, `TestSplitWithForeignKey`); M1e: BT-06 in `internal/btree` (`TestInlineLimit`, `TestAdversarial2000`) |
| R13-02 | A descriptor page holds 337 extents, with no guarantee that a value fits in it | M1 | done | M1c: descriptor limit and `CheckExtents`; M1e: the allocator falls back to the end of the file (`TestExtentLimitFallsBackToTail`) and a descriptor of 338 extents is CORRUPT (`TestDescriptorWithTooManyExtents`) |
| R13-03 | `os.Root` follows symlinks inside the root, contradicting "without following symlinks" | M1 | done | M1b: `internal/vfs`; green on Linux, macOS and Windows (CI run 41250873068, SEC-08) |
| R13-04 | A hard link to the main file escaped the name-based lock | M1 | partial | M1b: `internal/vfs`; green on Linux, macOS and Windows (CI run 41250873068), but the hard-link case skips when links are refused; the CI now fails on that skip, and the next run closes it (OPS-03) |
| R13-05 | PageID 1, bitmaps and PageIDs ≥ pageCount were not declared invalid as data pointers | M1 | partial | M1c: `CheckPointer`, `CheckExtent`, `Verify` and superblock roots; M1e: `internal/check` (`TestExtentBeyondPageCount`) and BT-02 (`TestForgedDescents`, `TestLeafChainCycle`, `FuzzForgedTree`); SEC-09 in M9 |
| R13-06 | The changefeed was on by default with no automatic retention and grew without bound | M6 | pending | — |
| R13-07 | Imported changefeed payloads mixed the R0 format with G2 in the same stream | M9 | pending | — |
| R13-08 | `result.<node>` inverted the TAC convention `<node>.result`; rows were positional only | M7 | pending | — |
| R13-09 | An orphan WAL from an interrupted creation blocked `Open(Create)` | M2 | done | M2c: `internal/engine` replaces only a log with segment 1 and no record; any other is refused (REC-12, `TestOrphanLog`) |
| R13-10 | Go 1.27.2 already superseded by a fix release | M1 | done | M1a: `go.mod` pins `toolchain go1.27.3`; first green CI run on go1.27.3 (CI run 41250873068) |
| R13-11 | Inconsistent names: the dictionary scan metric had two names; `compatibility.md` | M0 | done | SPEC 1.3 (§2 and §13.3) |
| R13-12 | `created_unix_ns` prevented byte-for-byte binary goldens | M2 | done | M2a: injectable clock in `internal/wal`; golden segment header and records (`TestGoldenRecords`) |
| R13-13 | The reference engine named in code and documentation | M0 | partial | SPEC 1.3 §2.1 and the CI `names` job; needs the `FORBIDDEN_NAMES_REGEX` repository variable |
| R13-14 | Snowball pt/en "in pure Go" with no defined source | M5 | pending | — |
| R13-15 | Format ambiguities: the length used by the inline rule, `key_len` of internal cells, when to set PREFIX, bitmap bit order, the canonical descriptor form and mixed superblock copies | M1 | done | M1c: definitions in `internal/page`, golden pages, `FuzzPage`, `FuzzSuperblock` |
| R13-16 | Split, merge and redistribution measured with the original page prefix underestimated sizes when the new key does not share it; a longer separator could overflow the parent | M1 | done | M1c: canonical size of any cell range; M1e: exact measure of each side (`TestKeyWithoutThePrefix`) and no redistribution whose separator does not fit the parent (`TestRedistributionThatWouldOverflowTheParent`) |
| R13-17 | `Error` had no field for the numeric details that §6.1 requires | M1 | done | M1a: `internal/errs` (`Error.Details`) and its tests |
| R13-18 | §20.9 did not say which objects 0600/0700 and the owner check apply to; the caller's directory was ambiguous | M1 | done | M1b: `internal/vfs`; green on Linux, macOS and Windows (CI run 41250873068, SEC-08) |
| R13-19 | The canonical form with a prefix could be larger than the page without one, and fit decisions based on a private copy's layout failed at finalization | M1 | done | M1c: canonical prefix and size (`TestCanonicalSizeIsMinimal`); M1e: every fit decision on the canonical size of the resulting set (`TestFitsOnlyWithTheNewPrefix`) and a finalization that checks the canonical form (`TestCommitRefusesNonCanonicalPages`) |
| R13-20 | A superblock copy from a future revision could be classified as damaged and silently reopen the older copy | M1 | done | M1c: `Classify` and `ChooseSuperblock` (`TestClassify`, `TestChooseSuperblock`) |
| R13-21 | Windows permissions without an owner check, macOS extended ACLs ignored, Windows junctions outside the symlink rule, path-based checks and inheritable ACEs ignored | M1 | partial | M1b: `internal/vfs`; green on Linux, macOS and Windows (CI run 41250873068), but the junction case skips when `mklink /J` fails; the CI now fails on that skip, and the next run closes it (SEC-08) |
| R13-22 | Write cursors survived a savepoint restore, and a counter restored with the savepoint allowed ABA | M1/M2 | done | M1e: per-tree mutation counters and the restore counter of the transaction, both monotonic (`TestCursorInvalidation`); M2b: the overlay of `internal/versions` restores in O(1) and counts restores (`TestRollbackTo`, `TestStatementSavepoints`); M2c: transactions restore the roots of every open tree, including trees opened after the savepoint (`TestRollbackAndStatements`) |
| R13-23 | The tail rule of §11.2 turned an ordinary power loss into CORRUPT when an unsynced append reached the disk out of order, or would drop an acknowledged commit corrupted after its Sync | M2 | done | M2a: `durable_lsn` in BEGIN, segment roll through `spare.tmp`, new segment after a discarded tail, evidence-based tail classification (`TestTornTailIsDiscarded`, `TestCorruptionAfterSync`, `TestDiscardedTailDoesNotResurrect`); M2e: the crash matrix of §11.3 with lost, torn, reordered and truncated writes (`TestCrashMatrix`, `TestCrashTruncatedSegment`, `TestDiscardedTailStaysDiscarded`, `TestProcessDeathThenPowerLoss`) |
