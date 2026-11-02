# FORMAT — the tacdb G2 format

This document describes the encodings of the G2 format **as implemented**, with examples that the tests check byte for byte. The normative source is the specification (§5.3, §7 and §9); where the two disagree, the specification wins and the difference is a bug.

The document grows with each milestone. This version covers the value codec, uvarints and composite keys (M1a), the database files (M1b), and pages, extents, bitmaps and the superblock (M1c). The WAL arrives with M2.

## 1. Conventions

| Item | Rule |
|---|---|
| Payload integers | Little-endian, fixed width. |
| Integers in B+tree keys | Big-endian, so that byte order is numeric order. |
| Variable-length integers | Uvarint (LEB128) in minimal form (§3). |
| Strings | Valid UTF-8, not normalized. |
| Canonical form | Every value has exactly one encoding; the decoder rejects any other. |

## 2. Value codec

Implementation: `internal/codec/value.go`. Checked examples: `internal/codec/testdata/golden/values.golden`.

Every value starts with a 1-byte tag:

| Tag | Type | Payload after the tag |
|---|---|---|
| `0x00` | Null | None. |
| `0x01` | Bool | 1 byte: `0x00` or `0x01`. |
| `0x02` | Int | int64, 8 bytes. |
| `0x03` | Float | The 8 bytes of `math.Float64bits`; NaN payloads, infinities and `-0` are preserved. |
| `0x04` | String | u32 length and the UTF-8 bytes. |
| `0x05` | Bytes | u32 length and the bytes. |
| `0x06` | Vector | u32 dimension (1 to 65535) and the bits of each float32 component (4 bytes). |
| `0x07` | List | u32 count and the values, recursively. |
| `0x08` | Map | u32 count and the pairs `u32 key length + UTF-8 key + value`, in increasing byte order of the key, without repetition. |

Examples (hex):

| Value | Encoding |
|---|---|
| `Int(-1)` | `02 ffffffffffffffff` |
| `Int(2^53+1)` | `02 0100000000002000` |
| `Float(1)` | `03 000000000000f03f` |
| `Float(-0)` | `03 0000000000000080` |
| `String("ação")` | `04 06000000 61c3a7c3a36f` |
| `Vector([1, 2])` | `06 02000000 0000803f 00000040` |
| `Map{"a": true, "origem": "manual"}` | `08 02000000 01000000 61 0101 06000000 6f726967656d 04 06000000 6d616e75616c` |

### 2.1 Decoding

The decoder takes limits (`value.Limits`) and an optional budget (`codec.Budget`).

| Situation | Result |
|---|---|
| Unknown tag, Bool other than 0/1, invalid UTF-8, Vector dimension 0 or above 65535 | `CORRUPT` |
| Map keys out of order or repeated (non-canonical form) | `CORRUPT` |
| Length or count larger than the remaining bytes | `CORRUPT`, before allocating |
| Bytes left after the value | `CORRUPT` |
| Depth above `MaxDepth` (default 64; a scalar has depth 1) | `LIMIT_EXCEEDED` |
| List and Map elements, summed over all levels, above `MaxValueElements` (default 1,000,000) | `LIMIT_EXCEEDED` |
| Map key above `MaxMapKeyBytes` (default 64 KiB) | `LIMIT_EXCEEDED` |
| Vector dimension above `MaxVectorDimensions` (default 4096, at most 65535) | `LIMIT_EXCEEDED` |
| Encoding above `MaxValueBytes` (16 MiB) | `LIMIT_EXCEEDED` |

The budget is charged **before** each allocation: 64 bytes per element, plus the variable bytes of strings, bytes and vectors. An error from the budget stops decoding and is returned unchanged.

## 3. Uvarint

Implementation: `internal/codec/uvarint.go`. This is the LEB128 of `encoding/binary` with one extra rule: the form must be minimal. A uvarint longer than one byte whose last byte is `0x00` is not minimal and is `CORRUPT`, as is a truncated one or one larger than 64 bits.

| Value | Bytes |
|---|---|
| 0 | `00` |
| 127 | `7f` |
| 128 | `80 01` |
| 1024 | `80 08` |
| 2^24 (16 MiB) | `80 80 80 08` |

