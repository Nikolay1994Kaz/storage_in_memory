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
| `VSIM.SEARCHTEXT` | `VSIM.SEARCHTEXT K query [EQ attr val]… [RANGE attr lo hi]…` | flat array `key, score, …` — lexical BM25 top-K (embedder-free path); score is BM25, **higher = better** (not a distance). Optional `EQ`/`RANGE` (same syntax as `VSIM.FILTER`) are judged **before** each source's top-K (pre-filter, no starvation); BM25 statistics stay global (whole corpus), not per-filter. Query terms with df > N/2 are pruned at search time on corpora ≥ 1000 docs (near-zero idf, dominates posting scan; all-common queries fall back to unpruned) |
| `VSIM.HYBRID` | `VSIM.HYBRID K TEXT query [EQ attr val]… [RANGE attr lo hi]… VEC v1 … vN` | flat array `key, score, …` — top-100 lexical + top-100 vector fused by Reciprocal Rank Fusion (k=60, rank-based → no score calibration); score is the RRF sum, comparable to nothing but itself. Optional `EQ`/`RANGE` apply to **both arms before fusion** (filter-then-fuse; post-fusion filtering would starve small tenants) |
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
joins vector search with KV documents; `VMEM.RECALL` joins the index with the
verbatim KV anchors. These joins are only cheap and consistent because there is
one process, one WAL, one memory space.

## VMEM commands (agent memory)

Thin memory layer over the same store (`docs/VMEM_DESIGN.md`, sprint 2). A fact
is an ordinary doc: key + CAT/NUM attributes + BM25 text — **zero new fields in
the engine format**. Times are absolute unix seconds; everything time-dependent
is stamped **before** the WAL write (replay never looks at the clock).

| Command | Syntax | Reply |
|---|---|---|
| `VMEM.REMEMBER` | `VMEM.REMEMBER scope TEXT text [ID id] [TYPE t] [IMPORTANCE 0..1] [VALIDFROM unix] [TTL sec] [SUPERSEDES id] [SOURCE s] [VEC v1 … vN]` | bulk `id` (server ULID unless `ID` given; retry with client `ID` = idempotent upsert). `SOURCE` records where the fact came from (channel/tool/document); **when omitted it is stamped with the literal `unknown`, never left absent** — a fact nobody vouched for must still be selectable by a source-scoped revocation. No `VEC` = embedding-ladder step 0: the fact lives BM25-only (deterministic placeholder vector). `SUPERSEDES` closes the target's `valid_to` and ingests the pair as **one atomic WAL record** (`OpVSimAddDocBatch`, single CRC — a crash cannot replay a half-pair); superseding a TTL-expired target is rejected regardless of reaper timing. The verbatim text (the anchor) is also stored in KV under `vmem:<id>` (`OpSet`; `TTL` mirrored via `OpExpire`) |
| `VMEM.RECALL` | `VMEM.RECALL scope K query [ASOF unix \| ALL] [TYPE t] [SOURCE s] [HALFLIFE sec] [WEIGHTS wtext wvec] [VEC v1 … vN]` | flat triples `id, score, text, …` — hybrid (or BM25-only without `VEC`) over the valid slice, rescored by `fused × 2^(−age/halfLife) × (0.5 + importance)` on a top-100 overfetch; score comparable to nothing but itself. Default = valid-now; `ASOF ts` answers "what was true at ts" (sees through supersession, **never** through erasure); `ALL` disables interval judgement. ⚠`ASOF` answers *what was true*, not *what the agent had seen*: `valid_from` may be set to the past on ingest, so a fact written today can appear in an `ASOF` query for April, and nothing in the read path distinguishes that from a fact the agent actually held in April. The true write time of every fact **is** recorded — in the audit chain, tamper-evident — but is not queryable from `RECALL`; see door 4 in `docs/VMEM_DESIGN.md`. `HALFLIFE` defaults to **365 days** (decay policy belongs to the client; the default was 30 days until 2026-08-04, when `BENCHMARKS.md` §9 measured it costing 11.8 points of recall@5 on conversations spanning eight months — set it low deliberately for short-lived context, not by inheriting it). `WEIGHTS wtext wvec` scales each arm's vote in the RRF fusion (default `1 1`); it requires `VEC`, rejects negatives and both-zero, and is a **lever, not autotuning** — the engine never guesses which arm to mute. Fusion is worth +6.2 points when the arms are comparable and −18.3 when one is far weaker, so a client who knows its data needs a way to say so. ⚠Weight 0 mutes an arm's *vote* in rank space; it is not the same request as omitting `VEC`, which switches to the BM25-only score scale. `SOURCE s` narrows the slice to facts of that provenance (`SOURCE unknown` finds the ones written without a declared source) — the forensic entry point: look at everything a suspect channel wrote before revoking any of it. `text` is the verbatim anchor from KV (empty if none) |
| `VMEM.QUARANTINE` | `VMEM.QUARANTINE scope SOURCE s [SINCE unix] [LIMIT n]` | a **receipt**: `scope`, `source`, `since`, `revoked`, **`still_trusted`, `outside_window`, `over_limit`**, `other_origins`. Mass revocation **by origin**: matching facts keep everything (text, vector, application time) and gain a `quarantined_at` axis — `RECALL` stops returning them, `ASOF` *before* the revocation still does (the record of what the agent believed is evidence, not noise), `ALL` always does. Selective by construction: neighbouring sources and facts written *after* the poison are untouched, which whole-store rollback cannot achieve. `SOURCE` is mandatory — "revoke everything" must not be expressible. `SINCE` bounds by `valid_from`. Idempotent: a repeat revokes nothing and never moves the original moment. The whole batch is **one** `OpVSimAddDocBatch` record (a crash cannot leave half the lies revoked), hence `LIMIT` (default/cap 4096); take the tail with another call. ⭐`LIMIT` bounds the facts **actually revoked**, not the candidates examined: a fact the verdict will reject anyway — already revoked, another scope, older than `SINCE`, expired — never consumes the batch budget. That is what makes the tail reachable and what makes a zero `revoked` mean one thing only ("nothing left under this predicate") instead of two |

