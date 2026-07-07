# KVStore

Single-node in-memory **vector search engine** (HNSW) speaking the RESP protocol,
with a small frozen KV/TTL/Pub-Sub payload layer around it, WAL-based durability
with continuous shipping to S3, and built-in RAG via Ollama. Not a Redis
replacement — the KV surface exists to serve the vector core.

## Features

- **Vector Search (HNSW)** — the core: arena-based graph, SQ8 quantization, tenant/attribute filtering, bitset visited, DotProduct optimization
- **AI / RAG** — Ollama embeddings, async ingestion, semantic queries (`AI.INGEST` / `AI.ASK`)
- **WAL + Snapshots** — CRC32-protected, batch writes, crash recovery
- **WAL-shipping** — continuous async replication of WAL+snapshots to S3/MinIO or a mounted dir (Litestream-style); restore on any machine with `-ship-restore` (see [docs/BACKUP.md](docs/BACKUP.md))
- **TCMalloc-style allocator** — per-worker MCache, lock-free GET, zero GC pressure
- **Epoll networking** — per-worker event loops, zero-alloc RESP parser, greedy drain
- **TTL** — 256-shard heap with lazy + active expiration
- **Pub/Sub** — back-pressure, sync.Pool, per-subscriber goroutines
- **AUTH + TLS/mTLS** — constant-time password auth, encrypted connections, optional client-cert verification
- **Observability** — Prometheus `/metrics`, `/health`, `/ready`; structured logging (slog, text/JSON); Grafana stack in `docker-compose.yml`
- **WASM Compute** *(experimental, behind a build tag, not in the default build)* — Reactor pattern (worker-local slots) + Command modules
- **Cluster** *(experimental, behind a build tag, not in the default build)* — hash-slot sharding, gossip protocol, live migration

The RESP command surface is deliberately small and **frozen**: the KV/state layer
is a payload layer for the vector engine, not a Redis replacement. The full
command manifest, gate semantics, and the unfreeze policy live in
[docs/COMMANDS.md](docs/COMMANDS.md).

## Quick Start

### Docker Compose (recommended)

Full stack — kvstore + Ollama + automatic model download:

```bash
git clone https://github.com/Nikolay1994Kaz/storage_in_memory.git
cd storage_in_memory
docker compose up -d --build     # or: make up
```

`--build` rebuilds the image from the current sources, so a `git pull` never
leaves you on a cached binary that lacks newer commands. That's it — KVStore is
on `localhost:6380`, Ollama on `localhost:11434`.

```bash
redis-cli -p 6380
> SET hello world
> AI.INGEST doc:1 "Go is a statically typed language"
> AI.ASK "What is Go?"
```

Then take the guided tour — real embeddings, tenant/attribute filtering, one
short Go file: [`kvstore/examples/quickstart`](kvstore/examples/quickstart/).

### Prebuilt binaries

