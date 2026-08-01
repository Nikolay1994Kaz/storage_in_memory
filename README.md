# KVStore

[![CI](https://github.com/Nikolay1994Kaz/storage_in_memory/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/Nikolay1994Kaz/storage_in_memory/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/Nikolay1994Kaz/storage_in_memory)](https://github.com/Nikolay1994Kaz/storage_in_memory/releases)

**Self-hosted memory engine for AI agents.** One process gives your agents
durable, queryable memory: facts with validity intervals and supersession
history ("what was true in March?"), erasure at two levels — immediate
revocation with its physical horizon stated, and cryptographic erasure of a
whole scope that reaches copies already shipped to an archive — an optional
tamper-evident audit chain over every memory-changing command, and
recency×importance-ranked recall — BM25 out of the box, hybrid BM25+vector
when you bring embeddings. Agents plug in over **MCP** in two minutes
([docs/QUICKSTART_MCP.md](docs/QUICKSTART_MCP.md)); everything is also
scriptable over **RESP** from any Redis client library.

Under the memory surface sits a single-node in-memory engine built for the
job: HNSW vector search with SQ8 quantization, BM25 full-text with RRF hybrid
fusion, tenant/attribute filtering, a small frozen KV/TTL/Pub-Sub layer, and
WAL-based durability with continuous shipping to S3. One static ~13MB binary —
no Postgres+Qdrant+Neo4j zoo — and self-hosted by design: your agents' memory
runs where your data must live (laptop, VPC, air-gapped on-prem). Not a Redis
replacement — the KV surface exists to serve the memory core.

## Agent memory in two minutes

```bash
# 1. the server — one static binary; the working dir keeps WAL + snapshots
./kvstore-server --port 6380

# 2. the MCP adapter — plug into Claude Code (or any MCP host)
claude mcp add memory -- vmem-mcp -addr 127.0.0.1:6380 -default-scope myproject
```

The agent now has three tools — `memory_remember`, `memory_recall`,
`memory_forget` — and its facts survive restarts, sessions and model
switches. Full setup incl. Claude Desktop and Docker:
[docs/QUICKSTART_MCP.md](docs/QUICKSTART_MCP.md); agent-driven install:
[INSTALL_FOR_AGENTS.md](INSTALL_FOR_AGENTS.md).

What makes it a memory engine rather than a search index (`VMEM.*` —
semantics in [docs/VMEM_DESIGN.md](docs/VMEM_DESIGN.md)):

- **Validity time.** A fact that replaces another (`supersedes`) closes the
  old one's interval instead of deleting it; `RECALL … ASOF <ts>` answers
  "what was true then".
- **Erasure beats time travel.** `FORGET` and TTL expiry make a fact
  unreachable in every read mode, `ASOF` included — the right to be forgotten
  wins over the time machine. That is *revocation*: the bytes leave the main
  store at the next consolidation and can outlive the call in the WAL, in
  earlier snapshots and in shipped archives.
- **Cryptographic erasure, for the stronger claim.** With `-encrypt-at-rest`
  facts are sealed under a per-scope key at the persistence boundary, so
  `VMEM.SHRED` destroying that key makes the journal, the snapshots and **the
  archives already shipped to S3 unreadable at once** — including a
  point-in-time restore to before the call. This is the thing deletion cannot
  do in principle: it never catches copies that already left. It is
  scope-granular (a scope *is* whose memory it is), it cannot reach facts
  written before the keyring existed (`VMEM.RESEAL` migrates those forward,
  `VMEM.COVERAGE` measures what is genuinely covered), and the receipt claims
  only what is checkable — *this key id was destroyed*, never "the data is
  gone". Both horizons are stated, not glossed —
  [docs/VMEM_DESIGN.md](docs/VMEM_DESIGN.md) ("Erasure guarantee").
- **Evidence, not just state.** With `-audit-chain` every memory-changing
  command leaves a record in a hash-chained, Merkle-batched journal — a
  fingerprint, never the content, since putting fact text in an append-only
  log would keep alive exactly what `SHRED` destroys. `VMEM.AUDIT EXPORT`
  signs the head with **Ed25519**, so an auditor verifies without holding your
  secret; `PROVE` gives an inclusion proof for one event without disclosing
  the others; `RECONCILE` compares live memory against the journal and names
  what disagrees (`resurrected`, `missing`, `unrecorded`).
- **Ranking is not truth.** Recency decay and importance reorder results but
  are mathematically floored so old facts stay reachable — judged on real
  embeddings, not vibes ([docs/BENCHMARKS.md §7](docs/BENCHMARKS.md)).
- **Verbatim anchor.** Recall returns the original stored text, never a lossy
  rewrite; every derived structure is recomputable from the anchors.

### Recovery after memory corruption

Prevention has a ceiling: a plausible false statement arriving from a
compromised channel is, at write time, indistinguishable from a new fact —
both are simply a contradiction with the past. So the interesting question is
not "can it be blocked" but "what does it cost to undo once it is in". These
four primitives answer that:

- **Provenance** (`SOURCE`) — every fact records where it came from. In the
  MCP adapter the *adapter* stamps it, never the agent: an attacker who can
  influence the agent must not be able to sign a fact with someone else's
  origin.
- **Localisation** (`VMEM.EXPLAIN`) — corruption shows up as a wrong *answer*,
  while revocation works on *provenance*. `EXPLAIN` decomposes the ranking of
  a query — which facts produced this answer, from which sources, and which
  axis dropped the ones that are missing. It does not recompute the score: the
  trace comes from the live recall path, so the explanation cannot drift from
  the ranking it explains.
- **Selective revocation** (`VMEM.QUARANTINE`) — revoke everything one origin
  wrote. The facts stay: `ASOF` before the revocation still returns them (what
  the agent believed is evidence, not noise), `ALL` always does.
- **Point-in-time restore** (`-restore-to-lsn`) — raise the whole store as of
  a past LSN, read-only, without touching the data directory.

Measured on one incident, same data, four strategies — two of them executed by
the real [OWASP Agent Memory Guard](https://owasp.org/www-project-agent-memory-guard/)
(`scripts/poison_recovery_compare.py`; `docker compose -f docker-compose.recovery.yml run --rm recovery`):

| | Guard `rollback()` | Guard `retire_if()` | our `-restore-to-lsn` | our `QUARANTINE` |
|---|---|---|---|---|
| lie no longer served | yes | yes | yes | yes |
| lawful facts written **after** the poison | **0/4** | **4/4** | **0/4** | **4/4** |
| revoked fact still queryable as evidence | no | no | no | **yes** |
| `ASOF` before the revocation returns it | no | no | no | **yes** |

Read the second row as an argument about *axes*, not effort: rolling back a
linear log cannot separate a lie from the truth written next to it, because on
that axis the distinction does not exist. Both whole-store rollbacks land on
the same number, in two independent implementations. The third and fourth rows
are what actually remains ours — a layer that owns no storage can delete
selectively (Guard's `retire_if` does, and it loses nothing), but it cannot
keep the revoked fact as queryable evidence, and it has no time axis to answer
"what did the agent believe at 14:32".

**Honesty about coverage.** Both recovery levers select on something a fact
must actually carry, so `VMEM.COVERAGE` reports two independent axes — and a
scope can be fully revocable while not being erasable at all. Revocation
selects **by origin**: a fact written before provenance existed carries no
`source` column and mass revocation matches nothing (`VMEM.BACKFILL` stamps
those forward — its one predicate is exactly "attribute absent" — and it never
overwrites a source someone already declared). Crypto-erasure reaches only
what went to disk **under an envelope**, which is measured by a stamp taken at
write time rather than by asking the keyring whether a key exists now: a scope
half-written before encryption would otherwise report as covered, an error in
our own favour.

We ran it against our own personal store and it reported **0 on both axes** —
every fact predated both features. The honest repair was not a migration: a
`RESEAL` cannot reach copies that already left, so the plaintext sitting in
old WAL segments would have stayed readable whatever the coverage report said
afterwards. The store was rebuilt from scratch under both flags instead. That
is the shape of the limit, stated rather than left to be discovered.

Measured (reproducible canon, §7): known-item hit@1 **0.982** / MRR **0.991**;
temporal accuracy (`ASOF`, supersession chains) **1.000**; scope isolation
**0 violations**; end-to-end `RECALL` p99 **0.29 ms** at **64 426 QPS** over
RESP on a 2019 laptop.

## Features

- **VMEM agent memory** — `VMEM.REMEMBER` / `VMEM.RECALL` / `VMEM.FORGET`: validity intervals + supersession history (`ASOF` time travel), TTL + erasure (immediate unreachability, physical horizon stated in `docs/VMEM_DESIGN.md`), recency×importance recall over BM25 or hybrid; verbatim KV anchors; MCP adapter `vmem-mcp` (Linux/macOS/Windows) for Claude Code/Desktop and any MCP host
- **Recovery after memory corruption** — `SOURCE` provenance on every fact, `VMEM.EXPLAIN` (which facts produced this answer, from which origins, and what dropped the rest), `VMEM.QUARANTINE` (revoke one origin, keep the fact as evidence), `VMEM.COVERAGE` + `VMEM.BACKFILL` (measure and repair provenance coverage), and point-in-time `-restore-to-lsn`; measured against the real OWASP Agent Memory Guard, reproducible with one `docker compose` command
- **Cryptographic erasure** *(opt-in)* — `-encrypt-at-rest` seals VMEM payload at the persistence boundary under a per-scope key (envelope: the wrapped DEK rides with the record, the KEK lives in a keyring that is never shipped and never snapshotted); `VMEM.SHRED` destroys that key and takes the WAL, the snapshots and the shipped archives with it; `VMEM.RESEAL` migrates pre-keyring facts forward. The read path pays nothing — the engine works on plaintext in memory; cost lands on write (~1.9 µs/fact, ≈1% of the insert budget)
- **Tamper-evident audit chain** *(opt-in)* — `-audit-chain` records every memory-changing command in a hash-chained journal (Merkle-batched, one link per second; fingerprints, never content), so a `SHRED` receipt is producible months later rather than read once; `VMEM.AUDIT VERIFY / EXPORT / PROVE / RECONCILE` — Ed25519-signed statements an auditor checks without holding your secret, inclusion proofs that disclose one event and not its batch, and a live-memory-vs-journal reconciliation
- **BM25 full-text + hybrid** — `VSIM.SEARCHTEXT` / `VSIM.HYBRID` (RRF fusion), embedder-free known-item search, query-side common-term pruning, attribute filters on both
- **Vector Search (HNSW)** — the core: arena-based graph, SQ8 quantization, tenant/attribute filtering, bitset visited, DotProduct optimization; non-blocking bulk ingest (per-shard delta freeze + batched LSM merges)
- **AI / RAG** *(optional, off by default)* — Ollama embeddings, async ingestion, semantic queries (`AI.INGEST` / `AI.ASK`); the engine itself is BYO-embeddings
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

### One binary

Static Linux binaries (amd64/arm64, zero dependencies, ~13MB) are published on
the [Releases page](https://github.com/Nikolay1994Kaz/storage_in_memory/releases):

```bash
tar xzf kvstore-server_*_linux_amd64.tar.gz
./kvstore-server --port 6380
```

Linux only — the network layer is built on epoll.

The engine is **BYO-embeddings**: it takes pre-computed vectors over RESP from
any embedding provider (OpenAI, Ollama, sentence-transformers, …):

```bash
redis-cli -p 6380
> VSIM.ADD doc:1 0.12 0.90 0.31
> VSIM.ADD doc:2 0.85 0.05 0.48
> VSIM.SEARCH 2 0.10 0.88 0.30
> SET hello world
```

### Docker Compose — server + metrics

kvstore plus the Grafana/VictoriaMetrics observability stack:

```bash
git clone https://github.com/Nikolay1994Kaz/storage_in_memory.git
cd storage_in_memory
docker compose up -d --build     # or: make up
```

`--build` rebuilds the image from the current sources, so a `git pull` never
leaves you on a cached binary that lacks newer commands. That's it — KVStore is
on `localhost:6380`, Grafana on `localhost:3000`.

### Optional: RAG demo (Ollama)

The `AI.*` commands (`AI.EMBED` / `AI.INGEST` / `AI.SEARCH` / `AI.ASK`) are a
self-contained RAG demo on top of the engine — they are the only part that
talks to Ollama, and they are **off by default**. Enable with the `ai` profile:

```bash
docker compose --profile ai up -d --build     # or: make up-ai
```

The first run downloads models: `nomic-embed-text` (~0.3GB, embeddings) and
`gemma4:e2b` (~7GB, the chat model behind `AI.ASK`). Embeddings only:
`OLLAMA_SKIP_CHAT_MODEL=1 docker compose --profile ai up -d --build`.

```bash
redis-cli -p 6380
> AI.INGEST doc:1 "Go is a statically typed language"   # async: returns QUEUED
> AI.ASK "What is Go?"
```

Notes: `AI.INGEST` indexes asynchronously (subscribe to `ai:indexed` for
completion); the first `AI.ASK` after startup loads the 7GB model into memory
and may take tens of seconds. The server detects Ollama in the background — no
restart needed, in whatever order the containers come up.

Then take the guided tour — real embeddings, tenant/attribute filtering, one
short Go file: [`kvstore/examples/quickstart`](kvstore/examples/quickstart/).

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
| `--data-dir` | `data` | Directory for WAL, snapshots, the keyring and the audit chain (a relative path is resolved against the working directory) |
| `--encrypt-at-rest` | `false` | Seal VMEM payload at the persistence boundary with a per-scope key from `keyring.dat`, and enable `VMEM.SHRED`. An existing keyring is opened regardless of the flag — otherwise earlier envelopes would become unreadable |
| `--audit-chain` | `false` | Record memory-changing commands in a hash-chained journal under `<data-dir>/auditchain/`. Never compacted (it is evidence): ≈3.6 GB/year |
| `--maxmemory` | `0` | Memory limit in MB (0 = unlimited); writes are rejected above the limit |
| `--max-connections` | `10000` | Cap on concurrent connections (0 = unlimited) |
| `--idle-timeout` | `5m` | Close connections idle longer than this (pub/sub subscribers are exempt); 0 = off |
| `--write-timeout` | `30s` | Max time to flush a response to a slow reader; 0 = off |
| `--requirepass` / `--requirepass-file` | `""` | AUTH password inline or from a file (empty = no auth) |
| `--tls-cert` / `--tls-key` | `""` | TLS certificate and private key (PEM) |
| `--tls-client-ca` | `""` | CA for client-certificate verification (mTLS) |
| `--ollama-url` | `http://localhost:11434` | Ollama API URL for the optional `AI.*` layer; the server pings it in the background and enables `AI.*` whenever Ollama comes up (no restart needed) |
| `--ship-url` | `""` | Continuous WAL-shipping target: `file:///path` or `s3://bucket/prefix?endpoint=...` (creds via env, see [docs/BACKUP.md](docs/BACKUP.md)) |
| `--ship-interval` | `1s` | Shipping period (≈ crash RPO) |
| `--ship-retain` | `3` | Restore points kept on the remote |
| `--ship-restore` | `false` | Restore data dir from `--ship-url` before start |
| `--restore-to-lsn` | `0` | Forensic point-in-time start: raise the store as it was after this LSN and serve **reads only**; the data directory is not modified (0 = normal start) |
| `--wal-inspect` | `false` | Print the journal (LSN, op, key) and exit — how you find the LSN for `--restore-to-lsn` |
| `--log-level` / `--log-format` | `info` / `text` | Structured logging level and format (`text`/`json`) |
| `--pprof` | `false` | Expose `/debug/pprof/*` on the metrics port — **never in production** |

## Supported Commands

The full command manifest (syntax, replies, gate semantics, WAL ops) lives in
[docs/COMMANDS.md](docs/COMMANDS.md) — the surface is deliberately small and frozen.
Families: agent memory (`VMEM.*`, incl. `SHRED`/`RESEAL`/`AUDIT`), vector +
full-text search (`VSIM.*`, incl.
`SEARCHTEXT`/`HYBRID`), KV/TTL (`SET`/`GET`/`DEL`/`EXPIRE`/…), transactions
(`MULTI`/`EXEC`/`DISCARD`), Pub/Sub, sorted sets (`ZADD`/…), AI/RAG (`AI.*`,
optional — requires a reachable Ollama).

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
├──────────────────────────────────────────────┤
│   VMEM memory layer (validity / erasure /    │
│   recency-ranked recall; MCP via vmem-mcp)   │
├──────────────────────────────────────────────┤
│  Persistence boundary: keyring envelope seal │
│  + audit chain (both opt-in)                 │
├──────────────┬───────────────────────────────┤
│   TCMalloc   │  TTL    │ WAL    │ Pub/Sub   │
│   Store      │  Heap   │ Batch  │ Hub       │
├──────────────┼─────────┼────────┼───────────┤
│ HNSW + BM25  │  WASM   │   AI   │  Cluster  │
│ Search       │  Engine │ Worker │  (gossip) │
└──────────────┴─────────┴────────┴───────────┘
```

## Benchmarks

Measured on standard ANN datasets and real OpenAI embeddings, including
same-machine head-to-head runs against hnswlib — full tables, methodology and
honest caveats in [docs/BENCHMARKS.md](docs/BENCHMARKS.md). Headlines:

- **Agent memory (VMEM):** on a 27.5k-event synthetic "agent life" —
  known-item hit@1 **0.982** / MRR **0.991**, paraphrase hit@10 1.000,
  temporal accuracy (`ASOF` + supersession chains) **1.000**, scope isolation
  **0 violations**; decay formula judged on 20k real ada-002 embeddings
  (hit@10 = 1.000 in every age bucket). End-to-end over RESP: `RECALL` p50
  110 µs / p99 **0.29 ms** at **64 426 QPS**, p99 13.3 ms under a mixed
  read/write soak — zero errors.
- **End-to-end through the server (RESP, real query vectors):**
  **12 985 QPS** @ recall@10 0.9996 on MNIST-784 (SQ8) and **3 928 QPS** @
  0.9888 on dbpedia-1536; ingest up to **5 533 vec/s** over the wire with
  sharded delta. After bulk loads the index consolidates back to peak shape
  automatically (`-idle-consolidate`).
- **Search stays online during bulk loads:** per-shard delta freeze + batched
  L0 merges hold a **750–800 QPS** search floor on dbpedia-1536 (100k, SQ8)
  while 12 connections ingest concurrently at **673 vec/s**, converging to
  **3 544 QPS** @ 0.9900 after the load — no brownout window (earlier builds
  dipped to double-digit QPS in this state).
- **High-dim (GIST-960, target path):** SQ8 beats hnswlib float32 **2.5–2.7×**
  in multithreaded QPS at equal recall, with 4× less vector memory.
- **Real embeddings (dbpedia ada-002, 1536-dim):** recall@10 0.977 (SQ8) /
  0.984 (fp32); SQ8 gives ~2.3× QPS and 3.7× less memory.
- **Multi-tenant filtered search:** tenant-routed queries are **5.8×–28 620×**
  faster than post-filtering a full graph traversal, at recall 1.0.
- **Low-dim float32 (SIFT-1M):** on a consolidated index the engine beats
  hnswlib **1.36–1.38×** multithreaded (25 697 vs 18 833 QPS @ recall 0.96;
  scaling 5.0× vs 3.2× on 6 cores); on a freshly loaded fragmented index
  hnswlib is ~3× faster — the honest cost of an LSM design that supports
  concurrent writes, deletes and crash recovery. Idle consolidation converges
  to the fast state automatically.

## Status & Scope

- **Single-node by design.** Durability and disaster recovery come from WAL +
  snapshots + continuous WAL-shipping (restore on any machine), not from
  replicas. Cluster mode exists behind a build tag and is not production-ready.
- **ANN search is approximate.** HNSW recall is high (0.99 end-to-end on real
  embedding datasets at default settings) but not 1.0; under heavy churn a small fraction
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
make test              # The gate: exactly what CI runs (-short, ~1 min)
make test-race         # Race detector over the subsystems, as in ci.yml
make test-full         # Everything, no -short: search-quality validation + 500k stress
make bench             # Run benchmarks
make vet               # Static analysis
```

`make test-full` is **not** a gate and CI never runs it: it takes tens of minutes and
exists to exercise scale (500k vectors) and search quality on real datasets. Read a
failure there as a *measurement* first — re-check it on an idle machine before
concluding the code broke.

Insert throughput is guarded by `TestShardedInsertScaling`, which runs in CI. It
asserts a *ratio* (sharded delta vs a single shard) rather than an absolute vec/s
figure, because an absolute floor measures the hardware as much as the code — and a
ratio compares two arms recorded on the same machine in the same run.

## Project Structure

```
kvstore/
├── cmd/kvstore/          # Server entry point (main.go)
├── cmd/vmem-mcp/         # MCP stdio adapter (agent memory tools)
├── internal/
│   ├── server/           # Epoll server, ConnBuf, Handler
│   ├── store/            # KV store, TTL manager
│   │   ├── tcmalloc/     # TCMalloc-style allocator (MCache, MCentral, MHeap)
│   │   └── zset/         # Sorted-set registry
│   ├── wal/              # Write-Ahead Log, snapshots, batch writer
│   ├── ship/             # Continuous WAL-shipping (file://, s3://) + restore
│   ├── keyring/          # Per-scope KEK, envelope seal/unseal, crypto-erasure
│   ├── auditchain/       # Hash-chained journal, Merkle batching, Ed25519 export
│   ├── pubsub/           # Pub/Sub hub (classic + semantic)
│   ├── btree/            # Sorted-set backing structure
│   ├── compute/          # WASM engine (wazero), triggers, worker slots
│   ├── ai/               # Ollama client, async worker
│   ├── cluster/          # Hash slots, gossip, replication (experimental)
│   ├── logging/          # slog setup (levels, text/JSON)
│   ├── monitoring/       # Prometheus-style metrics
│   ├── protocol/         # RESP parser and writer
│   ├── vmemcorpus/       # Deterministic "agent life" corpus generator (benches)
│   └── vmemmcp/          # MCP adapter logic (JSON-RPC loop, tool mapping)
├── vector/               # HNSW graph, SQ8, BM25 text index, VMEM layer, tenant/attr filtering
└── examples/             # WASM module examples
docs/                     # Command manifest, VMEM design, MCP quickstart, backup, benchmarks, format compat
monitoring/               # Grafana/VictoriaMetrics provisioning for the local stack
scripts/                  # Soak harness, backup/restore, poisoning-recovery comparison, live drills (shred, audit chain, agent)
```

## Commercial use

MIT — use it in a commercial product without asking. If you need something the
licence does not cover — support with a response time, an integration built to
your requirements, an answer to a procurement questionnaire (data residency,
what the erasure guarantee is and where it ends, "what did the agent know at
time T"), a deployment in a closed network, or terms other than MIT — write to
dubovoinikolai@gmail.com. The copyright is held by one person, so dual
licensing is a conversation, not a legal project.

## License

See [LICENSE](LICENSE).