## 4. Composite keys

Implementation: `internal/codec/key.go`. A B+tree key concatenates components whose byte order is their logical order:

- Unsigned integers: big-endian, fixed width (u8, u16, u32, u64).
- Strings and bytes: each `0x00` becomes `0x00 0xFF`, and the component ends with `0x00 0x00`. So `"a" < "a\x00"` holds for the bytes too (`61 00 00` before `61 00 FF 00 00`), and the terminator separates the component from the next one.
- String prefix: the escaped form **without** the terminator is a prefix of the key of every string that starts with it, for prefix scans.
- End of range: `PrefixEnd(p)` returns the smallest key greater than every key that starts with `p` (it increments the last byte other than `0xFF`); it returns nothing when no such key exists.
- Maximum length of an encoded key: 1024 bytes (`CheckKey`), checked before any mutation.

## 5. Database files

Implementation: `internal/vfs`. A database `name.tacdb` lives in one directory and uses these names (§7.1):

| Name | Content |
|---|---|
| `name.tacdb` | Main file. |
| `name.tacdb.lock` | Lock file: created empty on first open and never removed, so the lock name never changes. |
| `name.tacdb-wal/` | Directory of the WAL (§12): segments named `NNNNNNNNNNNNNNNN.wal` with the 16 hexadecimal digits of their sequence number, and `spare.tmp`, the next segment prepared in advance. |
| Temporaries (M2) | Always in the destination directory, with the database prefix and a random `crypto/rand` suffix. |

Every name goes through `vfs.ValidName`: one or two components separated by `/`, each starting with a letter or digit, made only of letters, digits, `.`, `_` and `-`, not ending with `.`, and not a reserved Windows device name (`CON`, `NUL`, `COM1` and the like). Any other name is `UNSAFE_PATH`.

The database lock has three layers (§8.3, R13-04): the `.lock` file, the main file itself (which catches hard-link aliases, whose `.lock` file is another one) and an in-process registry keyed by file identity. The exclusive lock belongs to the owner; read-only processes share the shared lock while there is no owner.

## 6. Pages

Implementation: `internal/page`. Golden images: `internal/page/testdata/golden/*.bin`. Pages have 4096 bytes; page `N` lives at byte `N*4096` of the main file. Pages 0 and 1 are the two superblock copies; page 2 is the bitmap of the first allocation group.

Every page starts with a 48-byte header:

| Offset | Bytes | Field |
|---:|---:|---|
| 0 | 1 | Type: free 0, internal 1, leaf 2, overflow 3, bitmap 4, fixed 5. |
| 1 | 1 | Flags: bit 0 = PREFIX on slotted pages; zero elsewhere. |
| 2 | 2 | `header_bytes` = 48. |
| 4 | 4 | CRC32C (Castagnoli) of the whole page, computed with this field zeroed. |
| 8 | 8 | The page's own ID. |
| 16 | 8 | CommitID of this version of the page. |
| 24 | 2 | Slots of a slotted page (the prefix slot included); useful bytes of an overflow or fixed page; zero otherwise. |
| 26 | 2 | `lower`: end of the slot array of a slotted page. |
| 28 | 2 | `upper`: start of the cell area of a slotted page. |
| 30 | 2 | Level: 0 for a leaf, at least 1 for an internal page. |
| 32 | 8 | Next leaf (leaf), group number (bitmap) or index ID (fixed). |
| 40 | 8 | First child (internal). |

Fields a type does not use are zero. A page read from storage goes through `page.Verify` before any other code sees it: CRC, header, the structure of its type and every page reference it holds (against `pageCount`). `page.Canonical` additionally checks the canonical form; the structural check (§11) uses it.

## 7. Slotted pages (leaf and internal)

Slots of 4 bytes (`offset u16`, `length u16`) follow the header, in key order, so `lower = 48 + 4*slots`. In canonical form, the cells are packed in slot order right after `upper`, and the last one ends at byte 4096; the space between `lower` and `upper` is zero.

