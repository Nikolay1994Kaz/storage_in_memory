#!/usr/bin/env python3
"""
bio_loader.py — Bioinformatics Data Loader for Molten KVStore
==============================================================

Loads protein embeddings into the Molten vector store and demonstrates
similarity search, filtered search, and performance benchmarking.

Three data modes (tried in order):
1. **HuggingFace**: Downloads real ESM-2 embeddings from HuggingFace Hub.
2. **Local H5**: Reads a local HDF5 file with pre-computed embeddings.
3. **Synthetic**: Generates realistic synthetic protein-family embeddings.

Usage::

    # Synthetic data (no dependencies beyond stdlib)
    python bio_loader.py

    # Against a running server
    python bio_loader.py --host localhost --port 6380

    # With real data from HuggingFace
    pip install datasets h5py numpy
    python bio_loader.py --source huggingface --limit 5000
"""

from __future__ import annotations

import argparse
import math
import os
import random
import statistics
import struct
import sys
import time
from typing import Dict, List, Optional, Sequence, Tuple

# Add parent directory so we can import the SDK
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from molten_client import MoltenClient, RESPError


# ──────────────────────────────────────────────────────────────────────
# Protein family metadata (for synthetic generation & metadata filters)
# ──────────────────────────────────────────────────────────────────────

PROTEIN_FAMILIES = {
    "kinase": {
        "description": "Protein kinases — phosphorylation enzymes",
        "organism": "Homo sapiens",
        "examples": ["P04637", "P00533", "P06213", "P12931", "Q07817"],
    },
    "globin": {
        "description": "Globins — oxygen transport proteins",
        "organism": "Homo sapiens",
        "examples": ["P69905", "P68871", "P02042", "P02100", "P09105"],
    },
    "immunoglobulin": {
        "description": "Immunoglobulins — antibody proteins",
        "organism": "Homo sapiens",
        "examples": ["P01857", "P01859", "P01861", "P01871", "P01876"],
    },
    "cytochrome": {
        "description": "Cytochromes — electron transport",
        "organism": "Saccharomyces cerevisiae",
        "examples": ["P00044", "P00045", "P07256", "P00048", "P38169"],
    },
    "histone": {
        "description": "Histones — chromatin structure",
        "organism": "Homo sapiens",
        "examples": ["P62805", "P04908", "P84243", "P62807", "Q71DI3"],
    },
    "hsp": {
        "description": "Heat shock proteins — chaperones",
        "organism": "Escherichia coli",
        "examples": ["P0A6Y8", "P04475", "P0A6Z1", "P0A850", "P77319"],
    },
    "protease": {
        "description": "Proteases — protein degradation enzymes",
        "organism": "Homo sapiens",
        "examples": ["P07339", "P00760", "P00766", "P00784", "P00790"],
    },
    "dna_repair": {
        "description": "DNA repair proteins",
        "organism": "Homo sapiens",
        "examples": ["P04637_DR", "O15350", "Q92878", "P38398", "Q12888"],
    },
    "receptor": {
        "description": "Cell surface receptors",
        "organism": "Homo sapiens",
        "examples": ["P04626", "P00533_R", "P08581", "P10721", "P07949"],
    },
    "transporter": {
        "description": "Membrane transporters",
        "organism": "Homo sapiens",
        "examples": ["P11166", "P02786", "P08195", "P21397", "Q01959"],
    },
}

# P53-like query proteins (tumor suppressor family)
P53_FAMILY_IDS = [
    "P04637",   # TP53 — Human tumor protein p53
    "Q00987",   # MDM2 — p53 E3 ubiquitin ligase
    "Q9NZJ5",   # TP63 — Tumor protein p63
    "Q9H3D4",   # TP73 — Tumor protein p73
    "O15350",   # TP53BP1 — p53-binding protein 1
]


# ──────────────────────────────────────────────────────────────────────
# Vector math utilities (pure Python — no numpy required)
# ──────────────────────────────────────────────────────────────────────

