#!/usr/bin/env python3
"""ann-benchmarks HDF5 → raw bin для тяжёлых бенчей (loadSIFTRaw в vector/*_test.go).

Формат: <II nTrain dim> train(f32) <I nTest> test(f32). Ground truth бенчи
считают сами brute-force'ом (кроме dbpedia — для него см. convert_dbpedia.py,
у него в файле есть angular GT).

Датасеты: http://ann-benchmarks.com/<name>.hdf5, например
  sift-128-euclidean.hdf5, gist-960-euclidean.hdf5, mnist-784-euclidean.hdf5

Примеры (пути, которые ожидают тесты):
  ./scripts/convert_annbench.py sift-128-euclidean.hdf5 /tmp/sift200k.bin --train 200000 --test 500
  ./scripts/convert_annbench.py sift-128-euclidean.hdf5 /tmp/sift1m.bin   --test 1000
  ./scripts/convert_annbench.py gist-960-euclidean.hdf5 /tmp/gist_sub.bin --train 500000 --test 500
  ./scripts/convert_annbench.py mnist-784-euclidean.hdf5 /tmp/mnist784.bin
"""
import argparse
import struct

import h5py
import numpy as np

p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
p.add_argument("src", help="входной HDF5 (ann-benchmarks)")
p.add_argument("dst", help="выходной raw bin")
p.add_argument("--train", type=int, default=0, help="сколько train-векторов взять (0 = все)")
p.add_argument("--test", type=int, default=0, help="сколько query-векторов взять (0 = все)")
args = p.parse_args()

with h5py.File(args.src, "r") as f:
    train = f["train"][: args.train or None].astype(np.float32)
    test = f["test"][: args.test or None].astype(np.float32)

with open(args.dst, "wb") as out:
    out.write(struct.pack("<II", train.shape[0], train.shape[1]))
    out.write(train.tobytes())
    out.write(struct.pack("<I", test.shape[0]))
    out.write(test.tobytes())

print(f"{args.dst}: train={train.shape} test={test.shape}")