**Prefix (R13-19).** Let `p` be the longest common prefix of the first and last full key. The PREFIX flag is set if and only if the page has at least two keys, `p >= 1`, and the encoding with prefix `p` is strictly smaller than the encoding without a prefix. When it is set, slot 0 holds exactly those `p` bytes and every cell stores only the rest of its key. The canonical size of a set of cells is the size of this form, and it is never larger than the size with any other prefix choice. Every fit decision uses it.

**Leaf cell.** `flags u8 | uvarint(suffix length) | uvarint(value length) | key suffix | payload`.

| Flags | Payload |
|---|---|
| `0x00` | The value itself (inline). |
| `0x01` | `extent count u8` (1 to 4), then that many extents of `start u64 + pages u32`. |
| `0x03` | `descriptor u64`: an overflow page listing 5 to 337 extents. |

**Inline rule (R13-01, R13-15).** A value is inline if and only if `1 + uvlen(len(key)) + uvlen(len(value)) + len(key) + len(value) <= 1344`, with the full key. The measure does not depend on the page prefix. With it, each cell and its slot take at most `(4048 - 4) / 3 = 1348` bytes, so any full page plus one more cell splits into two pages; a leaf holds at least three cells, and the worst out-of-page cell (1024-byte key, four extents) takes 1080 bytes.

**Extents.** A value out of the page needs `ceil(length / 4048)` overflow pages. Up to 4 extents stay inline; 5 to 337 go to a descriptor page; never more. Extents follow the order of the value's bytes, each has at least one page, and two consecutive extents are never physically contiguous (they would be one).

**Internal cell.** `uvarint(separator suffix length) | right child u64 | separator suffix`. The first child is the header's `aux` field. Child 0 holds keys below separator 0; the right child of separator `i` holds keys from separator `i` up to (not including) separator `i+1`.

Example (golden `leaf-prefix.bin`): keys `user/0001` and `user/0002` share the 8-byte prefix `user/000`; with it the page uses 84 bytes, without it 88, so the prefix is used. Slot 0 points to `user/000`, slot 1 to `00 01 05 31 616c696365` (inline, suffix `1`, value `alice`), slot 2 to `00 01 03 32 626f62`.

## 8. Other pages

| Type | Content |
|---|---|
| Overflow | Up to 4048 useful bytes from offset 48; `count` gives their number; the rest is zero. Every page of a value holds 4048 bytes except the last. |
| Descriptor | An overflow page whose useful bytes are 5 to 337 extents of 12 bytes; `count = 12 * n`. |
| Bitmap | Group `g` has its bitmap at page `2 + g*32385` and covers the 32384 pages after it; the header's offset 32 holds `g`. The page of index `i` in the group (`i = PageID - bitmap - 1`) is bit `i % 8`, counting from the least significant, of byte `48 + i/8`; 1 means allocated. |
| Free | What the scrub leaves behind: type 0, with `header_bytes`, CRC, page ID and CommitID set and every other byte zero. |
| Fixed | Vector storage (M8); this milestone checks only the header. |

## 9. Pointers

A page reference is valid only if it is not 0, not 1 (a superblock), not a bitmap page and below `pageCount` (`page.CheckPointer`, R13-05). An extent is valid only if it is not empty, ends at or below `pageCount` and covers no superblock or bitmap page (`page.CheckExtent`).

## 10. Superblock

Pages 0 and 1 hold two copies; a checkpoint writes the copy with the older generation.

| Offset | Bytes | Field |
|---:|---:|---|
| 0 | 8 | Magic `TACDBG2\0`. |
| 8 | 2 | `format_version` = 2. |
| 10 | 2 | `min_reader_version` = 2. |
| 12 | 4 | `page_size` = 4096. |
| 16 | 16 | Database UUID. |
| 32 | 8×12 | Generation, `checkpoint_lsn`, CommitID, schema epoch, node count, edge count, NodeID high-watermark, EdgeID high-watermark, SymbolID high-watermark, `pageCount`, `wal_floor_segment`, flags (bit 0 = cluster). |
| 128 | 8×32 | Tree roots; 19 to 31 are reserved and zero. |
| 384 | 8 | `free_page_count`. |
| 392 | 8×2 | Applied Raft index and term (zero in standalone). |
| 408 | 2 | `format_revision` = 13. |
| 410 | 3682 | Reserved, zero. |
| 4092 | 4 | CRC32C of bytes 0..4091. |

