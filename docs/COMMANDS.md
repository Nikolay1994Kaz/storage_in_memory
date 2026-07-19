# Command Surface (v1) — FROZEN

**Status:** frozen as of 2026-07-04.
**Decision:** the KV/state command surface below is *closed for extension*. New KV
command families are not added — not because they are hard, but because each one
permanently costs: an input-validation surface, an OOM-gate and durability
classification, a WAL op + replay + compaction path, dispatcher tests, a
soak-oracle model, and a compatibility promise for every future replication /
upgrade protocol. The product is the vector engine; the KV layer is its
payload/state layer, not a Redis replacement.

**What freeze means:**
- No new commands in the *Frozen* tier. Bug fixes, hardening, and semantic
  clarifications of existing commands continue as normal.
- The `VSIM.*` family (*Product* tier) is **not** frozen — it grows with the
  vector roadmap.
- `AI.*` grows only demand-driven.
- Unfreezing a KV command requires the checklist at the bottom, triggered by a
  real workload asking for it — never "for parity".

**What freeze buys:**
1. **Closed mutation alphabet.** WAL replay, snapshot compaction, and any future
   replication protocol only ever need to understand the ops listed here
   (`OpSet/OpDel/OpExpire/OpPersist/OpZAdd/OpZRem/OpVSimAdd/OpVSimAddAttrs/OpVSimDel`).
2. **Finite test surface.** Dispatcher coverage and the soak oracle can reach
   "models everything" — a completable goal, not a treadmill.
3. **Bounded attack surface.** A fixed set of argument parsers, each hardened once.
4. **Honest compatibility story.** We promise exactly these commands with exactly
   these semantics — we do not promise "Redis compatible".

---

## Tiers

| Tier | Families | Policy |
|---|---|---|
| **Frozen** | KV, TTL, Pub/Sub, ZSET, transactions, admin | Closed for extension |
| **Product** | `VSIM.*` | Grows with vector roadmap |
| **Demand-driven** | `AI.*` | Extended only on real demand |
| **Experimental** | `WASM.*`, `CLUSTER`/`MIGRATE`/`PSYNC` | Behind build tags; excluded from prod builds |

---

## Connection layer

- **`AUTH <password>`** — required before anything else when `-requirepass` /
  `KVSTORE_REQUIREPASS` / `-requirepass-file` is set (constant-time SHA-256
  compare). Unauthenticated connections may only run `AUTH` and `PING`; everything
  else gets `NOAUTH Authentication required`.
- **Unknown command** → `ERR unknown command '<CMD>'`.
- **Subscriber mode.** Once a connection holds any subscription (classic or
  semantic), it is served exclusively by the pub/sub path (single socket writer =
  `writePump`). In this mode only `SUBSCRIBE`, `UNSUBSCRIBE`, `PING`, `PUBLISH`,
  `VSIM.SUBSCRIBE`, `VSIM.UNSUBSCRIBE` are honored.

## Cross-cutting gates

Every command is classified against two independent gates (single choke point in
`executeCommand`):

- **OOM gate** (`isMemoryGrowingCmd`): when used memory > `maxmemory`, memory-growing
  commands are rejected with `OOM command not allowed…`. Applies to: `SET`, `ZADD`,
  `VSIM.ADD`, `VSIM.ADDBIN`, `VSIM.ADDATTR`, `VSIM.ADDDOC`, `WASM.LOAD`,
  `WASM.LOADFILE`, `AI.INGEST`.
  Deleting/reading commands are always allowed (the only way out of OOM).
- **Durability fail-stop** (`isWriteCmd`): when the WAL can no longer write durably
  (ENOSPC, I/O error), *all* mutating commands are rejected — including deletes
  (a delete must be logged too, or it resurrects on restart). Applies to: `SET`,
  `DEL`, `EXPIRE`, `PERSIST`, `VSIM.ADD`, `VSIM.ADDBIN`, `VSIM.ADDATTR`,
  `VSIM.ADDDOC`, `VSIM.DEL`, `ZADD`, `ZREM`, `AI.INGEST`. Reads stay available.