def normalize(vec: List[float]) -> List[float]:
    """L2-normalize a vector in-place and return it."""
    norm = math.sqrt(sum(x * x for x in vec))
    if norm > 0:
        return [x / norm for x in vec]
    return vec


def cosine_similarity(a: Sequence[float], b: Sequence[float]) -> float:
    """Compute cosine similarity between two vectors."""
    dot = sum(x * y for x, y in zip(a, b))
    norm_a = math.sqrt(sum(x * x for x in a))
    norm_b = math.sqrt(sum(x * x for x in b))
    if norm_a == 0 or norm_b == 0:
        return 0.0
    return dot / (norm_a * norm_b)


def euclidean_distance(a: Sequence[float], b: Sequence[float]) -> float:
    """Compute Euclidean distance between two vectors."""
    return math.sqrt(sum((x - y) ** 2 for x, y in zip(a, b)))


# ──────────────────────────────────────────────────────────────────────
# Data sources
# ──────────────────────────────────────────────────────────────────────

def generate_synthetic_proteins(
    n_per_family: int = 100,
    dim: int = 640,
    noise_std: float = 0.03,
    seed: int = 42,
) -> List[Tuple[str, List[float], str]]:
    """Generate synthetic protein embeddings grouped by family.

    Each family has a random centroid, and individual proteins are
    generated by adding Gaussian noise around the centroid.  This
    mimics how real ESM-2 embeddings cluster by structural similarity.

    Returns
    -------
    list of (protein_id, embedding, family_name) tuples
    """
    rng = random.Random(seed)
    proteins: List[Tuple[str, List[float], str]] = []

    for family_name, meta in PROTEIN_FAMILIES.items():
        # Generate a random centroid for this family
        centroid = normalize([rng.gauss(0, 1) for _ in range(dim)])

        # Use real UniProt IDs if available, then generate more
        example_ids = list(meta["examples"])
        for i in range(n_per_family):
            if i < len(example_ids):
                protein_id = example_ids[i]
            else:
                protein_id = f"{family_name}:{i:04d}"

            # Perturb centroid with noise
            vec = [
                c + rng.gauss(0, noise_std) for c in centroid
            ]
            vec = normalize(vec)
            proteins.append((protein_id, vec, family_name))

    # Shuffle to simulate realistic insertion order
    rng.shuffle(proteins)
    return proteins


def try_load_huggingface(limit: int = 5000, dim: int = 640) -> Optional[List[Tuple[str, List[float], str]]]:
    """Attempt to load ESM-2 embeddings from HuggingFace datasets.

    Returns None if the required packages are not installed.
    """
    try:
        from datasets import load_dataset
        import numpy as np
    except ImportError:
        return None

    print("📦 Loading ESM-2 embeddings from HuggingFace Hub…")
    try:
        ds = load_dataset(
            "facebook/esm2_embeddings",
            split=f"train[:{limit}]",
            streaming=True,
        )
        proteins = []
        for i, row in enumerate(ds):
            if i >= limit:
                break
            pid = row.get("id", f"hf:{i}")
            emb = row.get("embedding", row.get("mean_representations", None))
            if emb is None:
                continue
            if hasattr(emb, "tolist"):
                emb = emb.tolist()
            if len(emb) != dim:
                emb = emb[:dim] if len(emb) > dim else emb + [0.0] * (dim - len(emb))
            family = row.get("family", "unknown")
            proteins.append((pid, normalize(emb), family))

        if proteins:
            print(f"   Loaded {len(proteins)} proteins from HuggingFace")
            return proteins
    except Exception as e:
        print(f"   ⚠ HuggingFace load failed: {e}")

    return None