Static Linux binaries (amd64/arm64, no dependencies) are published on the
[Releases page](https://github.com/Nikolay1994Kaz/storage_in_memory/releases):

```bash
tar xzf kvstore-server_*_linux_amd64.tar.gz
./kvstore-server --port 6380
```

Linux only — the network layer is built on epoll.

### Build from source

```bash
make build
./kvstore-server --port 6380

# With AUTH
./kvstore-server --port 6380 --requirepass "s3cret"

# With AUTH + TLS
./kvstore-server --port 6380 --requirepass "s3cret" \
  --tls-cert cert.pem --tls-key key.pem

# Accept remote connections (localhost-only by default; requires AUTH in practice)
./kvstore-server --bind 0.0.0.0 --port 6380 --requirepass-file /etc/kvstore/pass
```

## CLI Flags

The most commonly used flags (run `./kvstore-server --help` for the full list,
including HNSW tuning `--hnsw-*`, `--partition-attr`, cluster slots, etc.):

| Flag | Default | Description |
|---|---|---|
| `--bind` | `127.0.0.1` | Listen interface for both the data port and the metrics port. Localhost-only by default — to accept remote connections set `--bind 0.0.0.0` **and configure AUTH (+TLS)** |
| `--port` | `6380` | Listen port |
| `--metrics-port` | `9090` | HTTP port for `/metrics`, `/health`, `/ready` |
| `--data-dir` | `data` | Directory for WAL and snapshots (a relative path is resolved against the working directory) |
| `--maxmemory` | `0` | Memory limit in MB (0 = unlimited); writes are rejected above the limit |
| `--max-connections` | `10000` | Cap on concurrent connections (0 = unlimited) |
| `--idle-timeout` | `5m` | Close connections idle longer than this (pub/sub subscribers are exempt); 0 = off |
| `--write-timeout` | `30s` | Max time to flush a response to a slow reader; 0 = off |
| `--requirepass` / `--requirepass-file` | `""` | AUTH password inline or from a file (empty = no auth) |
| `--tls-cert` / `--tls-key` | `""` | TLS certificate and private key (PEM) |
| `--tls-client-ca` | `""` | CA for client-certificate verification (mTLS) |
| `--ollama-url` | `http://localhost:11434` | Ollama API URL |
| `--ship-url` | `""` | Continuous WAL-shipping target: `file:///path` or `s3://bucket/prefix?endpoint=...` (creds via env, see [docs/BACKUP.md](docs/BACKUP.md)) |
| `--ship-interval` | `1s` | Shipping period (≈ crash RPO) |
| `--ship-retain` | `3` | Restore points kept on the remote |
| `--ship-restore` | `false` | Restore data dir from `--ship-url` before start |
| `--log-level` / `--log-format` | `info` / `text` | Structured logging level and format (`text`/`json`) |
| `--pprof` | `false` | Expose `/debug/pprof/*` on the metrics port — **never in production** |

## Supported Commands

The full command manifest (syntax, replies, gate semantics, WAL ops) lives in
[docs/COMMANDS.md](docs/COMMANDS.md) — the surface is deliberately small and frozen.
Families: KV/TTL (`SET`/`GET`/`DEL`/`EXPIRE`/…), transactions (`MULTI`/`EXEC`/`DISCARD`),
Pub/Sub, sorted sets (`ZADD`/…), vector search (`VSIM.*`), AI/RAG (`AI.*`).

> **Isolation contract (read this).** `MULTI`/`EXEC` provides command **grouping and
> pipelining** with EXECABORT on a bad queued command — it does **not** provide
> Redis-grade isolation. Unlike single-threaded Redis, this engine executes commands
> across per-worker shards, and the transaction lock only serializes `EXEC`-vs-`EXEC`.
> A plain command from another connection (e.g. `SET`) can interleave *between* the
> queued commands of an in-flight `EXEC`, so the queue is **not** isolated from
> concurrent traffic. Do not rely on `EXEC` for atomic read-modify-write against keys
> that other clients touch concurrently. This is a deliberate design choice: true
> isolation would require globally serializing every write for the duration of each
> `EXEC`, which defeats the per-worker, zero-alloc model the engine is built on.
> Durability (WAL + fsync) is unaffected and holds.

## Architecture

```
┌──────────────────────────────────────────────┐
│                  Clients                     │
│              (redis-cli, etc.)               │
└──────────────┬───────────────────────────────┘
               │ TCP / TLS
┌──────────────▼───────────────────────────────┐
│          Epoll Server (per-worker)           │
│     ConnBuf (ring buffer, zero-alloc)        │
├──────────────────────────────────────────────┤
│              AUTH Guard                      │
├──────────────┬───────────────────────────────┤
│   TCMalloc   │  TTL    │ WAL    │ Pub/Sub   │
│   Store      │  Heap   │ Batch  │ Hub       │
├──────────────┼─────────┼────────┼───────────┤
│  HNSW Vector │  WASM   │   AI   │  Cluster  │
│  Search      │  Engine │ Worker │  (gossip) │
└──────────────┴─────────┴────────┴───────────┘
```

## Benchmarks

Measured on standard ANN datasets and real OpenAI embeddings, including
same-machine head-to-head runs against hnswlib — full tables, methodology and
honest caveats in [docs/BENCHMARKS.md](docs/BENCHMARKS.md). Headlines:

- **High-dim (GIST-960, target path):** SQ8 beats hnswlib float32 **2.5–2.7×**
  in multithreaded QPS at equal recall, with 4× less vector memory.
- **Real embeddings (dbpedia ada-002, 1536-dim):** recall@10 0.977 (SQ8) /
  0.984 (fp32); SQ8 gives ~2.3× QPS and 3.7× less memory.
- **Multi-tenant filtered search:** tenant-routed queries are **5.8×–28 620×**
  faster than post-filtering a full graph traversal, at recall 1.0.
- **Low-dim float32 (SIFT-1M):** hnswlib is ~3× faster — the honest cost of an
  LSM design that supports concurrent writes, deletes and crash recovery.

## Status & Scope

- **Single-node by design.** Durability and disaster recovery come from WAL +
  snapshots + continuous WAL-shipping (restore on any machine), not from
  replicas. Cluster mode exists behind a build tag and is not production-ready.
- **ANN search is approximate.** HNSW recall is high (≈0.98 on real embedding
  datasets at default settings) but not 1.0; under heavy churn a small fraction
  of stored vectors may temporarily miss from top-K results. `VSIM.EXISTS`
  gives an exact existence check.
- **Experimental gates.** WASM compute and cluster mode are compiled out of the
  default build (`experimental` build tag) — their surfaces are not hardened to
  the same bar as the core.
- **Validated by soak testing**: multi-hour runs at ~6k RPS mixed load with
  graceful/crash restarts, ship-restore and corrupted-snapshot drills.

## WASM Modules

> WASM compute is **experimental** and excluded from the default build — compile
> the server with the `experimental` build tag to enable it.

Pre-compiled example modules are in `kvstore/examples/fraud_scorer/`.

To compile from source (requires [TinyGo](https://tinygo.org/getting-started/install/)):

```bash
# Command module
tinygo build -o fraud_scorer.wasm -target=wasi ./kvstore/examples/fraud_scorer/

# Reactor module (zero-alloc hot path)
tinygo build -o fraud_scorer_reactor.wasm -target=wasi -buildmode=c-shared \
  ./kvstore/examples/fraud_scorer/
```

## Testing

```bash
go test -short ./...   # Fast run — heavy benchmarks/soak tests are gated off (~30s)
make test              # Run all tests
make bench             # Run benchmarks
make vet               # Static analysis
```

## Project Structure

```
kvstore/
├── cmd/kvstore/          # Entry point (main.go)
├── internal/
│   ├── server/           # Epoll server, ConnBuf, Handler
│   ├── store/            # KV store, TTL manager
│   │   └── tcmalloc/     # TCMalloc-style allocator (MCache, MCentral, MHeap)
│   ├── wal/              # Write-Ahead Log, snapshots, batch writer
│   ├── ship/             # Continuous WAL-shipping (file://, s3://) + restore
│   ├── pubsub/           # Pub/Sub hub (classic + semantic)
│   ├── btree/            # Sorted-set backing structure
│   ├── compute/          # WASM engine (wazero), triggers, worker slots
│   ├── ai/               # Ollama client, async worker
│   ├── cluster/          # Hash slots, gossip, replication (experimental)
│   ├── logging/          # slog setup (levels, text/JSON)
│   ├── monitoring/       # Prometheus-style metrics
│   └── protocol/         # RESP parser and writer
├── vector/               # HNSW graph, SQ8, tenant/attr filtering, arena allocator
└── examples/             # WASM module examples
docs/                     # Command manifest, backup guide, benchmarks
monitoring/               # Grafana/VictoriaMetrics provisioning for the local stack
scripts/                  # Soak-test harness
```

## License

See [LICENSE](LICENSE).