A new mutating command (any tier) MUST be added to both classifiers explicitly.

## Transactions

- **`MULTI`** / **`EXEC`** / **`DISCARD`** — Redis-style queuing; queued commands
  reply `QUEUED`; a forbidden command in the queue aborts the whole transaction
  (`EXECABORT`, like Redis).
- Forbidden inside a transaction: `SUBSCRIBE`, `UNSUBSCRIBE`, `VSIM.SUBSCRIBE`,
  `VSIM.UNSUBSCRIBE` (they switch the socket to a second writer and would corrupt
  the promised RESP array frame).
- **Isolation contract (deliberate):** `EXEC` provides *grouping and durability*,
  **not isolation**. `EXEC`s are serialized against each other, but a plain command
  from another connection may interleave between queued commands. True isolation
  would require globally serializing all writes and was consciously rejected to
  preserve the per-worker zero-alloc model.

---

## Frozen tier — command reference

### KV / TTL

| Command | Syntax | Reply | Notes |
|---|---|---|---|
| `SET` | `SET key value [EX seconds]` | `+OK` | Only `EX` is supported (no `PX/NX/XX/KEEPTTL/GET`). Bare `SET` **clears** an existing TTL (Redis-without-KEEPTTL semantics), logging `OpPersist` only if a TTL actually existed. WAL: `OpSet` (+`OpExpire`). |
| `GET` | `GET key` | bulk / nil | Lazy-expiry aware: expired key reads as nil. |
| `DEL` | `DEL key` | `:1` / `:0` | Single key only (no variadic). **Composite:** also deletes the vector stored under the same key and drops its TTL. WAL: `OpDel`. |
| `EXPIRE` | `EXPIRE key seconds` | `:1` / `:0` | `seconds` must be > 0 (no delete-on-nonpositive). WAL: `OpExpire` with absolute deadline. |
| `TTL` | `TTL key` | `:sec` / `:-1` / `:-2` | `-1` = no TTL, `-2` = no key. |
| `PERSIST` | `PERSIST key` | `:1` / `:0` | WAL: `OpPersist` only when a TTL was removed. |
| `DBSIZE` | `DBSIZE` | `:n` | KV key count. |
| `PING` | `PING` | `+PONG` | Allowed pre-AUTH. |

**TTL is composite:** active/lazy expiry deletes the KV value *and* the vector
under the same key (compositeEvictor), logging `OpVSimDel` so the deletion
survives restart.

### Pub/Sub (classic)

| Command | Syntax | Reply | Notes |
|---|---|---|---|
| `SUBSCRIBE` | `SUBSCRIBE ch [ch ...]` | via writePump | Switches connection to subscriber mode. |
| `UNSUBSCRIBE` | `UNSUBSCRIBE [ch ...]` | `+OK` | Idempotent. |
| `PUBLISH` | `PUBLISH ch message` | `:receivers` | |

### Sorted sets

| Command | Syntax | Reply | Notes |
|---|---|---|---|
| `ZADD` | `ZADD key score member` | `:1` new / `:0` update | Single triple per call (no variadic, no `NX/XX/GT/LT/INCR`). WAL: `OpZAdd`. |
| `ZSCORE` | `ZSCORE key member` | bulk / nil | |
| `ZREM` | `ZREM key member` | `:1` / `:0` | WAL: `OpZRem`. |
| `ZRANGEBYSCORE` | `ZRANGEBYSCORE key min max [WITHSCORES]` | array | Numeric min/max only (no `(`-exclusive, no `-inf/+inf` tokens, no `LIMIT`). |
| `ZCARD` | `ZCARD key` | `:n` | |

### Admin

| Command | Syntax | Reply | Notes |
|---|---|---|---|
| `COMPACT` | `COMPACT` | `+OK compaction started` | Background WAL compaction: snapshot of KV+TTL+zset+vectors, old segments removed. |

---

## Product tier — `VSIM.*` (not frozen; grows with roadmap)

All vector ingest is sanitized **before** the WAL write (empty/NaN/Inf vectors and
bad keys are rejected so poison never survives via replay), and all `Add`s happen
**before** the WAL write (snapshot-watermark safety). `K` is bounded to
`1..100000` (`vector.MaxSearchK`).

