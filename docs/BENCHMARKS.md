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
| MNIST-784 (fp32) | 976 vec/s | **5 324 vec/s** |
| dbpedia 1536-dim (SQ8) | — | **2 725 vec/s** |

Search over RESP (12 connections, consolidated index):

| Dataset | mode | QPS | recall@10 |
|---|---|---|---|
| MNIST-784, 60k | float32 | 5 743 | 0.9998 |
| MNIST-784, 60k | **SQ8** | **10 093** | 0.9994 |
| dbpedia-openai-100k, 1536-dim | **SQ8** | **3 535** | 0.9902 |

Mixed workload (12 insert + 12 search connections simultaneously,
dbpedia-1536 100k, SQ8): **673 inserts/s** sustained while search holds a
**750–800 QPS** floor for the whole load, then converges to **3 544 QPS @
recall@10 0.9900** once ingest stops — matching the consolidated canon above.
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
server on a fully consolidated single segment measures *higher* — 10 093 QPS
@ 0.9994 at the same ef=100 (see the end-to-end section above) — segment
count, not protocol, is the dominant QPS factor.

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

The heavy benchmarks are committed but gated: they skip under `-short` and
skip silently if the dataset file is missing.

```bash
# 1. Datasets (ann-benchmarks HDF5 → raw bin)
wget http://ann-benchmarks.com/sift-128-euclidean.hdf5
./scripts/convert_annbench.py sift-128-euclidean.hdf5 /tmp/sift200k.bin --train 200000 --test 500
./scripts/convert_annbench.py sift-128-euclidean.hdf5 /tmp/sift1m.bin --test 1000
./scripts/convert_annbench.py gist-960-euclidean.hdf5 /tmp/gist_sub.bin --train 500000 --test 500
./scripts/convert_annbench.py mnist-784-euclidean.hdf5 /tmp/mnist784.bin
# dbpedia (includes angular ground truth, separate format):
# https://storage.googleapis.com/ann-datasets/ann-benchmarks/dbpedia-openai-100k-angular.hdf5
./convert_dbpedia.py dbpedia-openai-100k-angular.hdf5 /tmp/dbpedia100k.bin

# 2. Run (no -short → heavy tests enabled; -v prints the tables)
go test -run 'TestSIFT1M_Validation|TestGIST1M_Validation' -v -timeout 60m ./kvstore/vector/
go test -run 'TestDBpedia_RealEmbeddingValidation' -v -timeout 30m ./kvstore/vector/
go test -run 'TestTenant_SearchTenantQPSGain|TestFilter_AttrScaleQPSGain' -v -timeout 60m ./kvstore/vector/
```

The hnswlib side of the head-to-heads: `pip install hnswlib`, same M/efC/ef
grid, same query files, `index.set_num_threads(1|12)` — any published
ann-benchmarks harness will do; recall must be computed against the same
ground truth.