**Choosing a copy (R13-20).** Each copy is classified in this order: *foreign* if the magic differs; *damaged* if the CRC is wrong; *unsupported* if `format_version` is not 2, `format_revision` is not 13 or `min_reader_version` is above 2; *damaged* if any other field is invalid (`min_reader_version` below 2, page size, unknown flags, non-zero reserved bytes or roots 19..31, `pageCount` below 2 or too large, `free_page_count` above `pageCount`, a root that is not a valid pointer); *valid* otherwise. Then: any unsupported copy, or two foreign copies, make the file `UNSUPPORTED_FORMAT`; between valid copies the higher generation wins, and equal generations with different bytes are `CORRUPT`; with no valid copy the file is `CORRUPT`. Future formats keep the magic, the version fields and the CRC at these offsets so that this reader recognizes them as unsupported.

## 11. Trees and allocation

Implementation: `internal/btree`, `internal/alloc`, `internal/check`. `btree.Verify` checks one tree and `check.Structural` the whole state; a valid state follows these rules (§8.1, §8.4):

- **Roots.** An empty tree has root 0. A root is a leaf or an internal page with at least one separator; a root with one child collapses into it.
- **Levels.** A leaf has level 0, and each child of an internal page of level `L` has level `L - 1`. A tree has at most 40 levels.
- **Keys and separators.** Keys strictly increase within a page. Every key of child `i` lies in `[separator i-1, separator i)`, so each separator `s` between a left subtree `L` and a right subtree `R` satisfies `max(L) < s <= min(R)`.
- **Leaf chain.** The next-leaf field links the leaves in key order; the last one holds 0.
- **Ownership.** Every tree page, overflow page and descriptor page belongs to exactly one tree, in one place.
- **Bitmaps.** A bit is set if and only if its page is owned. Bits of pages at or beyond `pageCount` are zero, and `free_page_count` is the number of clear bits of data pages below `pageCount`.

The placement of pages and the choice of separators are policy, not format: a reader accepts any state that follows the rules above. tacdb splits a leaf with the shortest valid separator, `min(R)[:lcp(max(L), min(R)) + 1]`, and an internal page by moving its middle separator up; merges and redistributions keep the separators valid but not necessarily shortest. tacdb takes a new page from the lowest free address of the group of a related page (a sibling or the parent), then from the lowest free address, then from the end of the file, where reaching the position of the next bitmap page creates that group. A value takes one contiguous free run when there is one; otherwise free runs from the lowest address, completed at the end of the file; and when that would take more than 337 extents, one run at the end of the file, broken only by bitmap pages (R13-02). Freeing a page only clears its bit; `pageCount` does not shrink.

## 12. Write-ahead log

Implementation: `internal/wal`. Golden images: `internal/wal/testdata/golden/*.bin`, written with a fixed clock (R13-12). All integers are little-endian; CRCs are CRC32C (Castagnoli).

### 12.1 Segments

A segment has a 4096-byte header, followed by records. It is created at its target size (`WALSegmentBytes`, 64 MiB by default) filled with zeros; a group never splits across segments, so a segment may grow past its target to finish one. Writer and recovery share a bound on the size of a segment (256 MiB by default): the writer refuses a group that would not fit a new segment under it, and recovery refuses a larger segment.

| Offset | Bytes | Field |
|---:|---:|---|
| 0 | 8 | Magic `TACWAL2\0`. |
| 8 | 2 | Version = 2. |
| 10 | 2 | `header_bytes` = 4096. |
| 12 | 4 | `page_size` = 4096. |
| 16 | 16 | Database UUID. |
| 32 | 8 | `segment_seq`: starts at 1 and grows by one; never reused. |
| 40 | 8 | `first_lsn`: LSN of the first record of the segment. |
| 48 | 8 | `base_checkpoint_lsn` in force when the segment was created. |
| 56 | 8 | `created_unix_ns` (i64), from an injectable clock. |
| 64 | 2 | `format_revision` = 13. |
| 66 | 2 | Reserved, zero. |
| 68 | 8 | `prev_valid_end`: where the valid data of the previous segment ends; zero in segment 1 (R13-23). |
| 76 | 4016 | Reserved, zero. |
| 4092 | 4 | CRC of bytes 0..4091. |