| Command | Syntax | Reply |
|---|---|---|
| `VSIM.ADD` | `VSIM.ADD key v1 … vN` | `+OK` |
| `VSIM.ADDBIN` | `VSIM.ADDBIN key <float32-LE bytes>` | `+OK` |
| `VSIM.ADDATTR` | `VSIM.ADDATTR key [CAT k v]… [NUM k v]… VEC v1 … vN` | `+OK` — columnar attr/tenant ingest; WAL: `OpVSimAddAttrs` |
| `VSIM.ADDDOC` | `VSIM.ADDDOC key TEXT text [TITLE title] [CAT k v]… [NUM k v]… VEC v1 … vN` | `+OK` — doc ingest (vector + attrs + BM25 text). Tokenized once at ingest; the WAL entry (`OpVSimAddDoc`) carries the resulting terms, never raw text (replay never re-tokenizes → bit-exact across stemmer versions). Optional `TITLE` boosts title terms ×3 (field weighting baked into terms at ingest; changing the weight = re-ingest). Empty `TEXT ""` clears the doc's text (upsert semantics, same as attrs) |
| `VSIM.DEL` | `VSIM.DEL key` | `:1` / `:0` |
| `VSIM.EXISTS` | `VSIM.EXISTS key` | `:1` / `:0` — direct point lookup (delta/tombstones/segments), bypasses ANN; used by the soak durability oracle to separate real loss from recall miss |
| `VSIM.SEARCH` | `VSIM.SEARCH K v1 … vN` | flat array `key, dist, …` |
| `VSIM.SEARCHBIN` | `VSIM.SEARCHBIN K <float32-LE bytes>` | flat array |
| `VSIM.SEARCHTEXT` | `VSIM.SEARCHTEXT K query` | flat array `key, score, …` — lexical BM25 top-K (embedder-free path); score is BM25, **higher = better** (not a distance). Query terms with df > N/2 are pruned at search time on corpora ≥ 1000 docs (near-zero idf, dominates posting scan; all-common queries fall back to unpruned) |
| `VSIM.HYBRID` | `VSIM.HYBRID K TEXT query VEC v1 … vN` | flat array `key, score, …` — top-100 lexical + top-100 vector fused by Reciprocal Rank Fusion (k=60, rank-based → no score calibration); score is the RRF sum, comparable to nothing but itself |
| `VSIM.FILTER` | `VSIM.FILTER K [EQ attr val]… [RANGE attr lo hi]… VEC v1 … vN` | flat array — columnar filter (+tenant routing via `-partition-attr`) |
| `VSIM.SEARCHFILTER` | `VSIM.SEARCHFILTER K field value v1 … vN` \| `… K PREFIX prefix v1 … vN` | flat array — KV-metadata filter (`GET field:key == value`) or key-prefix filter |
| `VSIM.SEARCHRANGE` | `VSIM.SEARCHRANGE K zsetKey minScore maxScore v1 … vN` | flat array — B+Tree range ∩ HNSW |
| `VSIM.INFO` | `VSIM.INFO` | bulk `vectors:… dimension:… max_level:…` — flushes delta first (sync point) |
| `VSIM.SUBSCRIBE` | `VSIM.SUBSCRIBE threshold v1 … vN` | via writePump — semantic pub/sub |
| `VSIM.UNSUBSCRIBE` | `VSIM.UNSUBSCRIBE` | `+OK` (idempotent) |
| `VSIM.PUBLISH` | `VSIM.PUBLISH message v1 … vN` | `:receivers` |

**Cross-engine joins** (why KV/zset live in the same process as the index):
`VSIM.SEARCHFILTER` reads KV metadata per candidate; `VSIM.SEARCHRANGE` joins the
zset B+Tree with HNSW; `DEL`/TTL expiry atomically covers both stores; `AI.ASK`
joins vector search with KV documents. These joins are only cheap and consistent
because there is one process, one WAL, one memory space.

