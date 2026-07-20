# VMEM — Agent Memory Layer — Design (v1)

Status: **approved design, pre-implementation** (2026-07-19).
Scope: a thin memory layer (`VMEM.REMEMBER / RECALL / FORGET`) for AI agents on
top of the existing engine. This is sprint 2 of the memory-engine track;
sprint 1 (BM25/hybrid, `BM25_HYBRID_DESIGN.md`) is complete and provides the
embedder-free lexical tier ("step 0"). Sprint 3 (demo + MCP server) is out of
scope here.

## Motivation

- Agents are stateless: everything dies between sessions, and carrying the full
  history in-context costs O(history) tokens per turn. Memory = compress the
  history outside the window, recall a small relevant slice per turn.
- A search engine answers "which documents are similar". Memory needs more:
  **scope** (whose memory), **temporality** (facts supersede each other, old
  ones stay queryable "as of date"), **recall scoring** (relevance × recency ×
  importance, not similarity alone), **forgetting** (TTL, GDPR erase), and
  **hybrid retrieval** (exact names/IDs where vectors are blind).
- Every one of those maps onto machinery this engine already has: tenants,
  columnar attributes, delete-in-place, idle tasks, BM25/hybrid. VMEM is mostly
  assembly, not construction.

Guiding principle (from lab research on agent memory): **the anchor is
verbatim**. LLM-rewritten memory degrades over rewrite cycles; anything smart
(summaries, profiles, graphs) must be a derived, recomputable layer above the
verbatim store. VMEM v1 is the anchor layer only. This is LSM thinking: anchor
= WAL/segments, derived = indexes, consolidation = compaction.

## Fact contract

One memory record ("fact") maps onto one `VSIM.ADDDOC`-shaped ingest. **No new
fields in the engine format** — everything rides on keys, attributes, and the
BM25 text layer, all of which already flow through WAL / snapshot / merge.

| Field | Purpose | Engine shelf | Decision |
|---|---|---|---|
| `id` | address of the fact (supersedes/FORGET target) | key | server-generated ULID; client MAY pass `ID=` → retry of the same REMEMBER is an upsert (idempotency for free) |
| `scope` | isolation: whose memory | CAT attr `scope` = tenant (contiguous `[lo,hi)` after tenant sort) | required; free-form string (`user:dana`, `agent:7`); flat, no hierarchy in v1 |
| `text` | the fact itself, lexically searchable | `TEXT` | required; no `TITLE` (facts have no titles) |
| `type` | kind of fact (preference/event/task/…) | CAT attr `type` | free-form string, no enum in v1; feeds per-type decay later |
| `importance` | resistance to decay | NUM attr `imp` | float **0–1**, default 0.5 |
| `valid_from` | true since | NUM attr, unix seconds | server-stamped at ingest; client MAY override (importing old logs) |
| `valid_to` | true until | NUM attr | open interval = sentinel `2^53` (not "attr absent"), so `RANGE valid_to now +inf` needs no special case |
| `expires_at` | TTL erasure deadline | NUM attr | **absolute** timestamp; client-relative `TTL=` is converted at ingest (see door 1) |
| `supersedes` | provenance pointer to the replaced fact | CAT attr `supersedes` = old id | always recorded at contract level; the *mechanism* that closes the old fact's `valid_to` is step 4 and independent of this field |
| `vector` | semantic retrieval arm | `VEC`, optional | absent = step 0 of the embedding ladder: the fact lives BM25-only until (if ever) vectorized |

### Semantics pinned by the contract

- **REMEMBER** = full-state ingest of one fact (upsert semantics identical to
  `VSIM.ADDDOC`).
- **RECALL default = valid-now**: facts with `valid_to <= now` or
  `expires_at <= now` are excluded. History is explicit: `AS_OF ts` evaluates
  validity at `ts`; `ALL` disables the validity filter.
- **Two kinds of forgetting, never conflated**:
  - *supersession* (step 4): the fact is no longer true **now**, but history
    stays queryable — this is what buys temporality;
  - *erasure* (`VMEM.FORGET`, TTL reaper): physical delete-in-place, GDPR-style,
    gone from history too. FORGET erases by id and does **not** walk
    supersedes chains (documented limitation).
- Graceful degradation by construction: an unvectorized fact is invisible to
  the vector arm but found by the BM25 arm — hybrid gives step-0 tolerance for
  free.

## Doors (decisions that are expensive to reverse)

