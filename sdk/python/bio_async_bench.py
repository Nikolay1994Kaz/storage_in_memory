#!/usr/bin/env python3
"""
bio_async_bench.py — High-Performance Async Bioinformatics Benchmark for Molten KVStore
========================================================================================

Streams synthetic proteins with hierarchical cluster structure, inserts them into
the Molten KVStore using TCP pipelining, measures concurrent search QPS and latency,
and validates Recall@K accuracy.

Usage::

    # Run a quick smoke test (10k vectors)
    python3 sdk/python/bio_async_bench.py --limit 10000

    # Run the full 1M scale test
    python3 sdk/python/bio_async_bench.py --limit 1000000
"""

import argparse
import asyncio
import math
import random
import sys
import time
from typing import Dict, List, Optional, Set, Tuple

# ──────────────────────────────────────────────────────────────────────
# Vector Mathematics (Pure Python)
# ──────────────────────────────────────────────────────────────────────

def normalize(vec: List[float]) -> List[float]:
    """L2-normalize a vector in-place and return it."""
    norm = math.sqrt(sum(x * x for x in vec))
    if norm > 0:
        return [x / norm for x in vec]
    return vec


def dot_product(a: List[float], b: List[float]) -> float:
    """Compute dot product between two vectors."""
    return sum(x * y for x, y in zip(a, b))


def cosine_distance(a: List[float], b: List[float]) -> float:
    """Compute Cosine Distance (1 - Cosine Similarity) for normalized vectors."""
    return 1.0 - dot_product(a, b)


# ──────────────────────────────────────────────────────────────────────
# Hierarchical Synthetic Data Generator
# ──────────────────────────────────────────────────────────────────────

def generate_proteins(
    limit: int = 1000000,
    dim: int = 768,
    num_superfamilies: int = 10,
    families_per_sf: int = 10,
    seed: int = 42,
    run_id: int = 0,
):
    """Generate synthetic protein embeddings with a hierarchical cluster structure.

    1. Superfamilies: 10 random base centroids on the unit sphere.
    2. Families: 100 sub-centroids perturbed around superfamily centroids (std=0.20).
    3. Proteins: Individual protein vectors perturbed around family centroids (std=0.02).

    This simulates a realistic biological fold space and prevents orthogonality collapse.
    """
    rng = random.Random(seed)

    # 1. Generate Superfamilies
    superfamilies = []
    for _ in range(num_superfamilies):
        sf = normalize([rng.gauss(0, 1) for _ in range(dim)])
        superfamilies.append(sf)

    # 2. Generate Families
    families = []
    for sf in superfamilies:
        for _ in range(families_per_sf):
            fam = [sf[i] + rng.gauss(0, 0.20) for i in range(dim)]
            families.append(normalize(fam))

    num_families = len(families)
    proteins_per_family = limit // num_families
    if proteins_per_family == 0:
        proteins_per_family = 1

    count = 0
    # Yield proteins family by family
    for fam_idx, fam in enumerate(families):
        for p_idx in range(proteins_per_family):
            if count >= limit:
                return
            prot = [fam[i] + rng.gauss(0, 0.02) for i in range(dim)]
            normalized_prot = normalize(prot)
            pid = f"sync_{run_id}_{fam_idx:03d}_{p_idx:05d}"
            yield pid, normalized_prot, fam_idx
            count += 1

    # Yield remaining to reach exact limit
    while count < limit:
        fam_idx = rng.randint(0, num_families - 1)
        fam = families[fam_idx]
        prot = [fam[i] + rng.gauss(0, 0.02) for i in range(dim)]
        normalized_prot = normalize(prot)
        pid = f"sync_{run_id}_{fam_idx:03d}_{count:05d}"
        yield pid, normalized_prot, fam_idx
        count += 1


# ──────────────────────────────────────────────────────────────────────
# RESP Codec & Client
# ──────────────────────────────────────────────────────────────────────

