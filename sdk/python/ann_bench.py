#!/usr/bin/env python3
import argparse
import os
import sys
import time
import h5py
import numpy as np
import multiprocessing

# Add current folder to path to import molten_client
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from molten_client import MoltenClient

def search_worker(host: str, port: int, queries, K: int, gt_sets, results_queue, barrier, use_binary: bool = False, use_pipeline: bool = False, batch_size: int = 50):
    client = MoltenClient(host, port, timeout=60.0)
    try:
        client.connect()
        # Wait for all workers and the main thread to be ready
        barrier.wait()
        
        latencies = []
        recalls = []
        
        if use_pipeline:
            for idx in range(0, len(queries), batch_size):
                batch_queries = queries[idx:idx+batch_size]
                batch_gt = gt_sets[idx:idx+batch_size]
                
                t_start = time.perf_counter()
                with client.pipeline() as pipe:
                    for query_vec in batch_queries:
                        if use_binary:
                            pipe.vsim_search_bin(query_vec, k=K)
                        else:
                            pipe.vsim_search(query_vec.tolist(), k=K)
                    results_list = pipe.execute()
                duration = time.perf_counter() - t_start
                
                lat_per_query = duration / len(batch_queries)
                for results, gt_set in zip(results_list, batch_gt):
                    latencies.append(lat_per_query)
                    found_ids = []
                    for key, _ in results:
                        try:
                            found_ids.append(int(key.split(":")[1]))
                        except (IndexError, ValueError):
                            pass
                    overlap = len(gt_set.intersection(found_ids))
                    recalls.append(float(overlap) / float(K))
        else:
            for query_vec, gt_set in zip(queries, gt_sets):
                t_start = time.perf_counter()
                if use_binary:
                    results = client.vsim_search_bin(query_vec, k=K)
                else:
                    results = client.vsim_search(query_vec.tolist(), k=K)
                duration = time.perf_counter() - t_start
                latencies.append(duration)

                # Extract integer IDs from keys
                found_ids = []
                for key, _ in results:
                    try:
                        found_ids.append(int(key.split(":")[1]))
                    except (IndexError, ValueError):
                        pass

                overlap = len(gt_set.intersection(found_ids))
                recalls.append(float(overlap) / float(K))
                
        results_queue.put((latencies, recalls))
    except Exception as e:
        print(f"❌ Worker error: {e}")
        # Ensure we still satisfy the queue get() to avoid deadlock
        results_queue.put(([], []))
    finally:
        client.close()