def try_load_h5(path: str, limit: int = 5000, dim: int = 640) -> Optional[List[Tuple[str, List[float], str]]]:
    """Attempt to load embeddings from a local HDF5 file."""
    if not os.path.exists(path):
        return None
    try:
        import h5py
        import numpy as np
    except ImportError:
        return None

    print(f"📦 Loading embeddings from {path}…")
    try:
        with h5py.File(path, "r") as f:
            proteins = []
            keys = list(f.keys())[:limit]
            for key in keys:
                emb = f[key][:]
                if len(emb) != dim:
                    emb = emb[:dim] if len(emb) > dim else list(emb) + [0.0] * (dim - len(emb))
                proteins.append((key, normalize(list(map(float, emb))), "unknown"))
            if proteins:
                print(f"   Loaded {len(proteins)} proteins from H5")
                return proteins
    except Exception as e:
        print(f"   ⚠ H5 load failed: {e}")

    return None


# ──────────────────────────────────────────────────────────────────────
# Main loader / benchmarker
# ──────────────────────────────────────────────────────────────────────

def run(
    host: str = "localhost",
    port: int = 6380,
    source: str = "auto",
    limit: int = 1000,
    dim: int = 640,
    search_k: int = 10,
    n_per_family: int = 100,
    password: Optional[str] = None,
) -> None:
    """Main entry point: load data, insert into Molten, run searches."""

    # ── 1. Load / generate protein data ─────────────────────────────
    proteins: Optional[List[Tuple[str, List[float], str]]] = None

    if source in ("auto", "huggingface"):
        proteins = try_load_huggingface(limit, dim)

    if proteins is None and source in ("auto", "h5"):
        proteins = try_load_h5("embeddings.h5", limit, dim)

    if proteins is None:
        print(f"🧬 Generating {n_per_family * len(PROTEIN_FAMILIES)} synthetic proteins "
              f"({len(PROTEIN_FAMILIES)} families × {n_per_family}, dim={dim})…")
        proteins = generate_synthetic_proteins(n_per_family, dim, seed=42)

    print(f"   Total proteins: {len(proteins)}")

    # Count families
    families: Dict[str, int] = {}
    for _, _, fam in proteins:
        families[fam] = families.get(fam, 0) + 1
    print(f"   Families: {dict(sorted(families.items(), key=lambda x: -x[1]))}\n")

    # ── 2. Connect to Molten ────────────────────────────────────────
    print(f"🔌 Connecting to Molten at {host}:{port}…")
    client = MoltenClient(host, port, password=password, timeout=10.0)
    try:
        client.connect()
        pong = client.ping()
        print(f"   PING → {pong}")
    except (ConnectionRefusedError, ConnectionError) as e:
        print(f"❌ Cannot connect to {host}:{port}: {e}")
        print("   Make sure the Molten KVStore server is running.")
        sys.exit(1)

    try:
        # ── 3. Insert vectors ───────────────────────────────────────
        print(f"\n📥 Inserting {len(proteins)} vectors into HNSW index…")
        t0 = time.perf_counter()
        inserted = 0
        errors = 0

        for i, (pid, vec, family) in enumerate(proteins):
            key = f"protein:{pid}"
            try:
                client.vsim_add(key, vec)

                # Store metadata for filtered search
                client.set(f"family:{key}", family)
                client.set(f"organism:{key}", PROTEIN_FAMILIES.get(family, {}).get("organism", "unknown"))
                client.set(f"desc:{key}", PROTEIN_FAMILIES.get(family, {}).get("description", ""))

                inserted += 1
            except RESPError as e:
                errors += 1
                if errors <= 3:
                    print(f"   ⚠ Error inserting {key}: {e}")

            if (i + 1) % 200 == 0 or i == len(proteins) - 1:
                elapsed = time.perf_counter() - t0
                rate = (i + 1) / elapsed
                print(f"   [{i+1:>6}/{len(proteins)}] {rate:>8.1f} inserts/sec", end="\r")

        insert_time = time.perf_counter() - t0
        print(f"\n   ✅ Inserted {inserted} vectors in {insert_time:.2f}s "
              f"({inserted / insert_time:.1f} vec/s, {errors} errors)")

        # ── 4. VSIM.INFO ────────────────────────────────────────────
        info = client.vsim_info()
        print(f"\n📊 Vector store info: {info}")

        # ── 5. P53 homolog search ───────────────────────────────────
        print(f"\n🔬 Searching for P53 homologs (k={search_k})…")
        # Use the first P53 family member as query
        query_key = None
        query_vec = None
        for pid, vec, fam in proteins:
            if pid == "P04637":  # TP53
                query_key = pid
                query_vec = vec
                break

        if query_vec is None:
            # Fallback: use any kinase (P04637 is in kinase family in our synthetic data)
            for pid, vec, fam in proteins:
                if fam == "kinase":
                    query_key = pid
                    query_vec = vec
                    break

        if query_vec is not None:
            print(f"   Query: {query_key}")

            t0 = time.perf_counter()
            results = client.vsim_search(query_vec, k=search_k)
            search_time = time.perf_counter() - t0

            print(f"   Search latency: {search_time*1000:.2f}ms")
            print(f"   {'Rank':<6} {'Key':<30} {'Distance':>12}")
            print(f"   {'─'*6} {'─'*30} {'─'*12}")
            for rank, (key, dist) in enumerate(results, 1):
                # Fetch family metadata
                family = client.get(f"family:{key}") or "?"
                print(f"   {rank:<6} {key:<30} {dist:>12.6f}  [{family}]")
        else:
            print("   ⚠ No P53-like query vector found")

        # ── 6. Filtered search examples ─────────────────────────────
        print(f"\n🔍 Filtered search examples:")

        if query_vec is not None:
            # 6a. Filter by family (metadata mode)
            print(f"\n   (a) KV metadata filter: family=globin (should be empty for kinase query)")
            try:
                t0 = time.perf_counter()
                filtered = client.vsim_search_filter(
                    query_vec, k=5, filter_field="family", filter_value="globin"
                )
                ft = time.perf_counter() - t0
                print(f"       Latency: {ft*1000:.2f}ms")
                for rank, (key, dist) in enumerate(filtered, 1):
                    fam = client.get(f"family:{key}") or "?"
                    print(f"       {rank}. {key:<30} dist={dist:.6f} [{fam}]")
                if not filtered:
                    print("       (no results — expected since globin and kinase clusters are well-separated)")
            except RESPError as e:
                print(f"       Error: {e}")

            print(f"\n   (a2) KV metadata filter: family=kinase (should return kinase proteins)")
            try:
                t0 = time.perf_counter()
                filtered = client.vsim_search_filter(
                    query_vec, k=5, filter_field="family", filter_value="kinase"
                )
                ft = time.perf_counter() - t0
                print(f"       Latency: {ft*1000:.2f}ms")
                for rank, (key, dist) in enumerate(filtered, 1):
                    fam = client.get(f"family:{key}") or "?"
                    print(f"       {rank}. {key:<30} dist={dist:.6f} [{fam}]")
                if not filtered:
                    print("       (no results)")
            except RESPError as e:
                print(f"       Error: {e}")

            # 6b. Prefix filter
            print(f"\n   (b) PREFIX filter: protein:kinase:*")
            try:
                t0 = time.perf_counter()
                prefix_results = client.vsim_search_prefix(
                    query_vec, k=5, prefix="protein:kinase:"
                )
                pt = time.perf_counter() - t0
                print(f"       Latency: {pt*1000:.2f}ms")
                for rank, (key, dist) in enumerate(prefix_results, 1):
                    print(f"       {rank}. {key:<30} dist={dist:.6f}")
                if not prefix_results:
                    print("       (no results with this prefix)")
            except RESPError as e:
                print(f"       Error: {e}")

        # ── 7. Benchmark ────────────────────────────────────────────
        print(f"\n⚡ Benchmark: {200} random searches…")
        latencies = []
        rng = random.Random(12345)

        for _ in range(200):
            # Pick a random protein as query
            _, qvec, _ = proteins[rng.randint(0, len(proteins) - 1)]
            t0 = time.perf_counter()
            client.vsim_search(qvec, k=search_k)
            latencies.append(time.perf_counter() - t0)

        latencies_ms = [l * 1000 for l in latencies]
        sorted_lat = sorted(latencies_ms)
        p50 = sorted_lat[len(sorted_lat) // 2]
        p95 = sorted_lat[int(len(sorted_lat) * 0.95)]
        p99 = sorted_lat[int(len(sorted_lat) * 0.99)]
        total = sum(latencies)

        print(f"   Total time:  {total:.3f}s")
        print(f"   Throughput:  {200 / total:.1f} searches/sec")
        print(f"   Latency:")
        print(f"     min:   {min(latencies_ms):.3f}ms")
        print(f"     p50:   {p50:.3f}ms")
        print(f"     p95:   {p95:.3f}ms")
        print(f"     p99:   {p99:.3f}ms")
        print(f"     max:   {max(latencies_ms):.3f}ms")
        print(f"     mean:  {statistics.mean(latencies_ms):.3f}ms")
        print(f"     stdev: {statistics.stdev(latencies_ms):.3f}ms")

        # ── 8. ZSet + vector range search example ───────────────────
        print(f"\n🏷️  ZSet + vector range search example:")
        print(f"   Adding protein lengths as ZSet scores…")

        rng2 = random.Random(99)
        sample = proteins[:min(100, len(proteins))]
        for pid, vec, fam in sample:
            key = f"protein:{pid}"
            # Simulate protein length as score (real proteins: 50–5000 AA)
            length = rng2.randint(100, 3000)
            client.zadd("protein:lengths", float(length), key)

        print(f"   ZCARD protein:lengths → {client.zcard('protein:lengths')}")

        if query_vec is not None:
            print(f"\n   VSIM.SEARCHRANGE: k=5, score ∈ [200, 800] (short proteins)")
            try:
                t0 = time.perf_counter()
                range_results = client.vsim_search_range(
                    query_vec, k=5,
                    zset_name="protein:lengths",
                    min_score=200.0,
                    max_score=800.0,
                )
                rt = time.perf_counter() - t0
                print(f"   Latency: {rt*1000:.2f}ms")
                for rank, (key, dist) in enumerate(range_results, 1):
                    score = client.zscore("protein:lengths", key)
                    print(f"   {rank}. {key:<30} dist={dist:.6f}  length={score}")
                if not range_results:
                    print("   (no vectors matched the score range)")
            except RESPError as e:
                print(f"   Error: {e}")

        # ── 9. Cleanup summary ──────────────────────────────────────
        print(f"\n{'='*60}")
        print(f"✅ Bio loader completed successfully!")
        print(f"   Vectors in store: {client.vsim_info()}")
        print(f"   KV keys (DBSIZE): {client.dbsize()}")
        print(f"{'='*60}")

    finally:
        client.close()


# ──────────────────────────────────────────────────────────────────────
# CLI
# ──────────────────────────────────────────────────────────────────────

def main() -> None:
    parser = argparse.ArgumentParser(
        description="Load protein embeddings into Molten KVStore",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    parser.add_argument("--host", default="localhost", help="Molten server host")
    parser.add_argument("--port", type=int, default=6380, help="Molten server port")
    parser.add_argument("--password", default=None, help="AUTH password")
    parser.add_argument(
        "--source", choices=["auto", "synthetic", "huggingface", "h5"],
        default="auto", help="Data source"
    )
    parser.add_argument("--limit", type=int, default=1000, help="Max proteins to load")
    parser.add_argument("--dim", type=int, default=640, help="Embedding dimension")
    parser.add_argument("--k", type=int, default=10, help="Search K")
    parser.add_argument(
        "--n-per-family", type=int, default=100,
        help="Proteins per family (synthetic mode)"
    )
    args = parser.parse_args()

    run(
        host=args.host,
        port=args.port,
        source=args.source,
        limit=args.limit,
        dim=args.dim,
        search_k=args.k,
        n_per_family=args.n_per_family,
        password=args.password,
    )


if __name__ == "__main__":
    main()
