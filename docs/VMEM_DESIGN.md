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
| `source` | where the fact came from (channel/tool/document) | CAT attr `source` | **always stamped**; omitted `SOURCE=` writes the literal `unknown` rather than leaving the attribute absent (see below). Filterable via `RECALL … SOURCE s` |
| `vector` | semantic retrieval arm | `VEC`, optional | absent = step 0 of the embedding ladder: the fact lives BM25-only until (if ever) vectorized |

### Semantics pinned by the contract

- **REMEMBER** = full-state ingest of one fact (upsert semantics identical to
  `VSIM.ADDDOC`).
- **RECALL default = valid-now**: facts with `valid_to <= now` or
  `expires_at <= now` are excluded. History is explicit: `AS_OF ts` evaluates
  validity at `ts`; `ALL` disables the validity filter.
- **Revocation is a third axis, not a third meaning of `valid_to`.**
  `VMEM.QUARANTINE` stamps `quarantined_at` and touches nothing else. The two
  tempting alternatives are both wrong, and naming what each axis *means* shows
  why: `valid_from`/`valid_to` are **application time** ("true from … to …"),
  so `valid_to = <revocation moment>` would assert the poisoned fact *was* true
  until then — a lie in the axis's own terms; while `valid_to = valid_from`
  ("never true") is terminologically honest but erases the trace that the agent
  **believed** it, which is exactly the evidence this layer exists to keep.
  With its own axis, application time stays untouched and `ASOF` before the
  revocation still answers "yes, at 14:32 it believed this". This is the first
  concrete step toward the bitemporality already recorded as an open door — not
  a workaround across it.
  - Unlike `valid_to`, `quarantined_at` is **not** stamped on every fact: its
    absence means "never revoked", it is judged in the ranking layer (where a
    missing value is explicit `NaN`) rather than by a pre-filter `RANGE`, and
    leaving it absent means facts written before the feature existed keep
    reading normally — an upgrade must not hide data the user already stored.
- **Three answers to "the memory is wrong", each with its own reach.** They are
  not alternatives to pick one of — an incident normally uses all three:
  `VMEM.QUARANTINE` revokes a belief selectively and reversibly and keeps the
  record; `VMEM.FORGET` erases irreversibly, history included; point-in-time
  restore (`-restore-to-lsn`, see `BACKUP.md`) reproduces the whole store as of
  a moment, read-only, without touching the data directory. Restore is the
  coarse one — it rewinds *everything*, which is exactly why it is a forensic
  backstop and not the repair tool. The repair tool is quarantine.
- **Two kinds of forgetting, never conflated**:
  - *supersession* (step 4): the fact is no longer true **now**, but history
    stays queryable — this is what buys temporality;
  - *erasure* (`VMEM.FORGET`, TTL reaper): physical delete-in-place, GDPR-style,
    gone from history too. FORGET erases by id and does **not** walk
    supersedes chains (documented limitation).
- Graceful degradation by construction: an unvectorized fact is invisible to
  the vector arm but found by the BM25 arm — hybrid gives step-0 tolerance for
  free.
- **Undeclared provenance is a value, not a hole.** `source` is stamped on every
  fact; when the client declares none, the literal `unknown` is written. The
  reason is the operation provenance exists for — revoking by origin. An absent
  attribute matches neither `Eq` nor `Range`, so a fact written without a
  declared source would be invisible to a source-scoped revocation and would
  silently survive it — precisely the failure this layer is meant to prevent.
  Making it explicit turns "nobody vouched for this" into a first-class,
  filterable class: `RECALL … SOURCE unknown` finds all of them.
  - *Honesty boundary:* the stamp happens at ingest, so it describes facts
    written **once provenance existed**. Facts written before this attribute was
    introduced have no `source` column at all (physically absent), which is not
    the same as `unknown` and must not be presented as it — the store observed
    nothing about them. Distinguishing legacy from undeclared is itself
    provenance. There is currently no predicate for "attribute absent"; if
    mass-revoking legacy facts ever matters, that predicate is the missing
    piece, not a relabelling of them into `unknown`.

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

   **DONE — (c) chosen by experiment.** (a) was eliminated by analysis before
   any benchmark (correctness, not perf: the successor is not guaranteed to be
   lexically reachable by the query, so validity computed at recall would need
   a reverse index plus a per-candidate lookup, and a virtual `valid_to`
   breaks pre-filter Range). (c) needs no KV tier after all: the target's
   terms are re-read from the engine itself — O(1) from delta slots, and from
   frozen segments via a compact forward layer `doc → (termID, tf)` built
   alongside the postings (memory ≈ postings size; snapshot format unchanged,
   the layer is rebuilt once on load by inverting postings). The whole
   read-modify-write (read target → re-ingest with closed `valid_to` → insert
   successor) runs under the store's exclusive lock, so a concurrent upsert of
   the target either lands entirely before (its content gets closed) or
   entirely after (it reopens the fact — last writer wins); the torn
   interleaving is impossible. Target is validated (exists, same scope) before
   any insert — an error leaves no half-written pair. Both docs must go to the
   WAL as one atomic batch (step 7). Pre-registered thresholds, both met:
   p99 REMEMBER-with-supersedes = 2.33× plain REMEMBER (threshold ≤5×, target
   in a 20k-doc frozen segment, 20k vocabulary); RECALL QPS A/B vs pre-step-4
   master: no regression (74–84k QPS both sides, same probe). Without the
   forward layer the postings scan cost 7.5× — that variant is rejected.