1. **Replay never looks at the clock.** Everything time-dependent is stamped as
   an absolute number *at ingest* and travels in the WAL as an ordinary
   attribute. Relative `TTL=` becomes absolute `expires_at` **before** the WAL
   write. Same self-sufficiency contract as "terms, not text": replay is
   bit-exact by construction, not by luck.
2. **VMEM lives in the same store as VSIM** (v1): same keyspace, same WAL, same
   segments — zero new durability machinery. ULID fact keys coexist with user
   VSIM keys. If isolation is ever needed, a key prefix adds it without a
   format break (door stays open).
3. **Embedding ladder in v1 = step 0 + BYO only.** Server-side
   `-embeddings-url` (any OpenAI-compatible endpoint) with a background
   re-vectorization queue is deferred, demand-driven — it is the only piece
   with an external dependency and background state, and the demo value is
   already delivered by step 0.

## Deliberately NOT built (v1)

- LLM fact extraction from dialogue (upper floor — Mem0/Zep territory).
- Knowledge graphs; summary consolidation (derived layers, later, offline and
  reversible if ever).
- Scope hierarchies; multi-text/chunking (a fact is one short text; long
  documents are RAG's job, not memory's).
- Chasing LoCoMo/LongMemEval leaderboards.

## Plan of work

Methodology as in sprint 1: oracle before code, experiments with decision
thresholds fixed **before** running, canon numbers to `BENCHMARKS.md`.

0. **Contract** — this document. ✅
1. **Correctness oracle** — 15–20 hand-written golden scenarios ("agent life"
   mini-stories with known right answers): supersedes chains, `AS_OF` queries,
   scope isolation, expired TTL, unvectorized fact found lexically. Go test
   replays them through the live path across LSM states (delta / flushed /
   mixed), mirroring the BM25 golden harness.
2. **REMEMBER** — thin wrapper over the ADDDOC path: server fields (ULID,
   `valid_from`), TTL→`expires_at` conversion, optional client `ID=`/`VEC`.
   **Status: done (2026-07-20).** Internal `store.Remember` (`vmem.go`):
   a pure "field kitchen" (`rememberDoc`) resolves every server decision —
   ULID (own 30-line encoder, no dependency), defaults, TTL→absolute
   `expires_at` — and returns the materialized doc; the command layer
   (step 7) must serialize *that* doc into the WAL, never re-derive fields
   (door 1). Decisions pinned during implementation:
   - **No-VEC facts get a deterministic placeholder unit vector derived
     from the fact id** (the "trivial placeholder" recipe of the reserved
     has-vector door in `BM25_HYBRID_DESIGN.md`, moved server-side into the
     kitchen). Determinism keeps client-`ID=` retries bit-identical in the
     WAL. On an empty store this fixes `dim=32`; recorded trap: switching
     a step-0 store to BYO embeddings of another dimension requires
     re-ingest into a fresh store.
   - **TTL counts from ingest (`now`)**, not from a client-overridden
     `valid_from`: the override speaks about truth, not retention.
   - `supersedes` is recorded as provenance only; target existence is NOT
     validated at ingest (needs a per-key attr read — step 4 decides).
   - Oracle partially enabled (`TestVMEMOracleIngestParity`): scenario op
     tapes replayed through live `Remember`/`Delete` across 3 LSM states,
     consequences checked through real read paths (`Get` / `SearchText`
     set-parity vs the model / point Eq+Range `SearchFilter` on every
     contract attribute). RECALL queries stay for step 3.
   - **Bonus: pre-existing core bug found by the parity harness and
     fixed** — `SearchFilter` snapshotted attributes of the *active* delta
     only, so docs sitting in *flushing* deltas (flush-visibility window)
     passed through every Eq/Range filter unjudged (cross-tenant leak).
     Fix: snapshot all memtable deltas (active + flushing, freshest wins);
     deterministic regression `TestFlushVisibility_FilterAttrsInWindow`
     (buildSem-pinned window, fails on pre-fix code).