def run_bench(hdf5_path: str, host: str, port: int, limit: int, K: int, num_workers: int, use_binary: bool = False, use_pipeline: bool = False, batch_size: int = 50, skip_insert: bool = False):
    if not os.path.exists(hdf5_path):
        print(f"❌ File {hdf5_path} not found! Please download it first.")
        return

    print(f"📖 Reading HDF5 file: {hdf5_path}...")
    with h5py.File(hdf5_path, "r") as f:
        train_data = f["train"][:]
        test_data = f["test"][:]

    if limit > 0 and limit < len(train_data):
        train_data = train_data[:limit]
        print(f"⚠️ Limiting training database to first {limit} vectors")

    n_vectors = len(train_data)
    dim = train_data.shape[1]
    print(f"✅ Loaded. Base size: {n_vectors} vectors, Dimension: {dim}")

    client = MoltenClient(host, port, timeout=60.0)
    try:
        client.connect()
        if skip_insert:
            print("⏭️ Skipping vector loading step (--skip-insert is set)")
        else:
            # 1. Insert vectors
            print(f"📥 Loading {n_vectors} vectors into HNSW index...")
            t0 = time.perf_counter()
            
            for i in range(n_vectors):
                key = f"v:{i}"
                if use_binary:
                    client.vsim_add_bin(key, train_data[i])
                else:
                    vec = train_data[i].tolist()
                    client.vsim_add(key, vec)
                
                if (i + 1) % 2000 == 0 or i == n_vectors - 1:
                    elapsed = time.perf_counter() - t0
                    rate = (i + 1) / elapsed
                    print(f"   [{i+1:>6}/{n_vectors}] {rate:>8.1f} inserts/sec", end="\r")
            
            build_time = time.perf_counter() - t0
            print(f"\n   ✅ Built index in {build_time:.2f}s ({n_vectors/build_time:.1f} vec/s)")

        # Print graph stats
        print(f"📊 Vector store info: {client.vsim_info()}")
    finally:
        client.close()

    # 2. Precompute Ground Truth
    queries_to_run = min(5000, len(test_data))
    print(f"\n🔍 Precomputing exact ground truth for {queries_to_run} queries on the fly using numpy...")
    
    is_cosine = "angular" in hdf5_path.lower() or "cosine" in hdf5_path.lower()
    if is_cosine:
        print("   Using Cosine distance for ground truth")
        dot = np.dot(test_data[:queries_to_run], train_data.T)
        norm_train = np.linalg.norm(train_data, axis=1)
        norm_query = np.linalg.norm(test_data[:queries_to_run], axis=1)[:, np.newaxis]
        dists = 1.0 - dot / (norm_train * norm_query + 1e-9)
    else:
        print("   Using Euclidean distance for ground truth")
        train_norms = np.sum(train_data ** 2, axis=1)
        query_norms = np.sum(test_data[:queries_to_run] ** 2, axis=1)[:, np.newaxis]
        dot = np.dot(test_data[:queries_to_run], train_data.T)
        dists = train_norms - 2 * dot + query_norms

    gt_sets = []
    for qi in range(queries_to_run):
        gt_indices = np.argpartition(dists[qi], K)[:K]
        gt_sets.append(set(gt_indices.tolist()))

    print("✅ Ground truth precomputed. Spawning parallel workers...")

    # 3. Parallel Search using Multiprocessing and Barriers
    chunks_queries = [[] for _ in range(num_workers)]
    chunks_gt = [[] for _ in range(num_workers)]
    for i in range(queries_to_run):
        w_idx = i % num_workers
        chunks_queries[w_idx].append(test_data[i])
        chunks_gt[w_idx].append(gt_sets[i])

    results_queue = multiprocessing.Queue()
    barrier = multiprocessing.Barrier(num_workers + 1)
    processes = []
    
    print(f"⚡ Spawning {num_workers} worker processes...")
    for w_idx in range(num_workers):
        p = multiprocessing.Process(
            target=search_worker,
            args=(host, port, chunks_queries[w_idx], K, chunks_gt[w_idx], results_queue, barrier, use_binary, use_pipeline, batch_size)
        )
        p.start()
        processes.append(p)

    print("⏳ Waiting for all worker processes to connect...")
    # Wait for all workers to connect and be ready
    barrier.wait()
    
    print(f"🔥 Starting parallel search stress test (Total queries: {queries_to_run}, K={K})...")
    t_start_all = time.perf_counter()

    all_latencies = []
    all_recalls = []
    for _ in range(num_workers):
        latencies, recalls = results_queue.get()
        all_latencies.extend(latencies)
        all_recalls.extend(recalls)

    for p in processes:
        p.join()

    total_time = time.perf_counter() - t_start_all
    
    if len(all_latencies) > 0:
        avg_recall = sum(all_recalls) / len(all_recalls)
        avg_latency_ms = (sum(all_latencies) / len(all_latencies)) * 1000
        qps = len(all_latencies) / total_time

        print(f"\n🏆 PARALLEL BENCHMARK RESULTS ({os.path.basename(hdf5_path)}):")
        print(f"   🎯 Average Recall@{K}: {avg_recall*100:.2f}%")
        print(f"   ⚡ Peak Throughput:    {qps:.1f} QPS (wall-clock)")
        print(f"   ⏱️  Average Latency:    {avg_latency_ms:.3f} ms")
        print(f"   ⏳ Total test time:    {total_time:.3f} s")
    else:
        print("❌ Error: No benchmark results collected.")

if __name__ == "__main__":
    # Fix for multiprocessing freeze in some environments
    multiprocessing.set_start_method("spawn", force=True)

    parser = argparse.ArgumentParser()
    parser.add_argument("file", help="Path to HDF5 file")
    parser.add_argument("--host", default="localhost")
    parser.add_argument("--port", type=int, default=6380)
    parser.add_argument("--limit", type=int, default=50000, help="Max database vectors (0 = no limit)")
    parser.add_argument("-k", type=int, default=10, help="Search K")
    parser.add_argument("--workers", type=int, default=12, help="Number of concurrent processes")
    parser.add_argument("--binary", action="store_true", help="Use binary serialization protocol (zero-copy)")
    parser.add_argument("--pipeline", action="store_true", help="Use Redis-style command pipelining")
    parser.add_argument("--batch-size", type=int, default=50, help="Pipeline batch size")
    parser.add_argument("--skip-insert", action="store_true", help="Skip loading vectors (runs only search)")
    args = parser.parse_args()

    run_bench(args.file, args.host, args.port, args.limit, args.k, args.workers, args.binary, args.pipeline, args.batch_size, args.skip_insert)