⭐**Why the reply is a receipt and not a count.** The number of beliefs revoked is not the completeness of the cure, and the two are easy to mistake for one another. `revoked: 3` reads the same whether the lie is gone or twelve facts of the same channel sit untouched beyond the window — and a window is exactly what an operator reaches for, because it drops the cost of revocation to zero. That trade-off used to be **silent**: it was computable only from outside, by a harness that knows the plan, and the operator has no harness in production. `still_trusted` is the answer to the question a procurement questionnaire actually asks — *how do you know the removal was complete* — and until it existed the honest answer was "we don't say".

`still_trusted` counts facts of the same `scope`+`source` the memory **still treats as true** after this call. It is the only measured number of the three: a full pass over the store taken *after* the verdict, deliberately not arithmetic accumulated during the candidate scan — that scan stops as soon as the batch is full, so a counter taken along the way would report zero remaining precisely when `LIMIT` truncated the work, an error in our own favour and invisible from outside. `outside_window` and `over_limit` split it by the reason the verdict rejected each fact, mirroring the verdict's own conditions: `outside_window` is what the `valid_from` predicate does not take (**including a fact carrying no `valid_from` at all** — legacy data mass revocation cannot select, the same blind spot `absent` reports in `VMEM.COVERAGE`), `over_limit` is everything else, i.e. facts the predicate accepts that did not fit the batch — take them with another call.