3. **RECALL v1 (correct set, dumb order)** — scoped hybrid + validity filter
   (Range attrs). Correctness first, ranking later — separate layers, debugged
   one at a time.
   - **3a. RESP filter parity** — plumb `[EQ k v]… [RANGE k lo hi]…` into
     `VSIM.SEARCHTEXT` / `VSIM.HYBRID` (mirror of `VSIM.FILTER` syntax). The
     core has had scoped text search since sprint-1 step 3; only the command
     layer lags. Standalone value: multi-tenant RAG over SEARCHTEXT. Pinned
     rule: in HYBRID the filter applies to **both arms before RRF** (filter
     then fuse) — post-fusion filtering starves the fused list; test first.

   **Status: done, incl. 3a (2026-07-20).**
   - `SearchTextFilter` — pre-filter inside each source: memtables judged by
     an attrs snapshot of ALL memtable deltas (active + flushing — the same
     flush-visibility trap fixed in `SearchFilter` at step 2; freshest copy
     wins on key collision), segments by the compiled column predicate
     *inside* posting scoring, before the arm's top-K forms.
     **BM25 statistics stay global** (whole-corpus N/df/avgdl), not
     per-scope — decided 2026-07-20: cheap, stable on tiny scopes (N=5
     gives degenerate IDF), consistent with SearchText and the golden
     oracle. Cost accepted: a tenant-locally-common word scores as
     globally-rare. Revisit only on measured profit (step-8 bench), not on
     "fairer".
   - `SearchHybridFilter` — filter-then-fuse; empty filter degrades to the
     bit-exact old SearchHybrid path. Starvation regression: 300-doc
     foreign tenant vs 2-fact target scope, both arms dominated — target
     facts must all surface (`TestRecallSmallScopeNoStarvation`).
   - `store.Recall` — pure filter kitchen (`recallFilter`): three validity
     modes = one Filter (default ≡ AS_OF now; ALL drops the interval axis);
     erasure (`expires_at`) filtered in **every** mode against `now`, which
     also hides TTL-expired facts before the step-6 reaper exists. Integer
     seconds < 2^53 turn the model's strict inequalities into inclusive
     Ranges via +1 shifts. No query vector → deliberate BM25-only
     degradation (RRF is rank-blind, a placeholder-vector arm would vote
     noise with full weight).
   - `TestVMEMOracleParity` enabled: all scenarios through live
     Remember/Recall ×3 LSM states, set-parity against the executable model
     run in **step-3 semantics** (supersedes does not close `valid_to`
     yet — flag in `vmemReplay`); flips to the full model at step 4.
     Order/`expect_first` remain step 5.
   - RESP: `[EQ…][RANGE…]` in both commands (shared `parseAttrFilter`,
     backward-compatible), `docs/COMMANDS.md` updated.
4. **Supersession mechanism** — the one genuinely hard step. Closing the old
   fact's `valid_to` is a mutation of an already-ingested doc, but upsert =
   full replacement and raw text is not stored. Three candidates, chosen by
   experiment with pre-registered thresholds:
   (a) compute validity at recall via the new fact's `supersedes` pointer
   (no new structures, reads cost more);
   (b) a light validity side-layer `id → valid_to` modeled on tombstone masks
   (cheap writes, new durability surface);
   (c) re-ingest the old fact with a closed interval (simple, needs its text
   from the KV tier).
5. **Recall scoring** — re-rank the hybrid top-100 by
   `score × decay(now − valid_from) × importance`, return top-K. Pure
   rank-layer arithmetic (like RRF), core untouched. Decay shape and per-type
   half-lives chosen by experiment on the golden scenarios.
6. **FORGET + TTL reaper** — delete-in-place (churn already solved) + idle-task
   sweep of `expires_at <= now`.
7. **RESP surface + durability** — `VMEM.REMEMBER / RECALL / FORGET`; both
   command gates (`isMemoryGrowingCmd` + `isWriteCmd`, per `COMMANDS.md`
   checklist); WAL ops only if step 4 chose (b); docs.
8. **Profit bench + canon** — synthetic "agent life" corpus (hundreds of
   sessions, thousands of facts): known-item recall, `AS_OF` accuracy, scope
   isolation, RECALL latency within an agent-turn budget. Success criterion
   framed before code; numeric bar set **after** the first baseline (sprint-1
   lesson: the "hybrid ≥ vector" criterion was ill-posed against cosine GT).
   Canon → `BENCHMARKS.md`.

## Known risks / recorded traps

- **Idempotency**: agent retries of REMEMBER without client `ID=` create
  duplicate facts; documented, client `ID=` is the remedy.
- **Clock skew** on client-supplied `valid_from` is accepted (import
  use-case beats strictness in v1).
- Sprint-1 leftovers that may fire here: WAND stays in backlog — the recorded
  trigger is reframed as "BM25-arm latency on the *measured* distribution of
  memory-recall query lengths stops fitting the agent-turn budget", not "long
  queries exist"; step 8 produces that measurement.