## Demand-driven tier — `AI.*` (optional, requires a reachable Ollama)

`AI.*` is an optional RAG demo layer, **off by default** — the engine itself is
BYO-embeddings (`VSIM.ADD*`). Enable it by pointing `--ollama-url` at a running
Ollama (in Docker: `docker compose --profile ai up`). The server pings Ollama in
the background and enables `AI.*` the moment it comes up — startup order doesn't
matter and no restart is needed.

| Command | Syntax | Reply |
|---|---|---|
| `AI.EMBED` | `AI.EMBED text` | array of floats |
| `AI.SEARCH` | `AI.SEARCH K text` | flat array `key, dist, …` |
| `AI.ASK` | `AI.ASK question` | bulk — RAG: embed → vector top-3 → KV docs → LLM |
| `AI.INGEST` | `AI.INGEST key text` | `+QUEUED` — async embed+store; OOM- and WAL-gated |

Until Ollama is reachable all `AI.*` reply `ERR Ollama not available …` (with a
hint how to enable). `AI.ASK` additionally needs the chat model pulled
(`gemma4:e2b`, ~7GB); the first call after startup loads it into memory and may
take tens of seconds (command deadline 90s, warm calls ~13s).

## Experimental tier (build-tag gated)

- **`WASM.*`** — compute runtime, `-tags experimental` only; prod builds reply
  "WASM disabled" and do not link the engine.
- **`CLUSTER` / `MIGRATE` / `PSYNC`** — cluster mode, experimental build only;
  prod builds reply `ERR cluster mode is not enabled`. The replication protocol is
  deliberately undefined until alem.ai RPO/RTO requirements exist (see LSN design).

---

## Explicit non-goals

We will **not** add the following, regardless of how small each one looks
(the whole point is that "small" commands are not small — see header):

- String ops: `MGET`/`MSET`, `INCR`/`DECR`, `APPEND`, `GETRANGE`/`SETRANGE`,
  `SETNX`/`SETEX`/`GETEX`/`GETDEL`, `KEEPTTL`.
- Data-structure families: hashes (`HSET`…), lists (`LPUSH`…), sets (`SADD`…),
  streams (`XADD`…), bitmaps, HyperLogLog, GEO.
- Keyspace iteration: `SCAN`, `KEYS`, `RANDOMKEY`, keyspace notifications.
- Scripting: `EVAL`/Lua, `FUNCTION`.
- Full zset parity: rank-based `ZRANGE`, `ZINCRBY`, `ZRANGEBYLEX`, aggregations.
- Multiple logical DBs (`SELECT`), `WAIT`, client-side caching protocol.

Client libraries that assume full Redis compatibility will not work against this
surface, and that is intentional. Positioning: *a vector database whose
payload/state layer speaks RESP* — not a Redis replacement.

---

## Unfreeze checklist

A frozen-tier addition starts with a trigger: **a real workload (agent/RAG) needs
it and no existing command composes to cover it.** Then, before merge:

1. **Spec first** — syntax, reply shape, and error cases added to this document.
2. **Input validation** — every argument parsed defensively (bounds, NaN/Inf,
   lengths); poison must be rejected *before* the WAL write.
3. **Gate classification** — explicit entry (or explicit exemption) in
   `isMemoryGrowingCmd` and `isWriteCmd`.
4. **Durability** — WAL op defined; replay covered; `snapshotIterate`/compaction
   covered; `FORMAT_COMPAT.md` versioning bumped if the format changes; restart
   E2E test.
5. **TTL interaction** — defined and tested (including composite KV+vector effects).
6. **Transaction classification** — allowed in `MULTI`/`EXEC` or added to
   `forbiddenInTx`, with a test.
7. **Subscriber-mode behavior** — defined (honored or rejected).
8. **Dispatcher tests** — via the executeCommand harness (OOM / fail-stop /
   nil-seam cases included).
9. **Soak oracle** — the oracle models the new command; stress mix updated.
10. **Cluster/replication note** — statement of what the command means for the
    (future) replication alphabet, even if it's "experimental-only for now".

If any step feels not worth doing, the command is not worth adding.
