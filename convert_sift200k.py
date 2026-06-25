#!/usr/bin/env python3
"""SIFT-128 -> raw binary для arch-review бенчей. 200k train + 500 queries."""
import h5py, numpy as np, struct

src = "/home/nikolay/storage_in_memory/scratch/server_data/data/sift-128-euclidean.hdf5"
dst = "/tmp/sift200k.bin"
N_TRAIN, N_TEST = 200_000, 500

with h5py.File(src, "r") as f:
    train = f["train"][:N_TRAIN].astype(np.float32)
    test = f["test"][:N_TEST].astype(np.float32)

with open(dst, "wb") as out:
    out.write(struct.pack("<II", len(train), train.shape[1]))
    out.write(train.tobytes())
    out.write(struct.pack("<I", len(test)))
    out.write(test.tobytes())

print(f"Written {dst}: {len(train)} train + {len(test)} test, dim={train.shape[1]}")
