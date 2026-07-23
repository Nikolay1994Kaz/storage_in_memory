#!/usr/bin/env python3
"""П2-эксперимент (23.07): суд «RRF×decay на РЕАЛЬНЫХ эмбеддингах».

Готовит данные для kvstore/vector/vmem_dbpedia_life_test.go: первые N строк
HF-датасета KShivendu/dbpedia-entities-openai-1M (реальные title/text +
реальные ada-002 1536d эмбеддинги) из локального parquet-шарда
scratch/hf_dbpedia/data/train-00000-*.parquet (лежит с BM25-спринта 18.07).

Выход:
  /tmp/vmemlife.bin    — <II n dim> векторы f32 (little-endian), L2-нормализованы;
  /tmp/vmemlife.jsonl  — по строке на док, выровнено с bin: {"i","title","text"}.

Запуск: scratch/bm25_venv/bin/python scripts/prep_vmemlife.py  (или любой
python с pyarrow+numpy).
"""

import glob
import json
import os
import struct
import sys

import numpy as np
import pyarrow.parquet as pq

N = int(sys.argv[1]) if len(sys.argv) > 1 else 20000
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
shards = sorted(glob.glob(os.path.join(ROOT, "scratch/hf_dbpedia/data/train-*.parquet")))
if not shards:
    sys.exit("нет parquet-шардов в scratch/hf_dbpedia/data — скачай их (см. convert_dbpedia_hf.py)")

t = pq.read_table(shards[0], columns=["title", "text", "openai"])
if t.num_rows < N:
    sys.exit(f"в шарде {t.num_rows} строк < N={N}")
titles = t.column("title").to_pylist()[:N]
texts = t.column("text").to_pylist()[:N]
vecs = np.array(t.column("openai").to_pylist()[:N], dtype=np.float32)
# L2-нормализация (ada-002 почти нормализованы; выравниваем точно — cosine-путь)
vecs /= np.linalg.norm(vecs, axis=1, keepdims=True)

with open("/tmp/vmemlife.bin", "wb") as out:
    out.write(struct.pack("<II", vecs.shape[0], vecs.shape[1]))
    out.write(vecs.tobytes())
with open("/tmp/vmemlife.jsonl", "w") as out:
    for i in range(N):
        out.write(json.dumps({"i": i, "title": titles[i], "text": texts[i][:800]},
                             ensure_ascii=False) + "\n")
print(f"/tmp/vmemlife.bin: {vecs.shape}, /tmp/vmemlife.jsonl: {N} строк")
