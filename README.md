# KVStore

[![CI](https://github.com/Nikolay1994Kaz/storage_in_memory/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/Nikolay1994Kaz/storage_in_memory/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/Nikolay1994Kaz/storage_in_memory)](https://github.com/Nikolay1994Kaz/storage_in_memory/releases)

**A single-node in-memory data store that speaks RESP and does vector and
full-text search in the same process.** Vectors live in HNSW indexes organised
as an LSM of immutable segments (fp32 or SQ8); BM25 full-text sits next to them,
with reciprocal-rank fusion for hybrid queries; attribute filters and a
tenant-aware layout narrow the search. Everything is durable through a
write-ahead log and snapshots and can be shipped continuously to S3. A small
key-value / TTL / sorted-set / pub-sub layer rides along for metadata and joins.
One static binary, no external services.

## What is inside

- **Vector search** — HNSW in an LSM of segments. Writes land in a sharded
  in-memory delta; a full delta freezes into an immutable segment in
  milliseconds (no graph rebuild on the flush path); background merges
  consolidate levels; after a write lull the index consolidates back into a
  single segment by itself (`-idle-consolidate`). Segments are fp32 or SQ8
  (int8 codes, asymmetric distance and rerank). Deletes are tombstones until
  the next merge.
- **Full-text and hybrid** — a BM25 inverted index per segment
  (`VSIM.SEARCHTEXT`) and BM25+vector fusion by reciprocal rank
  (`VSIM.HYBRID`), with filters applied to both arms *before* fusion. The WAL
  carries tokenized terms, not raw text, so replay is bit-exact whatever the
  stemmer version.
- **Filters and tenants** — categorical and numeric attribute columns per
  segment (`VSIM.FILTER`, `EQ`/`RANGE`), and a tenant-contiguous layout
  (`-partition-attr`) that brute-forces a small tenant over its own range
  instead of traversing the whole graph.
- **Durability** — WAL with LSNs and group commit; fail-stop on I/O errors
  (writes are refused, reads keep working); CRC-checked snapshots with an LSN
  watermark, so replay skips what a snapshot already holds; read-only
  point-in-time restore (`-restore-to-lsn`); continuous WAL shipping to S3 or a
  directory, restorable on any machine.
- **KV layer** — strings with TTL, sorted sets on a B+-tree, pub/sub on named
  channels, `MULTI`/`EXEC` grouping. It exists
  for metadata and cross-engine joins (`VSIM.SEARCHFILTER`,
  `VSIM.SEARCHRANGE`); the surface is small and frozen on purpose — this is not
  a Redis replacement ([docs/COMMANDS.md](docs/COMMANDS.md)).
- **Server** — RESP over TCP, TLS or mTLS; `AUTH`; a goroutine per connection
  on Go's netpoller with a zero-alloc parser and one write per pipelined batch;
  a `maxmemory` gate; Prometheus `/metrics`, `/health`, `/ready`; structured
  logs.

## Numbers

Laptop i7-9750H (6 cores), same machine for both sides of every comparison.
Methodology, caveats and reproduction commands are in
[docs/BENCHMARKS.md](docs/BENCHMARKS.md).

| Workload | Result |
|---|---|
| End-to-end over RESP, MNIST-784, SQ8 | **12 985 QPS** at recall@10 0.9996 |
| End-to-end over RESP, dbpedia ada-002 (1536-dim) | **3 928 QPS** at recall@10 0.9888 |
| GIST-960, SQ8 vs hnswlib fp32 | **2.5–2.7×** multithreaded QPS at equal recall, 4× less vector memory |
| SIFT-1M, consolidated index vs hnswlib | **1.36–1.38×** (25 697 vs 18 833 QPS at recall 0.96) |
| SIFT-1M, fragmented index right after a load | hnswlib ~3× faster — the price of an LSM that takes concurrent writes and deletes |
| Tenant-routed filtered search | **5.8×–28 620×** faster than post-filtering, recall 1.0 |
| Search while 12 clients bulk-load dbpedia | **750–800 QPS** floor, 3 544 QPS at 0.99 after the load |
| KV `GET`, store level | 49 ns, zero allocations |

## Measured decisions

The parts of the design that were settled by measurement rather than by taste:

- **No custom allocator, no own epoll.** Both were built: a TCMalloc-style arena
  for the KV store and HNSW neighbour lists, and an epoll layer on top of Go's
  netpoller. An A/B against a sharded `map` and goroutine-per-connection showed
  the arena made `GET` 3× slower (with two allocations per call), and the epoll
  layer gave nothing back. Both were removed: with ~950k live keys p99 `GET`
  went from 1.04 to 0.59 ms, p99 `SET` from 1.33 to 0.76 ms, RSS from ~500 to
  ~368 MB ([BENCHMARKS §7](docs/BENCHMARKS.md#7-removing-the-custom-allocator-and-the-epoll-layer)).
- **Flush without rebuild.** Freezing a delta shard into a segment instead of
  rebuilding a graph, plus merging whole level prefixes at once, took the search
  floor during bulk loads from ~34 QPS to 750–800 QPS.
- **The LSM cost is stated, not hidden.** A freshly loaded, fragmented index is
  slower than a single hnswlib graph; idle consolidation returns it to the
  faster shape.

## Quick start

### One binary

Static Linux binaries (amd64/arm64) are published on the
[Releases page](https://github.com/Nikolay1994Kaz/storage_in_memory/releases):

```bash
sha256sum --ignore-missing -c checksums.txt
tar xzf kvstore-server_*_linux_amd64.tar.gz
./kvstore-server --port 6380
```

The code also compiles for macOS but is only tested on Linux.

The engine is **BYO-embeddings**: it takes pre-computed vectors over RESP from
any embedding provider (OpenAI, Ollama, sentence-transformers, …):

```bash
redis-cli -p 6380
> VSIM.ADD doc:1 0.12 0.90 0.31
> VSIM.ADD doc:2 0.85 0.05 0.48
> VSIM.SEARCH 2 0.10 0.88 0.30
> VSIM.ADDDOC doc:3 TEXT "vacuum and table bloat" CAT lang en VEC 0.20 0.70 0.40
> VSIM.ADDDOC doc:4 TEXT "graph deletes and tombstones" CAT lang en VEC 0.80 0.10 0.40
> VSIM.SEARCHTEXT 5 tombstones
> VSIM.HYBRID 5 TEXT "graph deletes" EQ lang en VEC 0.80 0.10 0.40
> SET hello world
```

### Docker Compose — server + metrics

```bash
git clone https://github.com/Nikolay1994Kaz/storage_in_memory.git
cd storage_in_memory
docker compose up -d --build     # or: make up
```

KVStore is on `localhost:6380`, Grafana on `localhost:3000`. A guided tour with
real embeddings computed on the client side and tenant filtering, in one short
Go file: [`kvstore/examples/quickstart`](kvstore/examples/quickstart/).

### Build from source

```bash
make build          # -> ./kvstore-server
./kvstore-server --port 6380

# With AUTH
./kvstore-server --port 6380 --requirepass-file /etc/kvstore/pass

# With AUTH + TLS
./kvstore-server --port 6380 --requirepass-file /etc/kvstore/pass \
  --tls-cert cert.pem --tls-key key.pem

# Accept remote connections (localhost-only by default; requires AUTH in practice)
./kvstore-server --bind 0.0.0.0 --port 6380 --requirepass-file /etc/kvstore/pass
```

## CLI flags

The most commonly used flags (`./kvstore-server --help` lists all of them):

| Flag | Default | Description |
|---|---|---|
| `--bind` | `127.0.0.1` | Listen interface for the data port and the metrics port. Localhost-only by default — to accept remote connections set `--bind 0.0.0.0` **and configure AUTH (+TLS)** |
| `--port` | `6380` | Listen port |
| `--metrics-port` | `9090` | HTTP port for `/metrics`, `/health`, `/ready` (0 = off) |
| `--data-dir` | `data` | Directory for the WAL and snapshots |
| `--maxmemory` | `0` | Memory limit in MB (0 = unlimited); memory-growing writes are rejected above it |
| `--hnsw-use-sq` | `false` | SQ8 quantization of frozen segments: 4× less vector memory, higher QPS |
| `--partition-attr` | `""` | Categorical attribute that defines tenants for the tenant-contiguous layout |
| `--idle-consolidate` | `60s` | Write lull after which the index consolidates into one segment (0 = off) |
| `--max-connections` | `10000` | Cap on concurrent connections (0 = unlimited) |
| `--idle-timeout` | `5m` | Close connections idle longer than this (pub/sub subscribers are exempt); 0 = off |
| `--write-timeout` | `30s` | Max time to flush a response to a slow reader; 0 = off |
| `--requirepass` / `--requirepass-file` | `""` | AUTH password inline or from a file (empty = no auth) |
| `--tls-cert` / `--tls-key` | `""` | TLS certificate and private key (PEM) |
| `--tls-client-ca` | `""` | CA for client-certificate verification (mTLS) |
| `--ship-url` | `""` | Continuous WAL shipping: `file:///path` or `s3://bucket/prefix?endpoint=...` (credentials via env, see [docs/BACKUP.md](docs/BACKUP.md)) |
| `--ship-interval` | `1s` | Shipping period (≈ crash RPO) |
| `--ship-retain` | `3` | Restore points kept on the remote |
| `--ship-restore` | `false` | Restore the data directory from `--ship-url` before start |
| `--restore-to-lsn` | `0` | Start as of this LSN and serve **reads only**; the data directory is not modified (0 = normal start) |
| `--wal-inspect` | `false` | Print the journal (LSN, op, key) and exit — how you find the LSN for `--restore-to-lsn` |
| `--log-level` / `--log-format` | `info` / `text` | Logging level and format (`text`/`json`) |
| `--pprof` | `false` | Expose `/debug/pprof/*` on the metrics port — **never in production** |

## Commands

The full manifest (syntax, replies, gate semantics, WAL ops) is in
[docs/COMMANDS.md](docs/COMMANDS.md). Families: vector and full-text search
(`VSIM.*`), KV/TTL (`SET`/`GET`/`DEL`/`EXPIRE`/…), sorted sets (`ZADD`/…),
pub/sub, transactions (`MULTI`/`EXEC`/`DISCARD`), admin (`COMPACT`).

> **Isolation contract.** `MULTI`/`EXEC` gives grouping and pipelining with
> `EXECABORT` on a bad queued command — not Redis-grade isolation. Commands run
> concurrently, and the transaction lock only serializes `EXEC` against `EXEC`,
> so a plain command from another connection can interleave between the queued
> commands of an in-flight `EXEC`. Do not rely on `EXEC` for atomic
> read-modify-write on keys other clients touch. True isolation would mean
> serializing every write for the duration of each `EXEC`. Durability (WAL +
> fsync) is unaffected.

## Architecture

```
┌───────────────────────────────────────────────┐
│        Clients (redis-cli, any RESP lib)      │
└───────────────┬───────────────────────────────┘
                │ TCP / TLS
┌───────────────▼───────────────────────────────┐
│ Server: goroutine per connection, ConnBuf     │
│ AUTH · maxmemory gate · WAL fail-stop gate    │
├────────────┬─────────┬──────────┬─────────────┤
│ KV store   │ TTL     │ Sorted   │ Pub/Sub     │
│ (shard map)│ (heaps) │ sets     │ hub         │
│            │         │ (B+tree) │             │
├────────────┴─────────┴──────────┴─────────────┤
│ Vector + text engine: LSM of segments         │
│ delta (sharded HNSW) → frozen fp32 / SQ8 /    │
│ HNSW segments → merges, idle consolidation    │
│ BM25 per segment · attribute columns · tenants│
├───────────────────────────────────────────────┤
│ WAL (LSN, group commit) · snapshots (CRC)     │
│ WAL shipping → S3 / directory                 │
└───────────────────────────────────────────────┘
```

## Status and scope

- **Single node by design.** Durability and disaster recovery come from the WAL,
  snapshots and continuous WAL shipping, not from replicas. A cluster mode
  exists behind the `experimental` build tag; it is compiled out of the default
  build and not production-ready.
- **ANN search is approximate.** Recall is high (0.99 end-to-end on real
  embedding datasets at default settings) but not 1.0; under heavy churn a small
  fraction of stored vectors can temporarily miss from top-K. `VSIM.EXISTS` is
  the exact membership check.
- **No encryption at rest and no secure erasure.** Deleted data can outlive the
  delete in segments until the next merge, in the WAL and in shipped archives;
  use disk encryption if the data directory needs it ([SECURITY.md](SECURITY.md)).
- **Validated by soak testing**: multi-hour runs at ~6k RPS mixed load with
  graceful and crash restarts, ship-restore and corrupted-snapshot drills.

## Testing

```bash
make test              # The gate: exactly what CI runs (-short, ~1 min)
make test-race         # Race detector over the subsystems, as in ci.yml
make test-full         # Everything, no -short: search-quality validation + 500k stress
make test-datasets     # Recall/QPS validation on real datasets (needs the files)
make bench             # Benchmarks
make vet               # Static analysis
```

`make test-full` is **not** a gate and CI never runs it: it takes tens of minutes
and exists to exercise scale (500k vectors) and search quality on real datasets.
Read a failure there as a *measurement* first — re-check it on an idle machine
before concluding the code broke.

Snapshot compatibility is pinned by fixtures written by earlier releases
(`kvstore/vector/testdata/snapshots`): a new reader must load what an old writer
produced ([docs/FORMAT_COMPAT.md](docs/FORMAT_COMPAT.md)).

## Project structure

```
kvstore/
├── cmd/kvstore/          # Server: flags, startup and recovery, command dispatch
├── cmd/soak_oracle/      # Durability oracle for soak runs
├── cmd/insertload/       # Ingest load tool (raw float32 files)
├── cmd/searchload/       # Search load tool
├── cmd/stress_test/      # Mixed-load stress client
├── internal/
│   ├── server/           # TCP/TLS server, ConnBuf, connection lifecycle
│   ├── protocol/         # RESP parser and writer
│   ├── store/            # TTL manager
│   │   ├── kv/           # KV store (sharded map, maxmemory accounting)
│   │   └── zset/         # Sorted-set registry
│   ├── btree/            # B+-tree backing sorted sets
│   ├── wal/              # Write-ahead log, snapshots, batch writer
│   ├── ship/             # Continuous WAL shipping (file://, s3://) + restore
│   ├── pubsub/           # Pub/Sub hub
│   ├── cluster/          # Hash slots, gossip, replication (experimental)
│   ├── monitoring/       # Metrics
│   └── logging/          # slog setup
├── vector/               # LSM of HNSW segments, SQ8, BM25, filters, tenants
└── examples/quickstart/  # End-to-end tour over RESP with client-side embeddings
docs/                     # Commands, benchmarks, backup, format compatibility, BM25 design
monitoring/               # Grafana/VictoriaMetrics provisioning for the local stack
scripts/                  # Soak harness, A/B harness, backup/restore, dataset converters
```

## Commercial use

MIT — use it in a commercial product without asking. If you need something the
licence does not cover — support with a response time, an integration built to
your requirements, a deployment in a closed network, or terms other than MIT —
write to dubovoinikolai@gmail.com. The copyright is held by one person, so dual
licensing is a conversation, not a legal project.

## License

See [LICENSE](LICENSE).
