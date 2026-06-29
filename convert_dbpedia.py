#!/usr/bin/env python3
"""dbpedia-openai-100k-angular hdf5 -> raw bin (с ground-truth) для валидации движка.
Формат: <II nTrain dim> train(f32) <I nTest> test(f32) <I Kgt> neighbors(i32).
Вектора уже L2-нормализованы (angular) → cosine = 1 - dot."""
import h5py, numpy as np, struct, sys
src = sys.argv[1] if len(sys.argv) > 1 else \
    "/home/nikolay/storage_in_memory/scratch/server_data/data/dbpedia-openai-100k-angular.hdf5"
dst = sys.argv[2] if len(sys.argv) > 2 else "/tmp/dbpedia100k.bin"
with h5py.File(src, "r") as f:
    train = f["train"][:].astype(np.float32)
    test  = f["test"][:].astype(np.float32)
    nbr   = f["neighbors"][:].astype(np.int32)   # (nTest, 100)
with open(dst, "wb") as out:
    out.write(struct.pack("<II", train.shape[0], train.shape[1]))
    out.write(train.tobytes())
    out.write(struct.pack("<I", test.shape[0]))
    out.write(test.tobytes())
    out.write(struct.pack("<I", nbr.shape[1]))
    out.write(nbr.tobytes())
print(f"{dst}: train={train.shape} test={test.shape} gt={nbr.shape}")