**Rolling to the next segment.** The next segment is prepared in the background as `spare.tmp`: created, sized and synced. To move appends to it, the writer syncs the current segment, writes the real header into the spare and syncs it, renames it to its final name and syncs the directory. Every segment except the last is therefore complete on disk. A `spare.tmp` found when opening is discarded.

**After recovery.** When recovery discarded a tail (§12.4), the next writer starts a new segment whose `prev_valid_end` marks the end of the recovered prefix; the bytes after it are never read again, so a discarded tail cannot come back under reused LSNs. Otherwise appends continue in the last segment, after its last valid record.

**Floor.** `wal_floor_segment` in the superblock is the segment that holds the record `checkpoint_lsn + 1`, or the last segment when that record is not written yet; records up to `checkpoint_lsn` are skipped. When the COMMIT at `checkpoint_lsn` is the last record of its segment and the next segment exists, the floor is that next segment; when more records follow the COMMIT in its segment, the floor stays there, so that recovery reads them.

### 12.2 Records

A record has a 64-byte header, then the payload, then zero padding to a multiple of 8 bytes; it is at most 8192 bytes long.

| Offset | Bytes | Field |
|---:|---:|---|
| 0 | 4 | Magic `0x32524354`. |
| 4 | 1 | Type: BEGIN 1, PAGE 2, COMMIT 3, ABORT 4, RESERVE_IDS 5, APPLIED 6 (cluster only). |
| 5 | 1 | Flags, zero. |
| 6 | 2 | Header size, 64. |
| 8 | 4 | Total length, header and padding included. |
| 12 | 4 | Payload length. |
| 16 | 8 | LSN; every record has the LSN of the previous one plus one. |
| 24 | 8 | GroupID; zero in RESERVE_IDS. |
| 32 | 8 | CommitID: of the intent (BEGIN), of the page version (PAGE) or of the last intent (COMMIT); zero in RESERVE_IDS. |
| 40 | 8 | LSN of the previous record of the same group, or zero. |
| 48 | 8 | PageID in PAGE; zero otherwise. |
| 56 | 4 | Reserved, zero. |
| 60 | 4 | CRC of header, payload and padding, computed with this field zeroed. |

| Type | Payload |
|---|---|
| BEGIN | 48 bytes: `token:16` (random), `parentCommitID:u64`, `commitID:u64`, `flags:u32` (zero), reserved `u32`, `durable_lsn:u64`. |
| PAGE | The 4096-byte image of the final version of the page in the group, with its CommitID and CRC. |
| COMMIT | `intent_count:u32`, `page_count:u32`, the 384-byte State and the 32-byte manifest digest. |
| ABORT | Empty: the group in progress is not recognized. |
| RESERVE_IDS | `kind:u8` (node 1, edge 2), 7 zero bytes, `high_watermark:u64`. Written only between groups. |

**State (384 bytes).** Nine u64 — CommitID, schema epoch, node count, edge count, NodeID, EdgeID and SymbolID high-watermarks, `pageCount`, `free_page_count` — then the 32 roots, `parentCommitID` at offset 328, the applied Raft index and term at 336 and 344 (zero in standalone) and 32 zero bytes.

**Manifest.** The digest is the SHA-256 of, in LSN order: for each BEGIN, `token + parentCommitID + commitID`; for each PAGE, `PageID:u64` and the 4-byte CRC field of the page.

### 12.3 Groups

A group is its BEGINs, in the order of the chain of intents, then one PAGE per PageID in increasing order, each with the final version of the page in the group, then the COMMIT. The first BEGIN has as parent the last recovered CommitID; each following BEGIN has as parent the commit of the previous one; the COMMIT counts the intents and pages, carries the State of the last intent and the digest of the manifest. Groups have increasing GroupIDs, also across checkpoints: a writer resumes after the highest GroupID in the log, checkpointed groups included. The writer applies the same checks as recovery before appending, so it never acknowledges a group that recovery would refuse.

