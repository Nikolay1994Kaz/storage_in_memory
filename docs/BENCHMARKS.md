# Benchmarks

Measured results of the vector engine on standard ANN datasets and real
embeddings, including same-machine head-to-head runs against
[hnswlib](https://github.com/nmslib/hnswlib) (the reference HNSW
implementation). Every number below comes from a committed, reproducible test —
see [Reproducing](#reproducing).

**Hardware disclaimer.** All runs: Intel i7-9750H (6 cores / 12 threads,
2019 laptop, thermal throttling observed under sustained load). Absolute
numbers are conservative; on server hardware expect higher. The *ratios*
(vs hnswlib on the same machine, filtered vs unfiltered) are the meaningful
part.

**Conventions.** `recall@10` against exact ground truth; `QPS_1` /
`QPS_12` = queries per second with 1 / 12 threads; queries are held-out test
sets, never training vectors. Head-to-head comparisons are **iso-recall**
(same recall, compare speed), the ann-benchmarks methodology.

---

## End-to-end through the server (RESP)

The sections below this one measure the engine in-process. This section
measures the whole product: a real `kvstore-server` process, vectors inserted
over the wire (`VSIM.ADDBIN`), searched over the wire (`VSIM.SEARCHBIN`) with
real held-out query vectors, recall against the official ground truth.
12 client connections, server-default `efSearch=100`, M=32, efC=400.
Same laptop as everything else.

Insert rate over RESP (12 connections):

| Dataset | single delta graph | `-delta-shards 12` |
|---|---|---|
| MNIST-784 (fp32) | 976 vec/s | **5 533 vec/s** |
| dbpedia 1536-dim (SQ8) | — | **3 681 vec/s** |

Search over RESP (12 connections, consolidated index; re-measured after the
fused-ADC rerank landed — previous canon was 10 093 / 3 535):

| Dataset | mode | QPS | recall@10 |
|---|---|---|---|
| MNIST-784, 60k | float32 | 6 190 | 0.9992 |
| MNIST-784, 60k | **SQ8** | **12 985** | 0.9996 |
| dbpedia-openai-100k, 1536-dim | **SQ8** | **3 928** | 0.9888 |

Mixed workload (12 insert + 12 search connections simultaneously,
dbpedia-1536 100k, SQ8): **673 inserts/s** sustained while search holds a
**750–800 QPS** floor for the whole load, then converges to **3 544 QPS @
recall@10 0.9900** once ingest stops — matching the consolidated canon of the
build it was measured on (3 535, pre-ADC-rerank).
SIFT-60k smoke under the same protocol: search floor ~2 350 QPS during
ingest, terminal 13.5–15.5k QPS, zero errors.

Notes, honestly:

- Search numbers are for a **consolidated index** — the steady state. After a
  bulk load the server reaches it by itself: the LSM merge cascade plus idle
  consolidation (`-idle-consolidate`, default 60 s of write silence) collapse
  all segments into one, no manual command. On this laptop: ~2 min after
  60k×784, ~10 min after 100k×1536.
- **No search brownout during loads.** Delta shards freeze into mini-segments
  in milliseconds (instead of a minutes-long graph rebuild on the flush path),
  and merges consume whole level prefixes in one pass (batched L0 merge), so
  search never degrades to scanning dozens of transient graphs. Measured floor
  while 100k×1536 loads under concurrent search: **750–800 QPS** (earlier
  builds dipped to ~34 QPS in this state). One honest limit remains: under
  *continuous saturating* search load, full consolidation to the terminal
  single-segment shape takes a long tail — the final large merge is
  single-threaded and competes with search for cores. In write silence, idle
  consolidation reaches terminal shape in minutes.
- RESP + epoll cost ~10–20% vs the in-process harness. Pipelining
  (`insertload -pipeline`) buys +52% ingest on a single connection; at 12
  connections the server is CPU-bound and pipelining adds nothing.

Load tools: `cmd/insertload`, `cmd/searchload` (raw float32 LE files; use real
query vectors — random queries lie on high-dim data, degenerating HNSW to
brute force).

---

## 1. Real embeddings — dbpedia-openai-100k (ada-002, 1536-dim, cosine)

The closest dataset to the target workload: real OpenAI `text-embedding-ada-002`
vectors with angular ground truth. N=100k, 973 queries, ef=128, K=10, M=32.

| Mode | recall@10 | QPS_12 | index memory |
|---|---|---|---|
| float32 | 0.984 | 1 936 | 614 MB |
| **SQ8 (int8)** | 0.977 | **4 545** | **165 MB** |

**Takeaway:** on high-dimensional real embeddings SQ8 is the clear default —
3.7× less memory and ~2.3× the throughput for −0.7 pp recall. High-dim search
is memory-bandwidth-bound; moving 4× fewer bytes wins more than the extra
dequantization costs.

Test: `TestDBpedia_RealEmbeddingValidation` (`kvstore/vector/dbpedia_validation_test.go`).
Filtered search on the same dataset: `TestDBpedia_FilterTenant`.

## 2. Head-to-head vs hnswlib — GIST-960 (500k, high-dim)

GIST has high intrinsic dimensionality (hard for ANN) and its 960 dims are
representative of transformer embeddings (768–1536). Both engines: M=16,
efConstruction=200, L2, same 500 queries. hnswlib runs float32 (its only mode);
we run SQ8 — comparing each engine's intended production configuration.

| ef | hnswlib fp32 recall / QPS_12 | ours SQ8 recall / QPS_12 | QPS ratio |
|---|---|---|---|
| 32 | 0.677 / 6 333 | 0.700 / 17 191 | 2.7× |
| 64 | 0.808 / 3 667 | 0.815 / 9 968 | 2.7× |
| 128 | 0.898 / 2 167 | 0.895 / 5 512 | **2.5×** |
| 256 | 0.955 / 1 167 | 0.938 / 3 050 | 2.6× |

**Takeaway:** at equal recall the engine is **2.5–2.7× faster than hnswlib
multithreaded, with 4× less vector memory**. The reason is scaling: our SQ8
throughput scales ~5.2× on 6 cores (near-linear) while hnswlib fp32 scales only
~1.8× — 3.75 KB/vector of float32 traffic saturates memory bandwidth, 960 B of
SQ8 does not. Single-threaded the two are within 5–15% of each other.

Known ceiling: SQ8 recall tops out ≈0.94 on GIST at M=16 (quantization floor);
above that use float32 or larger M.

Test: `TestGIST1M_Validation` (`kvstore/vector/step_profit_test.go`).

## 3. Head-to-head vs hnswlib — SIFT-1M (float32, low-dim)

The classic ann-benchmarks dataset: 1M × 128-dim, M=16, efC=200, official
ground truth, iso-recall comparison, both engines float32 (dim 128 is below the
SQ8 payoff threshold).

Segment count is the dominant variable on this dataset, so both operating
points are shown honestly.

**Fresh multi-segment LSM state** — right after a bulk load, before merges
catch up (the validation harness deliberately measures this worst case):

| recall@10 | ours QPS_12 | hnswlib QPS_12 | ratio |
|---|---|---|---|
| ≈0.955 | 5 734 | 18 833 | 0.30× |
| ≈0.986 | 3 517 | 10 500 | 0.33× |
| ≈0.997 | 2 066 | 5 833 | 0.35× |

**Consolidated single segment** — the steady state the server converges to on
its own (merge cascade + `-idle-consolidate`); in the harness: deltaMax=N:

| recall@10 | ours QPS_1 | hnswlib QPS_1 | ours QPS_12 | hnswlib QPS_12 |
|---|---|---|---|---|
| ≈0.96 | 5 115 (0.84×) | 6 082 | **25 697 (1.36×)** | 18 833 |
| ≈0.99 | 2 840 (0.86×) | 3 300 | **14 464 (1.38×)** | 10 500 |

**Honest takeaway:** single-threaded, hnswlib is ~15% faster (its hand-tuned
SIMD vs our AVX2 assembly). Multithreaded on a consolidated index this engine
is **1.36–1.38× faster** — throughput scales ~5.0× on 6 cores vs ~3.2× for
hnswlib. On a fresh fragmented index hnswlib is ~3× faster: fan-out + merge
across segments per query is the architectural price of supporting concurrent
writes, deletes and crash recovery, while hnswlib searches one static
monolith. Which state you live in depends on write pressure — idle
consolidation restores the consolidated shape automatically after write lulls.
If your workload is a static, low-dim, single-tenant index with no durability
needs, hnswlib/Faiss serve that perfectly; the live-workload case is the
target here.

Tests: `TestSIFT1M_Validation` (multi-segment state) and
`TestSIFT1M_SegmentEffect` (deltaMax=50k vs deltaMax=N A/B, QPS_1/QPS_12 per
ef) — both in `kvstore/vector/step_profit_test.go`; hnswlib side: same M/efC/ef
grid, same queries and ground truth.

## 4. Filtered / multi-tenant search

The engine's differentiating path. With `--partition-attr tenant`, vectors are
laid out contiguously per tenant; filtered queries on small tenants route to
brute-force over the tenant block instead of traversing the full graph.
Baseline below = what a non-tenant-aware engine does (full HNSW traversal with
a post-predicate and widened ef). **recall = 1.0 on both sides of every row** —
these are iso-recall speedups.

SIFT-200k, tenant of size B (`TestTenant_SearchTenantQPSGain`):

| tenant size | baseline QPS | tenant-routed QPS | speedup |
|---|---|---|---|
| 1 000 | 115 | 113 183 | **984×** |
| 5 000 | 419 | 23 053 | 55× |
| 16 000 | 886 | 5 176 | 5.8× |
| 50 000 (graph route) | 1 828 | 1 756 | ≈1× (same path) |

SIFT-1M, columnar attribute filters via `VSIM.FILTER` (`TestFilter_AttrScaleQPSGain`):

| filter | selectivity | speedup vs string-predicate baseline |
|---|---|---|
| `EQ tenant` | 1k of 1M | **7 350×** |
| `EQ tenant` | 16k of 1M | 48.9× |
| `EQ tenant ∧ EQ region ∧ RANGE price` | 125 of 1M | **28 620×** |
| `EQ tenant ∧ …` | 2k of 1M | 735× |
| large tenant (graph route) | 50k–200k | 1.1–1.3× |

**Takeaways:** the brute-route advantage *grows* with N (the baseline's full
graph traversal collapses as the graph grows; brute stays O(tenant block)).
Extra predicates make it faster, not slower (they shrink the block at O(1) per
vector). Large tenants fall back to graph traversal at parity — the routing
crossover is automatic (dim-aware threshold).

## 5. Small-scale operating point — MNIST-784 (60k)

Serve config (M=32, efC=400, SQ8, heuristic on), cache-resident dataset —
the ceiling when memory bandwidth is not the constraint:

| efSearch | recall@10 | QPS_12 |
|---|---|---|
| 50 | 0.997 | 11 337 |
| 100 (server default) | 0.999 | 6 421 |
| 200 | 0.999 | 3 605 |

Test: `TestStep6_CurrentStateQPS` (`kvstore/vector/step_profit_test.go`).

Note: this harness searches a multi-segment index. End-to-end through the
server on a fully consolidated single segment measures *higher* — 12 985 QPS
@ 0.9996 at the same ef=100 (see the end-to-end section above) — segment
count, not protocol, is the dominant QPS factor.

## 6. Lexical BM25 + hybrid RRF — dbpedia-openai-100k with texts

Self-contained dataset from HF `dbpedia-entities-openai-1M` (the ann-benchmarks
100k sample carries no texts and does not match HF rows): corpus = first 100k
rows (title + text + ada-002 embedding), queries = next 1000 held-out rows,
GT = exact brute-force cosine top-100 (`scripts/convert_dbpedia_hf.py`).
One consolidated segment, float32, ef=128, K=10. Fusion: `VSIM.HYBRID` =
top-100 lexical + top-100 vector → RRF (k=60). Ingest uses the `TITLE` field
boost (poor-man's BM25F: title terms weighted ×3); search applies query-side
common-term pruning (terms with df > N/2 dropped when N ≥ 1000). Both were
adopted only after an A/B experiment on this dataset
(`bm25_boost_experiment_test.go`): the boost took hit@1 0.846 → 0.906 with no
full-text recall loss, and pruning multiplied short-query QPS ~7× with zero
change in hit@1/hit@10/MRR.

**Known-item** (query = exact entity title; all 100k titles unique). This is
the class of queries the embedder-free `VSIM.SEARCHTEXT` exists for — without
an embedder the vector path cannot serve them at all:

| hit@1 | hit@10 | MRR@10 |
|---|---|---|
| 0.906 | 0.994 | 0.941 |

**Semantic** (1000 held-out queries, recall@10 vs cosine GT):

| vector | text (title) | text (full doc) | hybrid (title) | hybrid (full doc) |
|---|---|---|---|---|
| 0.994 | 0.158 | 0.306 | 0.570 | 0.539 |

Read the hybrid column correctly: the only GT this dataset has is exact
*cosine* — by construction the ideal output of the vector arm. Equal-weight
RRF interleaves two weakly-overlapping lists (v1,t1,v2,t2,…), so a fused
top-10 keeps ~5–6 vector docs and recall vs cosine-GT has a ~0.5–0.65 ceiling
*regardless of implementation quality*. The measured 0.53–0.56 sits inside
that predicted band — fusion behaves exactly as RRF arithmetic dictates.
Demonstrating a genuine hybrid *win* on semantic queries requires
human-labeled relevance (BEIR-class), which this dataset lacks; the
known-item table above is where the lexical arm's product value shows.

**Throughput** (same harness, 12 workers):

| path | QPS_12 |
|---|---|
| `VSIM.SEARCH` (vector) | 2 461 |
| `VSIM.SEARCHTEXT`, short query (~2–3 terms) | **7 958** |
| `VSIM.SEARCHTEXT`, full-doc query (~52 terms) | 234 |
| `VSIM.HYBRID`, full-doc text arm | 218 |

The first run of this bench measured short-query `SEARCHTEXT` at 934 QPS —
*slower* than vector search, against the design expectation. Root cause:
every query term scans its full posting list, and common English terms
(df 60–95% of the corpus) dominated the scan while carrying idf ≤ 0.5 —
near-noise. Query-side pruning of df > N/2 terms fixed it: 8.5× on short
queries (now 3.2× *faster* than vector search, as designed), 4.7× on
full-doc queries, with bit-identical known-item quality. The threshold is
deliberately conservative — on this corpus the *content* word "county" sits
at df=35% (idf≈1.0), so pruning the 25–50% band would eat real signal. Long
full-text queries remain postings-bound; WAND/max-score is the next lever if
they ever matter.

Test: `TestBM25HybridProfit` (`kvstore/vector/bm25_hybrid_profit_test.go`).

## 7. VMEM agent-memory layer — temporal quality and end-to-end latency

`VMEM.*` (REMEMBER / RECALL / FORGET — scoped facts with validity intervals,
supersession, TTL erasure and recency×importance scoring) is judged by two
benches fed by the **same deterministic generator** (`internal/vmemcorpus`,
seed=42), so the quality and latency numbers describe one world: a store-level
quality bench on compressed virtual time, and an end-to-end latency bench over
RESP on real clocks. The corpus is an "agent life": 200 scopes (Zipf sizes),
27 561 events over 180 virtual days — 800 supersession chains of 2–5 versions,
3k TTL facts (the reaper harvests 1 470 of them mid-run), 600 FORGETs — and
5 760 queries of six sorts with ground truth built into the tape.

**Quality** (`TestVMEMCorpusBench`, stage0 = BM25-only — the embedder-free v1
default). Right column = the accepted floors; temporal correctness and
isolation are invariants, not percentages:

| query sort | n | measured | floor |
|---|---|---|---|
| known-item | 2 000 | hit@1 0.982 · hit@10 1.000 · MRR 0.991 | hit@1 ≥0.85, MRR ≥0.90 |
| paraphrase | 2 000 | hit@1 0.974 · hit@10 1.000 · MRR 0.987 | — |
| AS_OF (point-in-time) | 1 000 | accuracy 1.000 | =1.000 |
| now-over-chain (supersession) | 300 | accuracy 1.000 | =1.000 |
| importance ordering | 60 | pairwise 1.000, full order 60/60 | — |
| erasure + scope isolation | 400+ | 0 violations | =0 |

Recency decay reorders but does not evict old truth: known-item hit@1 by fact
age is 1.000 (<30 d) / 0.984 (30–90 d) / 0.974 (>90 d).

**Decay formula, judged on real embeddings** (`TestVMEMDBpediaLifeDecay`: 20k
dbpedia facts, real ada-002 1536-dim vectors, ages spread over 180 d, 5 LSM
segments). The naive `RRF × 2^(−age/HL)` multiplier mathematically zeroes old
facts on the hybrid path — known-item hit@10 for >90 d facts measured **0.003**
(max fused RRF score × 2⁻⁵ sinks below any single-arm rank-100 candidate); no
embedder quality can fix a scale mismatch. Shipped scoring was picked by
judging 7 candidate formulas on this dataset: the BM25 path multiplies by
`max(2^(−age/HL), 0.25)` (floored decay), the hybrid path applies age as a
rank penalty `Σ 1/(k + rank + 5·age/HL)`. After the fix, hybrid+decay
known-item **hit@10 = 1.000 in every age bucket** (hit@1 0.995 / 0.840 /
0.618 — freshness still reorders, no longer erases); BM25 paraphrase >90 d
holds hit@10 0.985.

**End-to-end latency over RESP** (`cmd/vmemload` replays the same seed=42 tape
against a real server process, 8 connections; this laptop on the *powersave*
governor — conservative):

| phase | p50 | p99 | throughput |
|---|---|---|---|
| RECALL, settled store (5 760 queries) | 110 µs | **0.29 ms** | 64 426 QPS |
| RECALL under mixed load (8 readers + 2 writers, 30 s) | 2.9 ms | **13.3 ms** | 2 084 QPS |
| writes under mixed load | 3.8 ms | 10.4 ms | 509 op/s |
| REMEMBER, bulk replay concurrent with merge cascade | 16.5 ms | 49 ms | 339 op/s |

Accepted latency floors, all met: clean RECALL p99 ≤ 1 ms (measured 0.29 ms);
RECALL p99 under mix ≤ 25 ms (13.3 ms); scope-isolation violations = 0
(⚠ a *correctness* property — the engine never returns another scope's fact by
mistake — and **not** an access-control boundary: `AUTH` is one shared secret
with no per-principal authorization, so any authenticated connection may
address any scope by name. Trust boundary = process. See `MEMORY_GOVERNANCE.md`
primitive 6)
(checked E2E on every returned id, 0 of 9 219); errors = 0 (across 77 777 mix
operations and all recalls). REMEMBER latency is fsync-bound (durable WAL per
batch), not CPU-bound.

Tests: `TestVMEMCorpusBench` (`kvstore/vector/vmem_corpus_bench_test.go`),
`TestVMEMDBpediaLifeDecay` (`kvstore/vector/vmem_dbpedia_life_test.go`),
`cmd/vmemload`. Design: `docs/VMEM_DESIGN.md`.

---

## 8. Retrieval quality on a public agent-memory benchmark — LongMemEval

Sections 1–7 measure our index against our own ground truth. This one answers
the question a buyer asks first — *how well does the memory find the right
thing?* — on a public benchmark, against a published number.

**Setup.** LongMemEval_S (`xiaowu0162/longmemeval-cleaned`): 500 questions,
each with its own haystack of ~48 chat sessions (23 796 session-documents,
18 362 unique). The protocol is copied line-for-line from the MemPalace harness
that published 96.6% R@5: a session's document is **user turns only** joined
with `\n`; sessions with no user turn are dropped; the metric is
`recall_any@5` — *is at least one labelled session in the top 5?*; the embedder
is `all-MiniLM-L6-v2` (`max_seq_length=256`, so a ~9.6k-character session is
truncated to roughly its first tenth — theirs is too). **No LLM is invoked
anywhere**: ground truth ships in the data as `answer_session_ids`.

**Calibration first.** Our `exact` arm — a plain NumPy cosine scan, which is
also what ChromaDB does at a 48-document pool — reproduces **96.6%**, the
published figure. That is what makes the rest of the table comparable: same
corpus, same truncation, same metric, same embedder. A benchmark that first
reproduces someone else's number and then prints its own is a different object
from one that only prints its own.

| arm | R@5 | what it is |
|---|---|---|
| `exact` | 96.6% | embedder ceiling = the published baseline, reproduced |
| `vsim` | **96.6%** | our vector path (`VSIM.FILTER`, columnar tenant filter) |
| `bm25` | 96.2% | `VMEM.RECALL` without `VEC` — the lexical arm alone |
| `vmem` | **97.4%** | `VMEM.RECALL` — hybrid + decay + importance |
| control — foreign question | 18.6–22.2% | same pool, another question's text/vector |
| control — analytic random | 10.5% | 5/N |

**97.4% against 96.6% is 487 versus 483 questions out of 500 — parity, not a
lead.** The value is that the number exists and lines up, not that it is bigger.

**What the average hides.** Per question type the lexical arm is a trade, not a
free win:

| type | n | `exact`/`vsim` | `bm25` | `vmem` |
|---|---|---|---|---|
| knowledge-update | 78 | 100.0 | 100.0 | 100.0 |
| multi-session | 133 | 99.2 | 98.5 | 99.2 |
| single-session-assistant | 56 | 96.4 | 96.4 | 96.4 |
| **single-session-preference** | 30 | **96.7** | **73.3** | **80.0** |
| single-session-user | 70 | 91.4 | 98.6 | **98.6** |
| temporal-reasoning | 133 | 94.7 | 95.5 | **97.7** |

Fusion buys +7.2 points on `single-session-user` and +3.0 on
`temporal-reasoning`, and pays **−16.7 on preferences**: preferences are stated
indirectly, the question shares no distinctive terms with the session, BM25
alone scores near-noise there (73.3%), and RRF lets it displace the correct
vector hit. Known, unfixed, and invisible in the aggregate.

**Recency decay costs 0.2 points here — and this benchmark cannot test it.**
Inserting every session with its real date (`VALIDFROM`) changes all 500 top-1
scores, so the setting demonstrably took effect, yet R@5 does not move. The
reason is measured, not assumed: the **median date spread inside one haystack
is 11 days** against a 30-day half-life, so all candidates age together and the
rank penalty does not reorder them. Nothing about freshness should be claimed
from this dataset.

**One dataset artifact, named and subtracted.** Querying with `ASOF` = the
question's own date drops the temporal row to 82.7%. That is the data, not the
engine: 1 471 haystack sessions are dated **after** the question that asks
about them, and for 20 questions *every* labelled session is. All 20 are
`temporal-reasoning`, and 20 of 133 accounts for the drop exactly. Over the 480
questions where `ASOF` still leaves an answer reachable: 97.1%, against 97.3%
without it.

**How far this evidence reaches** — the harness prints this block on every run,
so the limits travel with the numbers instead of living in someone's notes:

- **Safe to quote — `exact` / `vsim`.** An independent oracle checks `vsim`
  against the exact scan **per question**, not on the mean (two different
  distributions can share a mean). Vectors are rounded to 6 decimals *before*
  both arms, so a rounding difference cannot masquerade as an engine defect.
- **Quote with a caveat — `vmem`.** Negative controls run through the engine,
  output completeness and id resolution are asserted, but there is **no
  independent oracle**: verifying RRF fusion needs a second implementation.
- **Do not quote `bm25` against the academic BM25 baseline (~70%).** Leakage is
  refuted by the control, but the gap itself is unexplained.
- **Do not quote the per-type table as agreeing with theirs.** Only the
  aggregate reproduces exactly; per bucket we differ by 1–3 questions in both
  directions — most likely ONNX (theirs) versus PyTorch (ours) MiniLM — and
  that was not separately verified.

Instrumentation catches what would otherwise be silent: 3 `bm25` and 6 `ASOF`
queries return fewer than 5 candidates, which *understates* those arms, and
every returned id resolved to a session (0 lost). The first run of `vsim`
returned a flat 0% and the oracle caught it — the cause was ours, not the
engine's: `VSIM.SEARCHFILTER` in KV mode looks up `<field>:<key>` in the KV
store and does not see columnar `CAT` attributes at all. The columnar filter is
`VSIM.FILTER`.

Harness: `scripts/longmemeval_bench.py` (self-contained; oracle, two negative
controls and a no-op check are part of the run, and any of them failing exits
non-zero).

---

## Caveats, all in one place

- **Laptop hardware, thermal throttling** — absolute QPS conservative, ratios reliable.
- **Background segment builds are single-threaded**: ~3 000 vec/s (SIFT-128
  fp32), ~700 vec/s (GIST-960 SQ8) on this machine vs hnswlib's parallel build
  at ~7 800 vec/s. The flush path itself no longer rebuilds (per-shard freeze,
  milliseconds), and sharded delta ingest (`DeltaShards`) recovers ~4× under
  concurrent writers — but merge/build speed vs a parallel builder is a known,
  accepted gap, and it is why time-to-peak-QPS under continuous saturating
  read load is long (see the end-to-end section).
- **Filtered-search wins assume a consolidated index** (few segments). Heavy
  recent-write churn fragments tenant blocks across segments until merge
  catches up; idle consolidation (`-idle-consolidate`) restores the
  consolidated shape automatically after write lulls.
- **SQ8 recall ceiling** ≈0.94 on the hardest dataset (GIST@M16); real
  transformer embeddings measure higher (0.977 on dbpedia@M32).
- **ANN is approximate**: under churn a small fraction of stored vectors can
  temporarily miss from top-K (measured ~2% on soak); `VSIM.EXISTS` is the
  exact membership check.
- **LongMemEval (section 8) does not exercise ANN at all.** Each question
  retrieves from its own ~48-document pool, so neither we nor the published
  baseline approximate anything — the number measures the embedder and our
  fusion, not the index. It is the right answer to "does retrieval work", and
  the wrong instrument for "does the index scale".

## Reproducing

The heavy benchmarks are committed but gated behind the `datasets` build tag:
they are excluded from a normal `go test` run entirely, and inside they still
skip if the dataset file is missing.

> The tag was added on 2026-08-02. Before it, these tests were merely
> `-short`-gated and skipped **silently** when the data was absent — so a green
> `make test-full` on a machine with an empty `/tmp` meant "30 functions never
> ran", indistinguishable from "30 functions passed". Run them with
> `make test-datasets` (or `go test -tags datasets ./kvstore/vector/`).

```bash
# 1. Datasets (ann-benchmarks HDF5 → raw bin)
wget http://ann-benchmarks.com/sift-128-euclidean.hdf5
./scripts/convert_annbench.py sift-128-euclidean.hdf5 /tmp/sift200k.bin --train 200000 --test 500
./scripts/convert_annbench.py sift-128-euclidean.hdf5 /tmp/sift1m.bin --test 1000
./scripts/convert_annbench.py gist-960-euclidean.hdf5 /tmp/gist_sub.bin --train 500000 --test 500
./scripts/convert_annbench.py mnist-784-euclidean.hdf5 /tmp/mnist784.bin
# dbpedia (includes angular ground truth, separate format):
# https://storage.googleapis.com/ann-datasets/ann-benchmarks/dbpedia-openai-100k-angular.hdf5
./scripts/convert_dbpedia.py dbpedia-openai-100k-angular.hdf5 /tmp/dbpedia100k.bin
# dbpedia with texts for BM25/hybrid (downloads HF parquet shards on first run):
python3 scripts/convert_dbpedia_hf.py   # → /tmp/dbpedia100k_text.* + queries jsonl

# 2. Run (-tags datasets → these tests exist at all; -v prints the tables)
go test -tags datasets -run 'TestSIFT1M_Validation|TestGIST1M_Validation' -v -timeout 60m ./kvstore/vector/
go test -tags datasets -run 'TestDBpedia_RealEmbeddingValidation' -v -timeout 30m ./kvstore/vector/
go test -tags datasets -run 'TestTenant_SearchTenantQPSGain|TestFilter_AttrScaleQPSGain' -v -timeout 60m ./kvstore/vector/
go test -tags datasets -run 'TestBM25HybridProfit' -v -timeout 60m ./kvstore/vector/

# 3. VMEM (section 7)
# quality — self-contained synthetic corpus, no external data needed:
go test -run 'TestVMEMCorpusBench' -v -timeout 30m ./kvstore/vector/
# decay on real embeddings (builds /tmp/vmemlife.* from the HF dbpedia shards;
# needs pyarrow):
python3 scripts/prep_vmemlife.py
go test -run 'TestVMEMDBpediaLifeDecay' -v -timeout 60m ./kvstore/vector/
# end-to-end latency over RESP (fresh server in an empty dir, then the bench):
go build -o /tmp/kv ./kvstore/cmd/kvstore
(cd "$(mktemp -d)" && /tmp/kv -port 6390 -metrics-port 0 &)
go run ./kvstore/cmd/vmemload -addr 127.0.0.1:6390 -mix 30s
# stop the server with `kill $(pidof kv)` (an exact-name kill, not a substring pkill)

# 4. LongMemEval retrieval quality (section 8) — Python, no Go tags involved
python3 -m venv .venv && ./.venv/bin/pip install sentence-transformers numpy
mkdir -p scratch/longmemeval
curl -L -o scratch/longmemeval/longmemeval_s_cleaned.json \
  https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned/resolve/main/longmemeval_s_cleaned.json
go build -o kvstore-server ./kvstore/cmd/kvstore   # the harness starts its own servers
./.venv/bin/python scripts/longmemeval_bench.py                              # all arms
./.venv/bin/python scripts/longmemeval_bench.py --limit=20 --arms=exact,vsim # smoke
# Pass flags as --name=value: the harness imports scripts/multiagent_sim.py,
# which parses sys.argv at import time.
# The first run embeds 18 362 documents (~7 min on 12 cores) and caches them to
# scratch/longmemeval/emb_*.npz; later runs take ~4 min, all of it engine time.
```

The hnswlib side of the head-to-heads: `pip install hnswlib`, same M/efC/ef
grid, same query files, `index.set_num_threads(1|12)` — any published
ann-benchmarks harness will do; recall must be computed against the same
ground truth.