⚠**What `still_trusted: 0` does and does not claim.** It claims completeness *within the predicate you asked for*, and nothing wider. Lies that arrived through a **different channel**, or that an agent restated **in its own words** (their provenance is clean by then), lie outside it — hence `other_origins: not_covered`, present in every receipt rather than only in the alarming ones. This is measured, not hypothesised: across the six cases in `scripts/revocation_limits.py`, three (L1, L4, L5) end with `still_trusted: 0` while a lie is demonstrably still in memory, and one (L3) reports `14` after revoking a single fact — the case the field exists for. Note also that the remainder does not judge *content*: on a benign corpus those same facts are legitimate mail. It states the size of what the cure did not touch, and leaves the reading of it to the operator.
| `VMEM.EXPLAIN` | `VMEM.EXPLAIN scope K query [same modifiers as RECALL]` | one record of `name, value, …` pairs per candidate, preceded by a query summary (`mode`, `t_eff`, `half_life`, `candidates`, `returned`); kept facts first by rank, then the rest by base score. Per fact: `verdict` (`kept` or the axis that dropped it), `rank`, `source`, `type`, `text_rank`/`vec_rank` (position in each arm *before* fusion), `base`, `age_sec`, `age_penalty`, `decay_mul`, `imp_mul`, `final`, `valid_from`/`valid_to`/`quarantined_at`, `text`. **The missing link between "the answer is wrong" and "revoke this origin":** poisoning shows up as a bad *answer*, revocation works on *provenance*, and this is the step that turns one into the other. It does **not** recompute the score — the trace is filled by the live `Recall` path, so an explanation cannot drift from the ranking it explains. Absent numeric attributes print as `none` (not the same as the literal `unknown` source). Reachable verdicts: `quarantine` and `below_k` always; `validity`/`erasure`/`type`/`source` only for a stale copy whose freshest version disagrees — everything the index pre-filter removed never becomes a candidate and is simply absent (use `ALL` / drop `TYPE`/`SOURCE` to see those; an erased fact is invisible in **every** mode by design) |
| `VMEM.BACKFILL` | `VMEM.BACKFILL scope SOURCE s [LIMIT n]` | `:n` facts migrated. Legacy migration: stamp a source on facts that have **no `source` column at all** (written before provenance existed). Without it the whole recovery layer is dead over old data — revocation selects by origin and absence is unfilterable (see `VMEM.COVERAGE`). The value is the **operator's** assertion: `unknown` ("nobody vouched for this") is the usual answer, but someone who knows the corpus may declare it (`crm-import`); provenance is an input, never our judgement. An **already declared source is never overwritten** — a command that could do that could erase the trace of who filled the memory, which is the property this layer exists for. The single, non-optional predicate is "attribute absent", hence idempotence; the freshest version is re-checked under the write lock, so a source declared by a concurrent upsert survives. Nothing but provenance is touched: application time, importance, quarantine axis and text stay bit-for-bit. Deliberately a command and not a startup migration — writing *meaning* into someone's memory during an upgrade is exactly what we criticise elsewhere. Costs a full scan; batched under `LIMIT` (default/cap 4096) as one atomic WAL record |
| `VMEM.RESEAL` | `VMEM.RESEAL scope [LIMIT n]` | a **receipt**: `scope`, `resealed`, `sealed_share`, `exposed`, `earlier_copies`. Rewrites the facts of a scope that were written before `-encrypt-at-rest` so that they are finally under the scope key — including the verbatim anchor in KV, which is the most readable thing there is. Without it `VMEM.SHRED` over a legacy corpus succeeds honestly and achieves **nothing**: there is no key those bytes were written under (`sealed_share` in `VMEM.COVERAGE` is how you see it). ⭐Unlike `VMEM.BACKFILL`, stamping is legitimate here: `source` is the operator's *assertion* about the past, while `sealed` is a *physical fact*, and this command actually re-writes the bytes it stamps. Refused without `-encrypt-at-rest` rather than degrading — stamping `sealed` on facts it did not seal would be a lie in the coverage report, not a reduced service. ⚠`earlier_copies` is always `not_covered` and is not decoration: resealing **cannot reach copies that already left** — WAL segments, snapshots and archives taken earlier stay in the clear, and destroying the key will not touch them. A share that rises to 1.0 means erasable *from now on*. Idempotent (the predicate is "no envelope", re-checked under the write lock); batched under `LIMIT` (default/cap 4096); recorded in the audit chain, because after it `VMEM.SHRED` promises more than it did yesterday |
| `VMEM.COVERAGE` | `VMEM.COVERAGE [scope]` | one record per scope: `scope`, `total`, **`sealed`, `unsealed`, `exposed`, `sealed_share`, `has_key`**, `declared`, `unknown`, `absent`, `declared_share`, `revocable_share`, plus a `source:<name>` breakdown. Two independent axes of coverage, shown side by side because they have **different** blind spots: a scope can be fully revocable and at the same time not erasable at all.

