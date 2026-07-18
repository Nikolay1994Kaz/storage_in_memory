#!/usr/bin/env python3
"""HF KShivendu/dbpedia-entities-openai-1M -> bin + тексты для BM25/hybrid-бенча.

Зачем отдельный конвертер: локальный ann-benchmarks HDF5 текстов не содержит,
а его выборка 100k НЕ совпадает построчно с HF-датасетом (проверено 18.07:
cos≈0.6-0.7) — старый GT к текстам непереносим. Поэтому датасет строится
самодостаточно из HF-строк:

  корпус  = первые 100k строк (title+text+embedding),
  запросы = следующие 1000 строк (held-out, как в ann-benchmarks),
  GT      = точный брутфорс top-100 по cosine (вектора нормализуются).

Выход:
  /tmp/dbpedia100k_text.bin       — тот же формат, что convert_dbpedia.py
                                    (<II nTrain dim> train f32 <I nTest> test f32
                                     <I Kgt> neighbors i32) => loadDBpediaRaw готов;
  /tmp/dbpedia100k_text.jsonl     — по строке на док корпуса, выровнено с train:
                                    {"i":..,"id":..,"title":..,"text":..};
  /tmp/dbpedia100k_queries.jsonl  — то же для запросов, выровнено с test.

Шарды parquet лежат в scratch/hf_dbpedia (гитигнорится, скачиваются
huggingface_hub'ом при отсутствии). Запуск:
  scratch/bm25_venv/bin/python scripts/convert_dbpedia_hf.py
"""

import glob
import json
import os
import struct
import sys

import numpy as np
import pyarrow.parquet as pq

N_CORPUS = 100_000
N_QUERIES = 1_000
K_GT = 100

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SHARD_DIR = os.path.join(ROOT, "scratch", "hf_dbpedia", "data")
SHARDS = [
    "train-00000-of-00026-3c7b99d1c7eda36e.parquet",
    "train-00001-of-00026-2b24035a6390fdcb.parquet",
    "train-00002-of-00026-b05ce48965853dad.parquet",
]
BIN_OUT = "/tmp/dbpedia100k_text.bin"
DOCS_OUT = "/tmp/dbpedia100k_text.jsonl"
QUERIES_OUT = "/tmp/dbpedia100k_queries.jsonl"


def ensure_shards():
    missing = [s for s in SHARDS if not os.path.exists(os.path.join(SHARD_DIR, s))]
    if not missing:
        return
    from huggingface_hub import hf_hub_download
    for s in missing:
        print(f"скачиваем {s} …")
        hf_hub_download("KShivendu/dbpedia-entities-openai-1M", f"data/{s}",
                        repo_type="dataset",
                        local_dir=os.path.join(ROOT, "scratch", "hf_dbpedia"))


def load_rows():
    ids, titles, texts, embs = [], [], [], []
    need = N_CORPUS + N_QUERIES
    for s in SHARDS:
        t = pq.read_table(os.path.join(SHARD_DIR, s))
        ids.extend(t["_id"].to_pylist())
        titles.extend(t["title"].to_pylist())
        texts.extend(t["text"].to_pylist())
        flat = t["openai"].combine_chunks().flatten().to_numpy(zero_copy_only=False)
        embs.append(flat.reshape(t.num_rows, -1).astype(np.float32))
        if len(ids) >= need:
            break
    emb = np.vstack(embs)[:need]
    if len(ids) < need:
        sys.exit(f"строк {len(ids)} < {need}: докачай шарды")
    return ids[:need], titles[:need], texts[:need], emb


def main():
    ensure_shards()
    ids, titles, texts, emb = load_rows()
    dim = emb.shape[1]
    emb /= np.linalg.norm(emb, axis=1, keepdims=True)  # angular: cosine = dot

    train, test = emb[:N_CORPUS], emb[N_CORPUS:]
    print(f"corpus {train.shape}, queries {test.shape}; брутфорс GT top-{K_GT} …")

    # 1000 x 100k полностью влезает: 400МБ f32
    sims = test @ train.T
    part = np.argpartition(-sims, K_GT, axis=1)[:, :K_GT]
    order = np.argsort(-np.take_along_axis(sims, part, axis=1), axis=1)
    gt = np.take_along_axis(part, order, axis=1).astype(np.int32)

    with open(BIN_OUT, "wb") as out:
        out.write(struct.pack("<II", train.shape[0], dim))
        out.write(train.tobytes())
        out.write(struct.pack("<I", test.shape[0]))
        out.write(test.tobytes())
        out.write(struct.pack("<I", K_GT))
        out.write(gt.tobytes())

    with open(DOCS_OUT, "w") as f:
        for i in range(N_CORPUS):
            f.write(json.dumps({"i": i, "id": ids[i], "title": titles[i],
                                "text": texts[i]}, ensure_ascii=False) + "\n")
    with open(QUERIES_OUT, "w") as f:
        for j in range(N_QUERIES):
            i = N_CORPUS + j
            f.write(json.dumps({"i": j, "id": ids[i], "title": titles[i],
                                "text": texts[i]}, ensure_ascii=False) + "\n")

    # смоук: ближайший сосед первого запроса должен быть тематически близок
    q0, n0 = titles[N_CORPUS], titles[gt[0][0]]
    print(f"пример: запрос {q0!r} -> ближайший док {n0!r} (cos={sims[0][gt[0][0]]:.4f})")
    for p in (BIN_OUT, DOCS_OUT, QUERIES_OUT):
        print(f"{p}: {os.path.getsize(p)/1e6:.1f} МБ")


if __name__ == "__main__":
    main()
