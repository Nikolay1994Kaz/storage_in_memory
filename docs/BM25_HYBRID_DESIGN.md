# BM25 / Hybrid Search — Design (v1)

Status: **approved design, pre-implementation** (2026-07-18).
Scope: lexical (BM25) search and BM25+vector hybrid fusion inside the existing
LSM vector engine. This is sprint 1 of the memory-engine track; the future
`VMEM.*` layer will call these paths internally.

## Motivation

- Hybrid BM25+vector retrieval is table stakes for retrieval engines in 2026;
  pure-vector search misses exact names, IDs, and rare terms.
- The agent-memory direction (VMEM) needs a lexical tier that works with **no
  embedder configured** ("step 0" of the embedding ladder).
- All required machinery (per-segment columns, dict interning, tenant-sorted
  layout, idle tasks, WAL/snapshot lifecycle) already exists.

## Decision: custom per-segment inverted index (not bleve)

An external engine (bleve) would be a second source of truth that must be kept
in sync across flush, merge, freeze-per-shard, idle consolidation, atomic
`LoadBinary`, WAL replay, tombstones and shadowed upsert copies. Instead we
build a small per-segment inverted index modeled directly on the columnar
attribute layer (`attr.go`): built at segment build time from entries, aligned
to the frozen row order, rebuilt on merge by decoding, immutable afterwards.

Decisive advantage: after `sortEntriesByTenant` a tenant is a contiguous range
`[lo, hi)` of row indices. Postings keep doc indices sorted, so scoped lexical
search = binary search of the range bounds inside each postings list. Scoped
RECALL (the hot path for VMEM) is nearly free; an external engine cannot use
this layout.

Estimated size: ~1k lines + tests (tokenizer ~100, segment build ~150,
query/scoring ~200, delta path ~100, WAL/snapshot ~150, fusion+commands ~150).

## Data structures (per segment)

```
segmentText {
    dict     *termDict     // term string -> local termID (same pattern as attrDict)
    postings [][]posting   // postings[termID] = sorted-by-docIdx []{docIdx uint32, tf uint16}
    docLen   []uint32      // tokens per doc, aligned to frozen row order
    totalLen uint64        // for avgdl
}
```

- Term dictionaries are **per-segment** (like attribute dicts): no global mutex
  on the hot Add path.
- The engine stores **postings only, not raw text**. Merge rebuilds postings by
  decoding `(term, tf, docLen)` from inputs — same as attribute columns; raw
  text is not needed for the lifecycle. Returning text to clients is the job of
  the KV tier / future VMEM layer.
  - Accepted cost: changing the tokenizer/stemmer cannot re-index existing
    data; it requires re-ingest. Documented, not worked around.

## Global IDF across segments

BM25 needs corpus-wide document frequencies; dictionaries are per-segment. The
resolution is cheap because only the **query's terms** need DF: per query,
resolve each term in each segment's dict (plus the delta index), sum DFs, sum
live doc counts → global IDF → score each segment's postings with the global
IDF. O(query terms × segments) hash lookups per query.

Accepted inaccuracy: shadowed upsert copies inflate DF until compaction —
exactly the semantics already accepted for `Info().count` (see `api.go`).

## Tokenization

- Unicode tokenizer (letter/digit runs), lowercase. Hand-rolled, no deps.
- Snowball stemming for Russian and English (small pure-Go dep,
  `kljensen/snowball`), enabled by default, flag to disable.
- Kazakh: **no stemming in v1** (agglutinative; no quality Go stemmer exists).
  Exact word forms only; documented limitation. Future door: per-scope language
  config.

## Delta path

Small mutable inverted map in the delta (`map[term][]docRef` with tf), updated
on Add. The delta is size-capped, so this stays small. Visibility follows the
existing rules: tombstones mask, delta entries shadow segment entries.

## Query path & fusion

Lexical top-N per source (delta + each segment) with global IDF, merged with
the same provenance discipline as vector search: provenance dedup by rank for
visible duplicates, memtable `Contains` shadowing for invisible ones,
tombstone masking.

Hybrid: top-100 lexical + top-100 vector → **Reciprocal Rank Fusion** (k=60)
→ top-K. RRF is rank-based, so no score calibration between BM25 and cosine
is needed.

## API (VSIM tier — not frozen, grows with roadmap)

| Command | Form |
|---|---|
| `VSIM.ADDDOC` | `VSIM.ADDDOC key TEXT <text> [CAT k v]… [NUM k v]… VEC v1 … vN` |
| `VSIM.SEARCHTEXT` | `VSIM.SEARCHTEXT K <query> [EQ attr val]… [RANGE attr lo hi]…` |
| `VSIM.HYBRID` | `VSIM.HYBRID K TEXT <query> VEC v1 … vN [EQ …]` |