**Key axis** — how much of the scope crypto-erasure can actually reach. `sealed` counts facts written *under an envelope*; the absence of that stamp means the fact predates encryption and its bytes sit in the clear in old journal segments and archives, where destroying the key does not reach them. Deliberately measured by a stamp taken **at write time**, not by asking the keyring: `has_key` says only that a key exists *now*, so a scope half-written before encryption would report as covered — an error that would overstate coverage in our own favour, which is precisely what this command exists to prevent. There is no backfill for it, and there must not be: stamping `sealed` on a fact whose bytes were never encrypted would be a lie about the past, not a migration.

⭐The write-time stamp alone was **not enough**, and the reasoning above is why it took a second axis to see it. A stamp records the *intention* held when the fact was written; between that intention and the bytes on disk sits the whole pipeline — delta freeze, segment-type choice, the merge cascade, the snapshot writer — and that pipeline can change the answer. It did: the sealed snapshot format arrived for `frozen` segments in v8 and for `frozenSQ` and flat-HNSW only in v9. In between, a fact written under encryption that settled in one of the latter two travelled into the snapshot in the clear — and the report still called it `sealed`. So coverage is now the **intersection** of two questions — *was it meant to be sealed* and *can the place it currently rests seal it at all* — and `exposed` counts facts that pass the first and fail the second. The distinction from `unsealed` is operational, not cosmetic: `unsealed` is repaired by `VMEM.RESEAL`, `exposed` cannot be repaired by any command, because the defect is in the segment format rather than in the bytes. The segment-type check is a **whitelist**: an unrecognised type counts as not sealing. A new segment type arrives as one more `case` in a `switch`, and adding a `case` never breaks its neighbours — so the default has to be "not covered", or the next type will slip past exactly as these two did. A coverage metric that reads a flag someone else wrote is a declaration, not a measurement.

**Provenance axis** — the honesty metric behind revocation: quarantine selects **by origin**, so if origin is declared for a minority the whole recovery story is decorative. Three states exist and only two are expressible as predicates: a concrete source, the literal `unknown`, and — the one that matters — **no attribute at all** (facts written before provenance existed). The last is unfilterable, therefore invisible to mass revocation; `absent`/`revocable_share` are the only way to see that blind spot. Costs a full scan: an admin/forensic operation, not a hot path |
| `VMEM.FORGET` | `VMEM.FORGET scope id` | `:1` erased / `:0` no such fact (idempotent). Makes the fact **unreachable in every read mode** including `ASOF`, does **not** walk supersedes chains; deletes the KV anchor. Cross-scope forget is an error and leaves the fact intact. ⚠Unreachability is immediate; *physical* removal is not: bytes leave sealed segments only at the next consolidation touching them (unbounded for a cold scope) and are not reached at all in the WAL, in earlier snapshots or in shipped archives — restoring to an LSN before the call brings the fact back. The full horizon, and why it is inherent rather than a bug, is `docs/VMEM_DESIGN.md` ("Erasure guarantee") |