5. **Recall scoring** — re-rank the hybrid top-100 by
   `score × decay(now − valid_from) × importance`, return top-K. Pure
   rank-layer arithmetic (like RRF), core untouched. Decay shape and per-type
   half-lives chosen by experiment on the golden scenarios.

   **DONE; formulas revised 2026-07-23** after the real-embedding trial
   (`TestVMEMDecayCandidatesJudge`, 20k dbpedia facts with real ada-002
   1536d vectors): the original post-fusion multiplier
   `fused × 2^(−age/halfLife)` mathematically zeroes old facts on the flat
   RRF scale — the best possible fused score (2/61, top-1 in both arms)
   times 2^−4 is smaller than a single-arm rank-100 tail (1/160), so
   hit@10 for facts older than 90 days measured 0.003 *with a perfect
   vector arm*. Age must pay in the same scale the score lives in, and the
   two paths live in different scales:

   - **BM25-only** (wide score scale): `final = score ×
     max(2^(−age/halfLife), 0.25) × (0.5 + importance)` — the 0.25 floor
     keeps decay reordering without quantizing the tail to zero
     (para/>90d hit@10 0.762 → 0.985);
   - **hybrid** (rank scale): fusion itself carries the age penalty,
     `fused = Σ over arms 1/(rrfK + rank + 5·age/halfLife)` (one half-life
     = +5 ranks), then `final = fused × (0.5 + importance)` — known/>90d
     hit@10 0.003 → 1.000, fresh bucket hit@1 improved 0.917 → 0.995.
     Fusion moved from `SearchHybridFilter` into `Recall` (it needs
     `valid_from` before fusing; arms, depth and filter-then-fuse are
     bit-for-bit the same contract).

   Rejected by measurement: one floor for both paths (hybrid stays broken:
   0.938/0.013), one rank penalty for both paths (BM25 para drops to
   0.690 — rank discards BM25's score magnitudes), decay-before-fusion
   (never fixes the BM25 tail). Age is measured against `as_of` when given
   ("what mattered *then*"), neutral importance 0.5 yields exactly 1, and
   neither age nor importance ever zeroes a fact — decay moves rank, only
   erasure hides. Half-life is per-request (`half_life`, default 30 days):
   the mechanism is ours, the policy is the client's; per-type half-lives
   remain an open door (needs type projection).
   Candidate attributes (`valid_from`, `imp`) are batch-projected by key:
   O(1) from memtables, O(log n) from frozen segments via a sorted key
   permutation added to the interned-key table (4 bytes/key, no format
   change, built at freeze/load — it also turned point `Get` and the step-4
   supersedes read from linear scans into binary searches). The golden
   scoring scenarios' `expect_first` is now asserted in the live parity test
   across all three LSM states. Pre-registered threshold: scored RECALL QPS
   ≥ 0.8× of the unscored step-4 path — measured 0.85× (median 52.5k vs
   61.5k on the same probe) after fixing an overfetch-sized allocation
   (result buffers were pre-sized to the fetch depth of 100 while typical
   scope queries return units of hits; ~4 KB of garbage per query gated QPS
   through GC — buffers now grow amortized from a small hint). The known
   risk "flat RRF rank scale vs multiplicative decay" is accepted and
   deferred to the step-8 corpus bench.
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
