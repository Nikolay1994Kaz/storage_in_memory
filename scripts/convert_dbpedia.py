#!/usr/bin/env python3
"""dbpedia-openai HDF5 (ann-benchmarks) → сырой bin с ground truth.

Зачем отдельно от convert_annbench.py. Формат почти тот же, но с ХВОСТОМ:
после тестовых векторов идёт ground truth (индексы истинных соседей). Читатель
loadDBpediaRaw в dbpedia_validation_test.go этот хвост ждёт всегда, а
convert_annbench.py его не пишет — файл от него читался бы за границей буфера.
Поэтому подменять один скрипт другим нельзя, и docs/BENCHMARKS.md полтора
месяца ссылался на `./convert_dbpedia.py`, которого в репозитории не было:
шесть тестов на dbpedia воспроизвести по инструкции было невозможно.

Формат (little-endian, как читает loadDBpediaRaw):

    <u32 nTrain><u32 dim> train(f32)
    <u32 nTest>           test(f32)
    <u32 K>               gt(int32)   ← K соседей на каждый тестовый вектор

Данные: https://storage.googleapis.com/ann-datasets/ann-benchmarks/dbpedia-openai-100k-angular.hdf5

Пример:
  ./scripts/convert_dbpedia.py dbpedia-openai-100k-angular.hdf5 /tmp/dbpedia100k.bin
"""
import argparse
import struct

import h5py
import numpy as np

p = argparse.ArgumentParser()
p.add_argument("src", help="исходный .hdf5 из ann-benchmarks")
p.add_argument("dst", help="куда писать .bin")
p.add_argument("--train", type=int, default=0, help="сколько train-векторов взять (0 = все)")
p.add_argument("--test", type=int, default=0, help="сколько test-векторов взять (0 = все)")
p.add_argument("--k", type=int, default=100, help="сколько соседей ground truth оставить")
args = p.parse_args()

with h5py.File(args.src, "r") as f:
    train = f["train"][: args.train or None].astype(np.float32)
    test = f["test"][: args.test or None].astype(np.float32)
    # neighbors — индексы в train; обрезаем до K и приводим к int32, как читает Go.
    gt = f["neighbors"][: args.test or None, : args.k].astype(np.int32)

if gt.shape[0] != test.shape[0]:
    raise SystemExit(f"ground truth на {gt.shape[0]} запросов против {test.shape[0]} тестовых векторов")

with open(args.dst, "wb") as out:
    out.write(struct.pack("<II", train.shape[0], train.shape[1]))
    out.write(train.tobytes())
    out.write(struct.pack("<I", test.shape[0]))
    out.write(test.tobytes())
    out.write(struct.pack("<I", gt.shape[1]))
    out.write(gt.tobytes())

print(f"{args.dst}: train={train.shape} test={test.shape} gt={gt.shape}")