| `VMEM.SHRED` | `VMEM.SHRED scope` | a **receipt**: `scope`, `kek_id`, `facts_removed_from_memory`, `destroyed_at`, `chain_seq` (see «The audit chain» below; `off` without `-audit-chain`). Crypto-erasure of a whole scope: destroying its key makes the **WAL, the shipped archives and any restore taken from them unreadable at once**, including a point-in-time restore to before the call. This is the one thing `FORGET` cannot do in principle: deletion cannot catch copies that already left for the archive. Covers `snapshot.wal` (anchors are sealed at write time) and `graph_leveled.bin` for **all three segment types** — vectors, attributes and terms travel in a per-scope sealed section (format v8 for `frozen`, v9 for `frozenSQ` and flat-HNSW). ⚠Until v9 the last two were **not** covered, and that mattered more than it sounds: flat-HNSW is what the *default* configuration produces at real embedding dimensions (768, 1536 — anything above 256 without `-hnsw-use-sq`), and it wrote the raw fp32 vector, the attributes and the terms in the clear. A snapshot taken by a pre-v9 build still contains them; destroying the key does not reach those bytes, and no command can repair them — the fix is forward-only, so re-snapshot after upgrading. `VMEM.COVERAGE` is what tells you whether you are affected: such facts count as `exposed` and stay out of `sealed_share`, so a receipt cannot read as full erasure while they sit in the clear. Requires `-encrypt-at-rest`; errors if the scope has no key, which means either "already shredded" or "its facts predate the keyring" — the second is **not** erasure and must not be reported as one (`VMEM.COVERAGE` is how you see that blind spot). ⚠The receipt claims only what is checkable: *this key id was destroyed*, never "the data is gone" — a signed claim of erasure over bytes that still exist would be a document asserting something untrue. Order inside the command is fixed: memory first, key second, so a failure between them can only leave *less* readable, never more |

Notes per the unfreeze checklist: gates — `VMEM.REMEMBER`, `VMEM.QUARANTINE`,
`VMEM.BACKFILL` and `VMEM.RESEAL` are memory-growing + write (revocation,
migration and resealing all append a new version rather than mutating in place,
so they genuinely grow), `VMEM.AUDIT` is read-only in every subcommand,
`VMEM.FORGET` and `VMEM.SHRED` are write-only
(erasure must stay available under OOM), `VMEM.RECALL`, `VMEM.EXPLAIN` and
`VMEM.COVERAGE` are read-only (they therefore stay available under
`-restore-to-lsn`, which is the point: forensics must work on a store raised
read-only at a past moment). Allowed in `MULTI`/`EXEC`; ordinary-command
behavior in subscriber mode (no special casing). TTL erasure is enforced at
read time (`expires_at` range filter) and physically reclaimed by the idle-tick
reaper; the KV anchor carries its own mirrored TTL. Cluster: not routed or
replicated in v1 (single-node product; ids may be server-generated); a future
replication alphabet must carry `OpVSimAddDocBatch` as one atomic unit.
Soak-oracle modelling of `VMEM.*` is deferred to the step-8 corpus bench.

### The audit chain (`-audit-chain`)

Off by default. With the flag, every command that changes memory leaves a
record in a hash-chained journal under `<data-dir>/auditchain/`, which is what
makes a `VMEM.SHRED` receipt something you can produce six months later rather
than only read once in the reply.

| event | when it reaches the disk |
|---|---|
| `VMEM.REMEMBER` — **fact created** | batched, one Merkle-rooted link per second |
| `VMEM.FORGET` | batched (measured at ~118/s in a mixed run; synchronous would spend a third of all wall-clock on one command) |
| `VMEM.BACKFILL` | batched — a migration, not a moment anyone will dispute |
| `VMEM.QUARANTINE` | **synchronous**, one leaf per retired fact plus a summary |
| `VMEM.SHRED` | **synchronous** |

Recording creation is the part usually missing: a journal that logs only
deletions cannot show that a fact was not slipped in after the fact. What each
record holds is a **fingerprint, never the content** — a `sha256` of the text,
its source, and whether it went to disk sealed. Putting fact text into an
append-only journal would keep alive exactly what `VMEM.SHRED` destroys.

Synchronous events flush the chain **up to themselves**, so a receipt always
covers everything that came before it and the batching window never covers the
moment being proved. Accordingly the `VMEM.SHRED` receipt carries a fifth pair,
`chain_seq` — the link number to look it up by. Its value is `off` when the
chain is not enabled and `unrecorded` if the chain write failed: the erasure
still happened, and saying nothing would be worse than admitting a gap in the
journal.

