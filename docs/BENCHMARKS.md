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

**Agent-memory results.** Earlier versions carried an agent-memory layer on top
of this engine. Its retrieval-quality results (LongMemEval_S 97.4% R@5, LoCoMo
64.6% recall@5) were measured on that layer, which has since been removed from
the project; the numbers, scripts and methodology stay in the `v0.4.0` tag.

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
- RESP + network cost ~10–20% vs the in-process harness (measured with the
  earlier epoll layer; the goroutine-per-connection server that replaced it
  is at parity, see section 7). Pipelining
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

## 7. Removing the custom allocator and the epoll layer

The server used to carry two pieces of infrastructure built before any
profile asked for them: a TCMalloc-style allocator (per-worker MCache, size
classes, QSBR reclamation) under the KV store, the sorted-set B+-tree and the
HNSW neighbor lists, and its own per-worker epoll loop on top of Go's
netpoller, whose one real job was a stable worker id for that allocator. Both
were replaced and measured A/B. Same laptop as above; A and B runs interleaved
(A, B, A, B, …); `redis-benchmark` 7 with 4 client threads on the same
machine; medians of 3 rounds (2 for the 1M-key run), AC power, background
load average ~8 of 12 — differences under ~10% are inside the noise.

**KV store, in-process** (`go test ./kvstore/internal/store/kv -bench .`;
the old side is the same benchmark bodies against `TCMallocStore` at
`bd60927`; battery power, n=6, p=0.002):

| | TCMalloc arena | sharded `map` |
|---|---|---|
| GET, 1 thread | 162 ns, 2 allocs | **49 ns, 0 allocs** |
| GET, 12 threads | 28 ns | **11.5 ns** |
| SET (overwrite) | 138 ns | **79 ns** |
| full GC with 1M live keys | **0.7 ms** | 29 ms |

The arena's single win is the full-GC price with a million live keys. Whether
that matters is answered by the server run below, not by this table.

**Whole server with ~950k live keys** (64-byte values; `ab_server.sh gc1m`;
`bd60927` vs the current tree):

| | before | after |
|---|---|---|
| GET rps / p99 | 210k / 1.04 ms | **222k / 0.59 ms** |
| SET rps / p99 | 194k / 1.33 ms | **210k / 0.76 ms** |
| RSS | ~500 MB | **~368 MB** |
| concurrent GC marking per ~12 s window | **8–15 ms** | 128–133 ms |
| longest stop-the-world pause | 0.32 / 1.73 ms | 0.36 / 0.16 ms |

The map pays ~120 ms more background marking per 12 s (about 1% of one core)
and nothing on latency: pauses stay sub-millisecond, p99 and memory improve.

**Whole server, small key space** (`ab_server.sh load`, 100k keys, 20-byte
values): rps within −0…+9% for the new tree in every scenario (50 and 500
clients, 16-command pipelines, PING); p99 lower in 6 of 7 (e.g. SET at 50
clients 1.01 → 0.73 ms, PING 0.69 → 0.38 ms), higher for pipelined GET
(1.53 → 1.78 ms); RSS at 500 clients 182–201 → 120–150 MB.

**Network layer alone** (same store on both sides, epoll at `253f354` vs
goroutine-per-connection): differences −9…+25% in both directions with
overlapping ranges — parity. Threads at 500 clients 27–29 → 22–23. The price
of a goroutine per connection is its stack: with **10 000 idle connections**
RSS is 148 MB with epoll and 311 MB without it (stacks are 16 KB right after
the first command and shrink to ~4 KB at the next GC: 157 → 40 MB in use).

**Vector engine.** The HNSW neighbor lists moved into an arena owned by each
graph. Graph build time is unchanged (2000 vectors, dim 128 and 768:
p=0.80 and p=0.96, n=8), bytes allocated per build −28% / −8%, upsert churn
−21% (p=0.015). Two memory defects went away with the allocator: a merge
built its temporary graph in pooled allocators that were never reset, so
with a constant 2000 vectors the heap grew +17.9 MB over 6 merge rounds
(now +1.0 MB, `TestMergeReleasesGraphMemory`); and every store held 12+
empty allocators at 5.3 MB each, kept alive forever by their background
goroutines — the vector test suite peaked above 7 GB and now peaks at 412 MB.

**Defects the old layers hid**, each reproduced on `bd60927` before the fix:
TLS mode (`-tls-cert`) panicked on the first connection (`*tls.Conn` cast to
`*net.TCPConn`), and fixing the cast alone leaves pipelined TLS records stuck;
`ZRANGEBYSCORE` returned 1 of 300 members sharing a score once they spanned
several B+-tree leaves (and `VSIM.SEARCHRANGE` lost candidates the same way);
`ZADD` accepted `nan`; after hitting `-maxmemory` once, writes were refused
until restart even with every key deleted; one `EMFILE` from `Accept` stopped
the listener for good.

```bash
git worktree add /tmp/kv-before bd60927 && (cd /tmp/kv-before && go build -o /tmp/kv-before.bin ./kvstore/cmd/kvstore)
go build -o /tmp/kv-after.bin ./kvstore/cmd/kvstore
scripts/ab_server.sh load    /tmp/kv-before.bin /tmp/kv-after.bin 3
scripts/ab_server.sh gc1m    /tmp/kv-before.bin /tmp/kv-after.bin 2
scripts/ab_server.sh idle10k /tmp/kv-epoll.bin  /tmp/kv-after.bin   # epoll side: build at 253f354
go test ./kvstore/internal/store/kv -run '^$' -bench . -benchmem
go test ./kvstore/vector -run TestMergeReleasesGraphMemory -v
```

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
```

The hnswlib side of the head-to-heads: `pip install hnswlib`, same M/efC/ef
grid, same query files, `index.set_num_threads(1|12)` — any published
ann-benchmarks harness will do; recall must be computed against the same
ground truth.