- `ADDDOC` tokenizes on ingest; `(term, tf)` travels with the delta entry
  (like `Attributes`), postings are built at flush/merge.
- `SEARCHTEXT` is the embedder-free "step 0" path.
- Existing commands are untouched; KV tier stays frozen.

## Durability

- New WAL op for `ADDDOC` (pattern: `OpVSimAddAttrs`).
- Snapshot format version bump; postings serialized like attribute columns.
  Follow FORMAT_COMPAT policy (actionable bad-magic, atomic all-or-nothing
  `LoadBinary`).

## Reserved door: docs without vectors

Full "step 0" implies text-only docs (no embedding at all). The frozen layer
is vector-aligned throughout (ADC, brute paths, dim checks), so v1 does NOT
implement vector-less docs. The segment format however **reserves a
has-vector flag** so the door stays open without a future format break. Until
then, embedder-free operation is achievable via a trivial client-side
placeholder vector or a cheap server-side embedder.

## Benchmarks (built BEFORE the implementation)

1. **Correctness oracle** — Python `rank_bm25` over a small fixed corpus →
   golden top-K file; Go test asserts parity (ties tolerated). Same spirit as
   the soak durability oracle.
   **Status: built (2026-07-18).** `scripts/bm25_oracle.py` generates
   `kvstore/vector/testdata/bm25/golden.json` from `corpus.json` (14 docs,
   12 queries, ru/en/kk + edge cases); ranking cross-checked against
   `rank_bm25.BM25Okapi`. Pinned contract (must match the Go implementation
   bit-for-bit):
   - scoring: Lucene BM25, `idf = ln(1 + (N−df+0.5)/(df+0.5))`, k1=1.2, b=0.75;
   - tokenizer: lowercase; runs of unicode letters/digits; per-token language:
     kazakh-specific letters (әғқңөұүһі) → no stem, else cyrillic → snowball-ru,
     latin → snowball-en;
   - known snowball-ru quirks captured as test cases: proper-noun cases stem
     apart («Астана»→`аста` vs «Астане»→`астан`, q12) and the stem collision
     «Дана»/«данных»→`дан` (q02).
   Go side: `bm25_oracle_test.go` — golden integrity test active now; parity
   test skipped until steps 2–3 land.
2. **Hybrid profit bench** — dbpedia-openai with original texts. The local
   ann-benchmarks HDF5 has vectors only; data prep step: download the
   HuggingFace `dbpedia-entities-openai-1M` subset (title + text + embedding,
   ~100k rows), extend the converter to emit texts alongside vectors.
   **Status: data prep done (2026-07-18).** The ann-benchmarks 100k sample
   does NOT match the HF rows (cos≈0.6–0.7 row-wise), so the dataset is
   self-contained instead: `scripts/convert_dbpedia_hf.py` takes the first
   100k HF rows as corpus, the next 1000 as held-out queries, computes exact
   brute-force top-100 cosine GT, and emits `/tmp/dbpedia100k_text.bin`
   (same layout as `convert_dbpedia.py` → `loadDBpediaRaw` works unchanged)
   plus `/tmp/dbpedia100k_text.jsonl` / `/tmp/dbpedia100k_queries.jsonl`
   sidecars aligned by row index. Corpus sanity: all 100k titles unique
   (known-item queries), median text 52 words, none empty. Parquet shards
   cached in `scratch/hf_dbpedia/` (gitignored, auto-redownloaded).
   Queries: (a) keyword-heavy known-item queries (exact entity titles / rare
   terms — where vector search goes blind), (b) semantic queries (existing
   ground truth). **Sprint success criterion: hybrid ≥ vector-only on semantic
   queries AND materially better on keyword queries.** Numeric bar set after
   the first baseline run, not invented up front.
3. **Performance** — `SEARCHTEXT` latency must come in well under vector
   search (postings are cheaper than HNSW); `HYBRID` ≈ vector + ~10–20%.
   Russian-language smoke test for tokenizer+stemmer correctness.

## Work plan (in order)

1. Bench harness + correctness oracle (data prep for dbpedia texts included).
2. Tokenizer (+ stemmer integration).
3. `segmentText`: build at flush, query with global IDF, tenant-range scoping.
4. Delta inverted map + visibility (tombstones/shadowing).
5. Durability: WAL op, snapshot bump, replay tests.
6. Merge path: postings rebuild by decode; DF semantics test.
7. RRF fusion + `ADDDOC`/`SEARCHTEXT`/`HYBRID` commands + docs/COMMANDS.md.
8. Profit + perf benches; record canon numbers in BENCHMARKS.md.

## Non-goals (v1)

- Phrase queries / positional postings (BM25 needs tf only).
- Kazakh morphology.
- Text storage inside vector segments.
- Vector-less documents (door reserved, not implemented).
- LLM-side features (fact extraction, summarization) — upper layers.
