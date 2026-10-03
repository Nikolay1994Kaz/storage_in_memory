# Architecture

A map for reading the code: what the parts are, how a command travels through
them, how state survives a crash, and which invariants hold the design together
— each with the test that pins it. Numbers behind the design choices are in
[BENCHMARKS.md](BENCHMARKS.md); the command surface is in
[COMMANDS.md](COMMANDS.md); on-disk formats in [FORMAT_COMPAT.md](FORMAT_COMPAT.md).

## Shape

One process, one data directory. A RESP server in front; behind it in-memory
structures — strings with TTL, sorted sets, pub/sub, and the vector/text engine;
under them a write-ahead log and two snapshot files. Everything a query touches
is in memory; the disk holds what recovery needs.

## The path of a command

1. **Connection** (`internal/server`). One goroutine per connection on Go's
   netpoller. `ConnBuf` reads into a buffer that starts at 4 KB and doubles on
   demand (a buffer that grew past 1 MB is released once it drains). The parser
   returns arguments as slices into that buffer — no copies, no allocations —
   and replies are appended to a write buffer flushed once per pipelined batch.
   A read deadline reaps idle connections; `Stop` wakes every reader and drains.
2. **Entry** (`cmd/kvstore/dispatch.go`, `commands.handle`): AUTH, subscriber
   mode (a connection with subscriptions is served only by the pub/sub hub,
   which owns the socket's single writer), `MULTI`/`EXEC`/`DISCARD` queueing.
3. **Gates** (`commands.execute`): memory (`-maxmemory`; only commands that
   grow memory are refused, so deleting is always possible), point-in-time
   restore (read-only), WAL fail-stop (no mutation after a disk error, reads
   still work).
4. **Handler** (`cmd_kv.go`, `cmd_zset.go`, `cmd_vector.go`, `cmd_pubsub.go`,
   `cmd_cluster.go`): parse and validate everything first, then change the
   in-memory structure and append the change to the WAL.

## In-memory structures

| Package | What | Notes |
|---|---|---|
| `internal/store/kv` | strings | 256 shards of `map[string][]byte` under `RWMutex`. `Set` takes ownership of the value, `Get` returns it read-only: no copy, no allocation. Per-shard accounting feeds the `maxmemory` gate. |
| `internal/store` | TTL | 256 shards of `map[key]deadline` on the monotonic clock, so a wall-clock jump does not expire keys; lazy expiry on read plus a background sweep of 4 shards every 100 ms (a full pass takes about 6 s). The evictor removes the key and its vector and logs both. |
| `internal/store/zset`, `internal/btree` | sorted sets | B+-tree keyed by (score, member) in Redis order plus a member→score map; ranges walk the leaf chain. |
| `internal/pubsub` | pub/sub | Named channels; one writer goroutine per subscriber; a slow subscriber is closed through the server's single close path. |
| `vector/` | vectors and text | An LSM of HNSW segments with BM25 and attribute columns — below. |

## The vector engine: an LSM of HNSW segments

- **Delta** (`delta.go`, `graph.go`). Writes land in N shards (`-delta-shards`),
  each a mutable HNSW graph built incrementally on insert, together with the
  document's attribute values and BM25 terms.
- **Flush** (`leveled_compaction.go`). When the delta is full, the coordinator
  swaps it out under the lock (microseconds) and freezes each shard's
  already-built graph into an immutable segment: CSR float32 for dim ≤ 256
  (`frozen.go`), SQ8 codes with `-hnsw-use-sq` (`frozen_sq.go`), or a plain HNSW
  graph above 256 dimensions. No graph is rebuilt on the flush path, and the
  frozen delta stays searchable until its segments are published.
- **Merge**. Segments sit in up to 8 levels; when a level collects `fanout` (4)
  segments they merge into one segment of the next level: entries are decoded,
  tombstoned keys dropped, upserts deduplicated, one graph built
  (`mergeSegments`, `buildSegment`). After a write lull (`-idle-consolidate`,
  60 s) the whole index consolidates into a single segment.
- **Delete**. A key in the delta is removed physically; a key in an immutable
  segment gets a tombstone (a copy-on-write set) until a merge drops it.
- **Search** (`leveled_search.go`). The query fans out over delta shards,
  flushing deltas and all segments in parallel; results merge into one top-K.
  A newer copy of a key shadows an older one; with `-strict-seg-shadow` (on by
  default) a hit from an older segment is dropped whenever a newer segment
  holds the key, even if the newer copy did not match the query.
- **Text** (`bm25_*.go`). Each segment carries a BM25 inverted index; the
  statistics are global across segments. Hybrid search runs the lexical and the
  vector arm with the same filters and fuses them by reciprocal rank (k = 60).
- **Attributes and tenants** (`attr*.go`, `tenant.go`). Categorical and numeric
  columns per segment answer `EQ`/`RANGE` filters. With `-partition-attr`,
  segments are laid out tenant-contiguously, and a small tenant is
  brute-forced over its own range instead of traversing the whole graph.

## Durability and recovery

- **WAL** (`internal/wal`). Frames `[CRC32][length][payload]`, an LSN per
  record, a version byte in the file header. Handlers queue records to a batch
  writer (up to 256 records or 1 ms per write); the syncer fsyncs every 100 ms.
  This is Redis `appendfsync everysec`: a crash can lose up to ~100 ms of
  acknowledged writes, a graceful shutdown loses nothing. A failed write or
  fsync latches fail-stop.
- **Terms, not text.** Document records carry tokenized terms, so replay never
  re-tokenizes and reproduces the index bit-exactly whatever the stemmer
  version.
- **Snapshots**. `snapshot.wal` holds strings, TTLs and sorted sets as WAL
  records; `graph_leveled.bin` holds all vector segments with a CRC32 trailer
  and the LSN they cover. Compaction (on `COMPACT` or when the WAL passes 64 MB)
  rotates the WAL, writes both snapshots in the background and then deletes the
  old WAL files — from then on the snapshots are the only copy of older state.
- **Recovery** (`cmd/kvstore/main.go`). Load `graph_leveled.bin` (all or
  nothing; an unreadable file stops startup — see invariants), replay
  `snapshot.wal`, then every `wal_*.log` in order, skipping vector records the
  snapshot already covers. An operation unknown to this version, or a data
  directory written with the removed encryption, also stops startup.
- **Point-in-time restore**. `-restore-to-lsn` replays up to an LSN, serves
  reads only and writes nothing into the data directory; `-wal-inspect` prints
  the journal to find the LSN.
- **Shipping** (`internal/ship`). WAL chunks and snapshots go to S3 or a
  directory with manifests; `-ship-restore` rebuilds a data directory on
  another machine.

## Invariants and their tests

| Invariant | Why | Test |
|---|---|---|
| A vector is added to the index before its WAL record is written | a snapshot watermark taken between the two would otherwise cover a record whose vector the snapshot lacks | `TestWalApplier_HonorsSnapshotWatermark` and `exec_vector_durability_test.go` |
| A snapshot loads all or nothing | replaying the WAL over half-loaded segments gives a silently inconsistent index | `TestSnapshot_LoadBinaryAtomicOnError` |
| An unreadable vector snapshot stops startup | after compaction it is the only copy of older vectors | `TestLoadVectorSnapshot`, soak drill 2 |
| A corrupt count in a snapshot is an error, not an allocation | the CRC is verified at the end, allocations happen on the way | `TestLoadBinaryCorruptCountIsBounded` |
| Snapshots of earlier releases still load | an upgrade must not lose data | `snapshot_compat_test.go` with fixtures written by those releases |
| Writes after `FlushDeltaSync` stay in the delta | otherwise a stale copy surfaces across segments | `TestFlushDeltaSyncDoesNotFlushLaterWrites` |
| A rejected command writes nothing | an error reply must not hide a half-applied change | `TestSetRejectedOptionsWriteNothing` |
| Each connection is closed exactly once | a double decrement breaks the connection limit, a missed one leaks a slot | `TestCloseConn_ExactlyOnce` |
| Equal scores keep their order across B+-tree leaves | range queries descend to the first equal key | `TestRangeEqualScoresAcrossLeaves` |

## Code map

| Path | Responsibility |
|---|---|
| `kvstore/cmd/kvstore/main.go` | flags, startup and recovery, shutdown order |
| `kvstore/cmd/kvstore/dispatch.go` | `commands`: entry, gates, dispatch, transactions |
| `kvstore/cmd/kvstore/cmd_*.go` | command handlers by family |
| `kvstore/cmd/kvstore/wal_applier.go` | applying WAL records on recovery |
| `kvstore/cmd/kvstore/persistence.go` | KV snapshot for compaction, TTL evictor, vector snapshot loading |
| `kvstore/cmd/kvstore/restore.go`, `datadir.go` | point-in-time restore, data-directory checks |
| `kvstore/internal/server` | connections, `ConnBuf`, RESP parsing, lifecycle |
| `kvstore/internal/wal` | WAL, batch writer, syncer, compaction |
| `kvstore/internal/ship` | WAL shipping and restore |
| `kvstore/internal/store/...`, `internal/btree`, `internal/pubsub` | in-memory structures |
| `kvstore/vector/leveled_*.go` | the LSM: state, segments, search, snapshot, compaction |
| `kvstore/vector/delta.go`, `graph.go`, `frozen*.go` | mutable delta, HNSW graph, frozen fp32/SQ8 graphs |
| `kvstore/vector/bm25_*.go`, `attr*.go`, `tenant.go` | text, attribute columns, tenant layout |
| `kvstore/internal/cluster` | cluster mode, `experimental` build tag only |