Two limits worth stating. The chain is **never compacted** — compacting
evidence destroys it — so it grows forever, at roughly 3.6 GB/year at the
default one-second period regardless of traffic. And whoever owns both the
journal and the head file can truncate the tail and recompute the head;
local tamper-evidence is evidence against everyone except the owner. Under
`-restore-to-lsn` the chain is not opened at all, because opening it repairs a
torn tail — a write, and a forensic session must not write to the evidence.

#### `VMEM.AUDIT` — reading the chain

| subcommand | reply | notes |
|---|---|---|
| `VMEM.AUDIT VERIFY [FROM seq]` | `from`, `links_checked`, `head_seq`, `head_hash`, `status` | Walks the chain and matches it against the stored head. **Defaults to a window of 10 000 000 links**, not the whole chain: a full pass was measured at 27–40 s per year of chain (415–453 ns to verify plus 442–813 ns to read, per link) and grows forever. `FROM 0` asks for the full pass explicitly; `FROM <seq>` starts from a link you already attested. |
| `VMEM.AUDIT EXPORT` | a signed JSON statement | Ed25519 over `{version, pubkey, head_seq, head_hash, links, signed_at}`, canonically encoded with length prefixes so a third-party checker can reproduce the bytes without our code. The public key is printed at startup — pin it there. |
| `VMEM.AUDIT PROVE <scope> [ID id] [TYPE t]` | a signed JSON inclusion proof | Leaf + Merkle path + root + link seq + a fresh statement. **Reveals only your own event**: the path proves membership without disclosing the other leaves in the batch. `TYPE` is one of `remember` (default), `forget`, `quarantine`, `shred`, `backfill`. Searches the last 100 000 links and says so on a miss. |
| `VMEM.AUDIT RECONCILE [scope]` | two elements: journal coverage, then per-scope discrepancies | See below. Full scan — an admin operation, like `VMEM.COVERAGE`. |

**Why the signature is asymmetric.** An HMAC-sealed journal — which is what the
nearest comparable product uses — can only be checked by the holder of the
secret, and that holder is the party whose claims are under review. Handing the
auditor the secret makes the attestation prove nothing about anyone. Ed25519
breaks the loop: the private key never leaves the machine, the public key goes
in an email. ⚠What it proves is *this key signed this head*, **not** that the
owner did not rewrite the journal — someone holding both would rewrite and
re-sign. What it adds is checking without a secret, and binding to an
**instance**: a party that pinned the key earlier detects a swapped server or a
freshly-minted "clean" journal, because the signature stops matching. Verify a
proof in three steps: check the statement's signature against your pinned key,
check the Merkle path against the root, then take `chain.log` and confirm the
link at `link_seq` carries that root and chains up to the signed head.

**Reconcile** answers what the chain alone cannot: does the live memory match
the record of how it got that way? Proving a journal with the journal is
circular; the value appears only when two independent sources are compared.

| field | meaning |
|---|---|
| `recorded` | in memory, creation is journalled — normal |
| `revoked` | the journal says quarantined, the fact **is still there and carries `quarantined_at`** — also normal, and deliberately so: quarantine keeps the belief as evidence so `ASOF` before the revocation can still show it |
| `unrecorded` | in memory, **no** creation record: either older than the chain, or it entered memory outside the commands |
| `resurrected` | ⚠the fact is in memory when it should not be: either the journal says **deleted** (`FORGET`/`SHRED` must remove it), or it says **quarantined** but the `quarantined_at` mark is absent — the revocation did not take while the journal already claims otherwise |
| `missing` | the journal says alive, memory does not have it, and no TTL explains it |
| `expired` | absent, but it had a deadline and the deadline passed |

That last row exists because the TTL reaper runs inside the engine and writes
nothing to the chain; without separating it, every expired fact would be
reported as loss and the check would cry corruption over normal operation. The
first reply element (`journal_links`, `head_seq`, `leaves_read`,
`leaves_expired`) must be read first: leaves expire under retention while links
live forever, so a year-old instance legitimately cannot account for its early
facts, and `unrecorded` has to be read against how much journal survives.

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