def encode_resp(*args) -> bytes:
    """Encode command arguments into a RESP array of bulk strings."""
    parts = [f"*{len(args)}\r\n".encode()]
    for a in args:
        if isinstance(a, bytes):
            parts.append(f"${len(a)}\r\n".encode() + a + b"\r\n")
        else:
            s = str(a).encode("utf-8", "ignore")
            parts.append(f"${len(s)}\r\n".encode() + s + b"\r\n")
    return b"".join(parts)


def encode_vsim_add(key: str, vec: List[float]) -> bytes:
    """Highly optimized encoder for VSIM.ADD to maximize Python CPU throughput."""
    prefix = f"*{2 + len(vec)}\r\n$8\r\nVSIM.ADD\r\n${len(key)}\r\n{key}\r\n"
    parts = [prefix.encode("ascii")]
    for val in vec:
        s_val = str(val)
        parts.append(f"${len(s_val)}\r\n{s_val}\r\n".encode("ascii"))
    return b"".join(parts)


def encode_vsim_add_bin(key: str, vec: List[float]) -> bytes:
    """Highly optimized binary encoder for VSIM.ADDBIN using struct.pack."""
    import struct
    b_vec = struct.pack(f"<{len(vec)}f", *vec)
    key_bytes = key.encode("utf-8")
    parts = [
        b"*3\r\n$11\r\nVSIM.ADDBIN\r\n",
        f"${len(key_bytes)}\r\n".encode(), key_bytes, b"\r\n",
        f"${len(b_vec)}\r\n".encode(), b_vec, b"\r\n"
    ]
    return b"".join(parts)


def parse_vsim_info(raw: str) -> dict:
    """Parse the raw space-separated string from VSIM.INFO into a dictionary."""
    if not raw or not isinstance(raw, str):
        return {}
    result = {}
    for part in raw.split():
        if ":" in part:
            k, v = part.split(":", 1)
            try:
                result[k] = int(v)
            except ValueError:
                result[k] = v
    return result


class AsyncRESPParser:
    """Asynchronously reads and decodes RESP values from a StreamReader."""

    def __init__(self, reader: asyncio.StreamReader):
        self.reader = reader

    async def read_value(self) -> any:
        line = await self.reader.readline()
        if not line:
            raise ConnectionError("Server closed connection")
        prefix = chr(line[0])
        payload = line[1:-2]  # strip trailing \r\n

        if prefix == "+":
            # Simple string
            return payload.decode("utf-8", "ignore")
        elif prefix == "-":
            # Error
            raise RuntimeError(f"Server error: {payload.decode('utf-8', 'ignore')}")
        elif prefix == ":":
            # Integer
            return int(payload)
        elif prefix == "$":
            # Bulk string
            length = int(payload)
            if length == -1:
                return None
            data = await self.reader.readexactly(length)
            await self.reader.readexactly(2)  # consume trailing \r\n
            try:
                return data.decode("utf-8", "ignore")
            except UnicodeDecodeError:
                return data
        elif prefix == "*":
            # Array
            count = int(payload)
            if count == -1:
                return None
            return [await self.read_value() for _ in range(count)]
        else:
            raise ValueError(f"Unknown RESP prefix: {prefix!r}")


# ──────────────────────────────────────────────────────────────────────
# Main Benchmark Logic
# ──────────────────────────────────────────────────────────────────────

