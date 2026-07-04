# KVStore

High-performance in-memory key-value store with built-in vector search, WASM compute, and AI integration.

## Features

- **TCMalloc-style allocator** — per-worker MCache, lock-free GET, zero GC pressure
- **Epoll networking** — per-worker event loops, zero-alloc RESP parser, greedy drain
- **WAL + Snapshots** — CRC32-protected, batch writes, crash recovery
- **WAL-shipping** — continuous async replication of WAL+snapshots to S3/MinIO or a mounted dir (Litestream-style); restore on any machine with `-ship-restore` (see [docs/BACKUP.md](docs/BACKUP.md))
- **TTL** — 256-shard heap with lazy + active expiration
- **Pub/Sub** — back-pressure, sync.Pool, per-subscriber goroutines
- **Vector Search (HNSW)** — arena-based graph, bitset visited, DotProduct optimization
- **WASM Compute** — Reactor pattern (worker-local slots) + Command modules
- **AI Integration** — Ollama embeddings, async ingestion, RAG queries
- **AUTH + TLS** — optional password authentication and encrypted connections
- **Cluster** — hash-slot sharding, gossip protocol, live migration (experimental)

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
docker compose up -d     # or: make up
```

That's it. KVStore is on `localhost:6380`, Ollama on `localhost:11434`.

```bash
redis-cli -p 6380
> SET hello world
> AI.INGEST doc:1 "Go is a statically typed language"
> AI.ASK "What is Go?"
```

### Build from source

```bash
make build
./kvstore-server --port 6380

# With AUTH
./kvstore-server --port 6380 --requirepass "s3cret"

# With AUTH + TLS
./kvstore-server --port 6380 --requirepass "s3cret" \
  --tls-cert cert.pem --tls-key key.pem
```

## CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--port` | `6380` | Listen port |
| `--maxmemory` | `0` | Memory limit in MB (0 = unlimited) |
| `--requirepass` | `""` | AUTH password (empty = no auth) |
| `--tls-cert` | `""` | Path to TLS certificate (PEM) |
| `--tls-key` | `""` | Path to TLS private key (PEM) |
| `--ollama-url` | `http://localhost:11434` | Ollama API URL |
| `--ship-url` | `""` | Continuous WAL-shipping target: `file:///path` or `s3://bucket/prefix?endpoint=...` (creds via env, see [docs/BACKUP.md](docs/BACKUP.md)) |
| `--ship-interval` | `1s` | Shipping period (≈ crash RPO) |
| `--ship-retain` | `3` | Restore points kept on the remote |
| `--ship-restore` | `false` | Restore data dir from `--ship-url` before start |
| `--cluster` | `false` | Enable cluster mode |
| `--slot-start` | `0` | Cluster slot range start |
| `--slot-end` | `16383` | Cluster slot range end |

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

## WASM Modules

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
make test         # Run all tests
make bench        # Run benchmarks
make vet          # Static analysis
```

## Project Structure

```
kvstore/
├── cmd/kvstore/          # Entry point (main.go)
├── internal/
│   ├── server/           # Epoll server, ConnBuf, Handler
│   ├── store/            # TTL manager
│   │   └── tcmalloc/     # TCMalloc-style allocator (MCache, MCentral, MHeap)
│   ├── wal/              # Write-Ahead Log, snapshots, batch writer
│   ├── pubsub/           # Pub/Sub hub
│   ├── compute/          # WASM engine (wazero), triggers, worker slots
│   ├── ai/               # Ollama client, async worker
│   ├── cluster/          # Hash slots, gossip, replication
│   └── protocol/         # RESP parser and writer
├── vector/               # HNSW graph, distance functions, arena allocator
├── examples/             # WASM module examples
└── docs/                 # Benchmarks, optimization logs
```

## License

See [LICENSE](LICENSE).
