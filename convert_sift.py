#!/usr/bin/env python3
"""Конвертирует SIFT HDF5 → raw binary для Go прототипа."""
import h5py
import numpy as np
import struct

path_hdf5 = "scratch/server_data/data/sift-128-euclidean.hdf5"
path_raw = "/tmp/sift_raw.bin"

with h5py.File(path_hdf5, "r") as f:
    train = f["train"][:50000].astype(np.float32)
    test = f["test"][:200].astype(np.float32)

with open(path_raw, "wb") as out:
    out.write(struct.pack("<II", len(train), train.shape[1]))
    out.write(train.tobytes())
    out.write(struct.pack("<I", len(test)))
    out.write(test.tobytes())

print(f"Written: {len(train)} train + {len(test)} test, dim={train.shape[1]}")
print(f"File size: {len(train)*train.shape[1]*4 + len(test)*train.shape[1]*4 + 12} bytes")