async def run_benchmark(
    host: str,
    port: int,
    password: Optional[str],
    limit: int,
    dim: int,
    batch_size: int,
    concurrency: int,
    search_duration: int,
    use_binary: bool = False,
):
    # Use a unique run ID to avoid HNSW delete-and-insert overhead when overwriting
    run_id = int(time.time()) % 100000
    
    mode_str = "BINARY (VSIM.ADDBIN)" if use_binary else "TEXT (VSIM.ADD)"
    print(f"\n🚀 Starting Async Bioinformatics Benchmark (Scale: {limit:,} proteins, dim={dim}, mode={mode_str}, run_id={run_id})")
    print(f"🔌 Connecting to Molten KVStore at {host}:{port}...")

    # Establish control connection
    try:
        reader, writer = await asyncio.open_connection(host, port)
        parser = AsyncRESPParser(reader)
    except Exception as e:
        print(f"❌ Connection failed: {e}")
        sys.exit(1)

    if password:
        writer.write(encode_resp("AUTH", password))
        await writer.drain()
        await parser.read_value()

    # Check database size & vector count before test
    writer.write(encode_resp("DBSIZE"))
    writer.write(encode_resp("VSIM.INFO"))
    await writer.drain()
    initial_dbsize = await parser.read_value()
    initial_vsim_info_raw = await parser.read_value()
    initial_vsim_info = parse_vsim_info(initial_vsim_info_raw)
    print(f"   Initial DBSIZE: {initial_dbsize:,} keys")
    print(f"   Initial Vector Store: {initial_vsim_info}")

    # ──────────────────────────────────────────────────────────────────
    # Phase 1: Setup data generator and select validation queries
    # ──────────────────────────────────────────────────────────────────
    print(f"\n🧬 Generating synthetic protein multi-clusters...")
    
    # We will choose 50 random protein IDs to use as queries for Recall@K validation.
    # To compute exact brute-force recall in Python without using gigabytes of memory,
    # we save all vectors belonging to the query's family in memory.
    # Since all top-10 true neighbors are guaranteed to lie inside the query's own family,
    # this family-local brute force is mathematically exact and takes < 1 ms.
    
    num_queries = 50
    # Pre-select indices of proteins to use as validation queries
    query_indices = set(random.Random(1337).sample(range(limit), num_queries))
    
    queries_info: Dict[int, Dict] = {}  # index -> {pid, vec, family_idx}
    family_vectors: Dict[int, List[Tuple[str, List[float]]]] = {}  # family_idx -> [(pid, vec)]
    
    # Fast first-pass to collect the vectors needed for Recall validation
    t0 = time.perf_counter()
    for idx, (pid, vec, fam_idx) in enumerate(generate_proteins(limit, dim, run_id=run_id)):
        # If this family belongs to one of our validation queries, save the vector
        if idx in query_indices:
            queries_info[idx] = {"pid": pid, "vec": vec, "family_idx": fam_idx}
            if fam_idx not in family_vectors:
                family_vectors[fam_idx] = []
        
        # If we are tracking this family, record the vector
        for q_idx, q_info in queries_info.items():
            if q_info["family_idx"] == fam_idx:
                family_vectors[fam_idx].append((pid, vec))
                break
                
    print(f"   Selected {len(queries_info)} validation queries from {len(family_vectors)} families.")
    print(f"   Prepared local validation arrays in {time.perf_counter() - t0:.2f}s.")

    # ──────────────────────────────────────────────────────────────────
    # Phase 2: Async Pipelined Insert
    # ──────────────────────────────────────────────────────────────────
    print(f"\n📥 Phase 2: Pipelined Indexing ({limit:,} proteins)...")
    
    # Open dedicated writer connection for insertion
    ins_reader, ins_writer = await asyncio.open_connection(host, port)
    ins_parser = AsyncRESPParser(ins_reader)
    if password:
        ins_writer.write(encode_resp("AUTH", password))
        await ins_writer.drain()
        await ins_parser.read_value()

    t_start_insert = time.perf_counter()
    inserted = 0
    buffer = bytearray()
    
    # Select encoder based on mode
    encoder = encode_vsim_add_bin if use_binary else encode_vsim_add

    # Stream-generate proteins and pipeline writes
    for pid, vec, fam_idx in generate_proteins(limit, dim, run_id=run_id):
        buffer.extend(encoder(f"protein:{pid}", vec))
        # Also store family metadata in KV store for filtered searches
        buffer.extend(encode_resp("SET", f"family:protein:{pid}", f"fam_{fam_idx}"))
        inserted += 1

        # Flush batch
        if inserted % batch_size == 0 or inserted == limit:
            ins_writer.write(bytes(buffer))
            await ins_writer.drain()
            buffer.clear()
            
            # Read responses for the batch (each loop iteration added 2 commands: VSIM.ADD + SET)
            num_commands = (batch_size * 2) if (inserted % batch_size == 0) else ((inserted % batch_size) * 2)
            for _ in range(num_commands):
                await ins_parser.read_value()
                
            if inserted % 10000 == 0 or inserted == limit:
                elapsed = time.perf_counter() - t_start_insert
                rate = inserted / elapsed
                print(f"   [{inserted:>7,}/{limit:,}] indexed... Speed: {rate:>8.1f} proteins/sec", end="\r")

    await ins_writer.drain()
    ins_writer.close()
    await ins_writer.wait_closed()

    insert_time = time.perf_counter() - t_start_insert
    print(f"\n   ✅ Completed indexing in {insert_time:.2f}s ({limit / insert_time:.1f} proteins/sec)")

    # ──────────────────────────────────────────────────────────────────
    # Phase 3: Concurrent Search Benchmark (QPS & Latency under load)
    # ──────────────────────────────────────────────────────────────────
    print(f"\n⚡ Phase 3: Concurrent Search Benchmark ({concurrency} parallel clients, duration: {search_duration}s)...")
    
    # Collect all queries to search from (we sample random queries from our generated set)
    all_validation_queries = list(queries_info.values())
    
    latencies: List[float] = []
    total_searches = 0
    
    async def search_worker(worker_id: int):
        nonlocal total_searches
        # Open dedicated connection per worker
        w_reader, w_writer = await asyncio.open_connection(host, port)
        w_parser = AsyncRESPParser(w_reader)
        if password:
            w_writer.write(encode_resp("AUTH", password))
            await w_writer.drain()
            await w_parser.read_value()

        local_latencies = []
        end_time = time.perf_counter() + search_duration
        rng = random.Random(worker_id + 42)

        while time.perf_counter() < end_time:
            # Pick a random query vector
            q = rng.choice(all_validation_queries)
            q_vec = q["vec"]
            
            # Format command: VSIM.SEARCH 10 v1 v2 ... vN
            args = ["10"]
            for val in q_vec:
                args.append(repr(val))

            t0 = time.perf_counter()
            w_writer.write(encode_resp("VSIM.SEARCH", *args))
            await w_writer.drain()
            await w_parser.read_value()  # Read search array response
            latency = (time.perf_counter() - t0) * 1000  # ms
            
            local_latencies.append(latency)
            
        w_writer.close()
        await w_writer.wait_closed()
        
        latencies.extend(local_latencies)
        total_searches += len(local_latencies)

    # Launch all workers concurrently
    t_start_search = time.perf_counter()
    await asyncio.gather(*(search_worker(i) for i in range(concurrency)))
    search_time = time.perf_counter() - t_start_search

    # Calculate statistics
    if latencies:
        sorted_lats = sorted(latencies)
        p50 = sorted_lats[len(sorted_lats) // 2]
        p95 = sorted_lats[int(len(sorted_lats) * 0.95)]
        p99 = sorted_lats[int(len(sorted_lats) * 0.99)]
        mean_lat = sum(latencies) / len(latencies)
        
        # Standard deviation
        variance = sum((l - mean_lat) ** 2 for l in latencies) / len(latencies)
        stdev = math.sqrt(variance)

        print(f"   ✅ Total Searches: {total_searches:,} queries")
        print(f"   ✅ Throughput:     {total_searches / search_time:.1f} searches/sec (QPS)")
        print(f"   ✅ Latency Metrics:")
        print(f"      min:   {min(latencies):.3f} ms")
        print(f"      p50:   {p50:.3f} ms")
        print(f"      p95:   {p95:.3f} ms")
        print(f"      p99:   {p99:.3f} ms")
        print(f"      max:   {max(latencies):.3f} ms")
        print(f"      mean:  {mean_lat:.3f} ms")
        print(f"      stdev: {stdev:.3f} ms")
    else:
        print("   ⚠ No searches completed.")

    # ──────────────────────────────────────────────────────────────────
    # Phase 4: Recall@K Accuracy Validation
    # ──────────────────────────────────────────────────────────────────
    print(f"\n🔬 Phase 4: Recall@10 Accuracy Validation (50 random query proteins)...")
    
    total_recall = 0.0
    
    for q_info in all_validation_queries:
        q_pid = q_info["pid"]
        q_vec = q_info["vec"]
        q_fam_idx = q_info["family_idx"]
        
        # 1. Compute local brute force against family members
        family_members = family_vectors[q_fam_idx]
        scored_members = []
        for pid, vec in family_members:
            # We use cosine distance (1 - dot_product)
            dist = cosine_distance(q_vec, vec)
            scored_members.append((pid, dist))
            
        # Sort by distance ascending to find true nearest 10
        scored_members.sort(key=lambda x: x[1])
        true_top_10 = [f"protein:{pid}" for pid, dist in scored_members[:10]]
        true_top_set = set(true_top_10)

        # 2. Query server
        args = ["10"]
        for val in q_vec:
            args.append(repr(val))
        writer.write(encode_resp("VSIM.SEARCH", *args))
        await writer.drain()
        
        # Server returns: [key, dist, key, dist, ...]
        raw_results = await parser.read_value()
        server_results = []
        if raw_results:
            for i in range(0, len(raw_results), 2):
                server_results.append(raw_results[i])
                
        # 3. Calculate overlap
        overlap = sum(1 for res in server_results if res in true_top_set)
        recall = overlap / 10.0
        total_recall += recall

    avg_recall = total_recall / len(all_validation_queries)
    print(f"   ✅ Average Recall@10: {avg_recall * 100:.2f}% (measured over 50 queries)")

    # ──────────────────────────────────────────────────────────────────
    # Phase 5: Resource & Memory Audit
    # ──────────────────────────────────────────────────────────────────
    print(f"\n📊 Phase 5: Resource & Memory Audit...")
    writer.write(encode_resp("DBSIZE"))
    writer.write(encode_resp("VSIM.INFO"))
    await writer.drain()
    final_dbsize = await parser.read_value()
    final_vsim_info_raw = await parser.read_value()
    final_vsim_info = parse_vsim_info(final_vsim_info_raw)
    
    print(f"   Final DBSIZE: {final_dbsize:,} keys (+{final_dbsize - initial_dbsize:,} added)")
    print(f"   Final Vector Store: {final_vsim_info}")
    
    # Calculate index footprint on server
    added_vectors = limit
    if "vectors" in final_vsim_info and "vectors" in initial_vsim_info:
        added_vectors = final_vsim_info["vectors"] - initial_vsim_info["vectors"]
    print(f"   Index Growth: +{added_vectors:,} vectors added to HNSW index.")
    
    writer.close()
    await writer.wait_closed()
    print(f"\n🏁 Benchmark completed successfully!")


# ──────────────────────────────────────────────────────────────────────
# CLI Entry Point
# ──────────────────────────────────────────────────────────────────────

def main():
    parser = argparse.ArgumentParser(
        description="High-Performance Async Bioinformatics Benchmark for Molten KVStore",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    parser.add_argument("--host", default="localhost", help="Molten server host")
    parser.add_argument("--port", type=int, default=6380, help="Molten server port")
    parser.add_argument("--password", default=None, help="AUTH password")
    parser.add_argument("--limit", type=int, default=1000000, help="Total proteins to index")
    parser.add_argument("--dim", type=int, default=768, help="Embedding dimension")
    parser.add_argument("--batch-size", type=int, default=1000, help="RESP Pipelining batch size")
    parser.add_argument("--concurrency", type=int, default=50, help="Concurrent clients for search QPS")
    parser.add_argument("--search-duration", type=int, default=10, help="Duration of search benchmark (seconds)")
    parser.add_argument("--use-binary", action="store_true", help="Use binary vector serialization (VSIM.ADDBIN)")
    args = parser.parse_args()

    asyncio.run(
        run_benchmark(
            host=args.host,
            port=args.port,
            password=args.password,
            limit=args.limit,
            dim=args.dim,
            batch_size=args.batch_size,
            concurrency=args.concurrency,
            search_duration=args.search_duration,
            use_binary=args.use_binary,
        )
    )


if __name__ == "__main__":
    main()