**`durable_lsn` (R13-23).** The highest LSN of any record whose write completed before a Sync that then returned success, as known when the group was formed. It is a lower bound: a lower value only weakens the detection below, a higher one would turn an ordinary crash into `CORRUPT`.

### 12.4 Recovery of the log

1. The segments from the floor up must form a contiguous sequence, carry the database UUID and have continuous LSNs; the first starts at or before `checkpoint_lsn + 1`. Anything else in the directory except `spare.tmp` is `CORRUPT`. A segment below the floor is a removable leftover once its header shows it belongs to the database; a spare is always removable. A segment larger than the scan bound (256 MiB by default) is `CORRUPT`.
2. A segment that is not the last is valid up to the `prev_valid_end` declared by the next one; any defect before that point is `CORRUPT`.
3. In the last segment, the data ends at a record whose magic is zero and after which every byte is zero. Records up to `checkpoint_lsn` are skipped with only their fixed header fields checked; the log must reach `checkpoint_lsn`, which the checkpoint proves durable, so a log that ends before it, or a defect before it, is `CORRUPT`.
4. Only complete, valid groups are recovered: CRCs, LSNs, the chain of the group, counts, the chain of parents, increasing GroupIDs, the digest, and every page image (`page.Verify`, a PageID from 2 to below the `pageCount` of the group's State, and the CommitID of one of its intents). ABORT has no payload and ends the group in progress without recognizing it. An APPLIED record in a standalone log is `UNSUPPORTED_FORMAT`.
5. A defect in the last segment — an invalid record, an incomplete group, a broken chain, or a zero magic followed by data — at the end P of the last complete unit is classified by evidence. An unsynced append may reach the disk in any order, so a later record can be intact while an earlier one is not. The rest of the segment is searched, at 8-byte boundaries, for plausible records — fixed header fields, an LSN at or after the one at P and within what the segment can hold, and the payload length of a BEGIN or a RESERVE_IDS, all checked before the CRC — whose CRC is valid. If any BEGIN among them declares a `durable_lsn` at or beyond the LSN at P, the region had been synced and the log is `CORRUPT`; otherwise the tail is discarded. RESERVE_IDS records with a valid CRC in a discarded tail are still applied, which only skips identifiers.

The last synced group has no later evidence: if it is corrupted on the medium after its Sync and nothing newer survives, it is discarded as a tail. This residual risk is accepted (R13-23).

### 12.5 Creating a database

A new database first gets its log: the directory `name.tacdb-wal/` with segment 1 synced, then the directory and its parent synced. Then the main file is written as a temporary, `name.tacdb.<16 hex digits>.tmp`, with the initial superblock in page 0 (generation 1, all roots zero, `pageCount=2`, CommitID and `checkpoint_lsn` zero, `wal_floor_segment=1`) and page 1 zero; it is synced, renamed to `name.tacdb`, and the directory is synced. A published main file therefore always has its log.

A creation that crashes before the rename leaves a log without a main file. Creating the database again replaces that log only if it holds nothing but segment 1 (and possibly `spare.tmp`), with a valid header and no record: the state only an interrupted creation produces. Any other log without a main file is refused with `DESTINATION_EXISTS`, and a main file without its log is `RECOVERY_REQUIRED`. Temporaries left by an interrupted creation are not removed.

### 12.6 Checkpoint

A checkpoint copies to the main file the state of a target commit T: the durable head, or the oldest snapshot if older. Every live snapshot reads at or after T, so none of them reads the pages it rewrites from the main file.

1. Before any write, it computes what to write: every page with a version in the log newer than the CommitID of the superblock and at most T, except the pages free in T; and, with the scrub, the pages free in T that are allocated in the bitmap of the main file or have such a version. The scrub writes them as free pages (§8). A bitmap of the main file that fails verification, or whose CommitID is newer than the superblock's, was overwritten by an interrupted checkpoint: every page of its group free in T is scrubbed.
2. It writes those pages in PageID order, never at or above the `pageCount` of T, and syncs the main file.
3. It writes the superblock copy with the older generation: generation + 1, the State of T, `checkpoint_lsn` the LSN of the COMMIT that published T, the floor of §12.1, and each identifier high-watermark at least the previous superblock's, T's and that of every reservation at or below `checkpoint_lsn` (recovery skips those records). It syncs the main file.
4. It removes the segments below the new floor that no snapshot still reads, and syncs the log directory.

A crash before step 3 keeps the previous superblock and every segment from its floor; pages already written are read from the log until a checkpoint completes. A crash after it leaves segments below the floor, which the next open removes once recovery is done. A checkpoint can be repeated any number of times.


## 13. Logical trees

Implementation: `internal/catalog`. The State holds 19 roots; slots 19 to 31 are reserved and zero. A root of 0 is an empty tree. Keys follow §4.

| Slot | Tree | Key → value |
|---:|---|---|
| 0 | Nodes | `(node u64, 0)` → node record; `(node u64, 1, property u32)` → Value (§13.3) |
| 1 | Adjacency | `(node u64, direction u8, type u32, peer u64, edge u64)` → empty (§13.3) |
| 2 | Labels | `(label u32, node u64)` → empty |
| 3 | SymbolsForward | `(kind u8, name string)` → `id` u32 big-endian |
| 4 | SymbolsReverse | `(kind u8, id u32)` → name, UTF-8 bytes |
| 5–8 | FTSRuns, FTSPostings, FTSDocs, VectorMeta | later milestones |
| 9 | Edges | `(edge u64, 0)` → edge record; `(edge u64, 1, property u32)` → Value (§13.3) |
| 10–12 | StreamMeta, StreamEvents, StreamOffsets | later milestones |
| 13 | Catalog | `(prefix u8, …)` → record (§13.1) |
| 14, 15 | NodeProperty, EdgeProperty | `(index u64, sortable value, entity u64)` → empty (§13.4) |
| 16 | Stats | `(prefix u8, …)` → record or counter; prefix 1 holds index entry counts (§13.4), prefix 2 degrees (§13.3) |
| 17 | Sessions | later milestones |
| 18 | Maintenance | `(prefix u8, …)` → record (M3d) |

### 13.1 Records

A record of Catalog, Stats, Sessions or Maintenance is `version` u16 little-endian followed by a Map (§2) whose keys are the fields of its kind of record. A reader rejects version 0 and an unknown field, a field of another kind or a missing required field in a version it knows (`CORRUPT`); a version newer than it knows is `UNSUPPORTED_FORMAT`.

Catalog prefixes: 1 configuration, 2 index declarations, 3 analyzer versions, 4 durable operational state and internal identifier watermarks, 5 symbol watermarks, 6 classification and policies. Only changes under 2, 3 and 6 raise `schemaEpoch`, at most once per commit.

### 13.2 Symbols

Three namespaces: label (kind 1), property (2) and edge type (3). An identifier is a u32 counted from 1 in its namespace; 0 is reserved. A name is valid UTF-8, not empty, without NUL and at most 255 bytes.

The watermark of a namespace is the Catalog record at key `(5, kind)`, version 1, with one required field `hwm` (Int): the largest identifier handed out. A new symbol takes `hwm + 1`, writes its forward and reverse entries and the new watermark in the same commit, and raises the State's `symbolHWM`, which is the largest watermark of the three namespaces and only checks them: a namespace watermark above it, or an identifier above a watermark, is corruption. Symbols are never removed. `MaxSymbolsPerNamespace` (default 1,000,000) limits each namespace.

### 13.3 Graph

Implementation: `internal/graph`. Integers inside records are little-endian; keys follow §4.

A node record is `version` u16 (2), a reserved u16 (0), `labelCount` u32 and that many label identifiers (u32), in insertion order, none zero or repeated. An edge record is 28 bytes: `version` u16 (2), a reserved u16 (0), `source` u64, `target` u64, the edge type identifier u32 and a reserved u32 (0). A reader rejects an older version, reserved bits or a wrong length (`CORRUPT`) and a newer version (`UNSUPPORTED_FORMAT`).

Each property is its own entry `(entity, 1, property)` whose value is the codec form (§2) of the Value; an absent entry is an absent property, and a stored Null is a value. The record and the properties of an entity are contiguous, and identifiers are never reused.

An edge has two adjacency entries: `(source, 0, type, target, edge)` and `(target, 1, type, source, edge)`, also when source and target are the same node.

A degree counter is the Stats entry `(2, node u64, direction u8, type u32)` → count u64, never zero: the entry is removed when the count reaches zero. Direction 0 counts outgoing entries, 1 incoming entries and 2 self-loops; type 0 counts every type. The undirected degree is outgoing + incoming − self-loops, so a self-loop counts once. Counters change in the commit that changes the edges.

The State's `nodeCount` and `edgeCount` are the live nodes and edges.

### 13.4 Property indexes

Implementation: `internal/index`.

**Declarations.** An index is the Catalog record at key `(2, index u64)`, version 1, with the required fields `name` (String, a structural name), `entity` (Int: 0 nodes, 1 edges), `scope` (Int: the label or edge type identifier), `property` (Int: the property identifier), `kind` (Int: 0, a property index), `unique` (Bool), `state` (Int: 0 BUILDING, 1 ONLINE, 2 FAILED) and `prefixBytes` (Int, 16 to 989: the `IndexKeyPrefixBytes` the index was created with, which it keeps). Names are unique. Index identifiers count from 1 and are never reused: the largest one handed out is the Catalog record at key `(4, 1)`, version 1, field `hwm` (Int). Creating or dropping an index, or changing its state, raises `schemaEpoch` once per commit; entries and counters never do.

**Entries.** Writers keep every BUILDING or ONLINE index. An entity in the scope of an index with its property has one entry in tree 14 (nodes) or 15 (edges): the index identifier u64 big-endian, the sortable form of the value, then the entity identifier u64 big-endian; the value of the entry is empty. An absent property has no entry; a stored Null has one. The entry count of an index is the Stats entry `(1, index u64)` → count u64 little-endian, never zero (removed at zero), changed in the same commit as the entries.

**Sortable form.** A tag, then the value; the tags order the families, Null apart:

| Tag | Family | Value |
|---|---|---|
| `0x01` | Null | nothing |
| `0x10` | Bool | u8, 0 or 1 |
| `0x20` | Int | u64 big-endian of the value with its sign bit flipped |
| `0x21` | Float, finite | u64 big-endian of the bits: all flipped if negative, the sign bit flipped otherwise; `-0` is written as `+0` |
| `0x30` | String | escaped (§4) and terminated with `00 00`, or truncated |
| `0x40` | Bytes | as String |
| `0xF0` | List, Map, Vector | the SHA-256 of the equality key |

Ordered families refuse NaN and infinities: a write or a backfill that would index one fails with `INVALID_ARGUMENT`.

**Truncation.** When the escaped form of a String or Bytes value is longer than `prefixBytes`, the entry keeps the longest prefix of the escaped form of at most `prefixBytes` bytes that does not split an escape `00 FF`, then the marker `00 01` and the first 16 bytes of the SHA-256 of the value. `00 01` sorts after the terminator `00 00`, so a truncated key follows the text equal to its prefix and precedes every greater text; texts that share the same truncated prefix are ordered by their full values when read.

**Equality key.** Uniqueness and the hashed family use the canonical equality key of a value: Null `00`; Bool `01` and u8; a finite non-zero number `02`, a sign u8, then the exponent i32 and the odd mantissa u64 (big-endian) of its exact binary fraction, so Int 1 and Float 1.0 meet; zero `03`, either sign; NaN `04`; +Inf `05`; −Inf `06`; String `07` and Bytes `08`, each with a u32 length and the bytes; Vector `09`, List `0A` and Map `0B`, each with a u32 count, then the components as numbers, the items, or each key with a u32 length followed by its value, keys in byte order. A hash never proves equality: the base record decides.

