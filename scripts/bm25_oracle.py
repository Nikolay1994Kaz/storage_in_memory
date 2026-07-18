#!/usr/bin/env python3
"""Оракул корректности BM25: генерирует golden-файл для Go-теста.

Контракт (обязан бит-в-бит совпадать с будущей Go-реализацией):

Токенизация:
  - lowercase (unicode);
  - токен = непрерывный ран unicode-букв/цифр (letter|digit, смешанные раны
    типа "error404" — один токен);
  - стемминг по-токенно: токен с казахскими буквами (ә ғ қ ң ө ұ ү һ і) —
    БЕЗ стемминга; иначе кириллица -> snowball russian, латиница -> snowball
    english; чисто числовой/прочий -> как есть.

Скоринг (Lucene-вариант BM25, k1=1.2, b=0.75):
  idf(t)      = ln(1 + (N - df + 0.5) / (df + 0.5))          # всегда >= 0
  score(q,d)  = sum_t idf(t) * tf*(k1+1) / (tf + k1*(1 - b + b*dl/avgdl))

Внешний эталон: rank_bm25.BM25Okapi (та же токенизация, k1/b те же) — скрипт
сверяет ПОРЯДОК выдачи с нашей формулой и падает при расхождении (ничьи
толерируются). Расхождение возможно только на термах с df > N/2 (у Okapi там
отрицательный IDF с epsilon-полом) — корпус построен так, чтобы этого избегать.

Запуск (venv с rank_bm25 + snowballstemmer):
  scratch/bm25_venv/bin/python scripts/bm25_oracle.py

Читает  kvstore/vector/testdata/bm25/corpus.json
Пишет   kvstore/vector/testdata/bm25/golden.json
"""

import json
import math
import os
import re
import sys

import snowballstemmer
from rank_bm25 import BM25Okapi

K1 = 1.2
B = 0.75
EPS = 1e-9  # группировка ничьих по скору

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
TESTDATA = os.path.join(ROOT, "kvstore", "vector", "testdata", "bm25")

KAZAKH_LETTERS = set("әғқңөұүһі")
TOKEN_RE = re.compile(r"[^\W_]+", re.UNICODE)  # раны букв/цифр, без underscore

stem_ru = snowballstemmer.stemmer("russian")
stem_en = snowballstemmer.stemmer("english")


def is_cyrillic(tok: str) -> bool:
    return any("Ѐ" <= ch <= "ӿ" for ch in tok)


def is_latin(tok: str) -> bool:
    return any("a" <= ch <= "z" for ch in tok)


def tokenize(text: str) -> list[str]:
    out = []
    for tok in TOKEN_RE.findall(text.lower()):
        if KAZAKH_LETTERS & set(tok):
            out.append(tok)  # казахский: без стемминга (v1)
        elif is_cyrillic(tok):
            out.append(stem_ru.stemWord(tok))
        elif is_latin(tok):
            out.append(stem_en.stemWord(tok))
        else:
            out.append(tok)
    return out


def lucene_bm25(query_toks, doc_toks_list):
    """Наша целевая формула. Возвращает список скоров по докам."""
    n = len(doc_toks_list)
    avgdl = sum(len(d) for d in doc_toks_list) / n
    df = {}
    for toks in doc_toks_list:
        for t in set(toks):
            df[t] = df.get(t, 0) + 1
    scores = []
    for toks in doc_toks_list:
        dl = len(toks)
        tf = {}
        for t in toks:
            tf[t] = tf.get(t, 0) + 1
        s = 0.0
        for t in query_toks:
            if t not in df or t not in tf:
                continue
            idf = math.log(1.0 + (n - df[t] + 0.5) / (df[t] + 0.5))
            f = tf[t]
            s += idf * f * (K1 + 1.0) / (f + K1 * (1.0 - B + B * dl / avgdl))
        scores.append(s)
    return scores


def rank_groups(scores, ids):
    """Ранжирование с группами ничьих: [[id,...], [id,...]] по убыванию скора."""
    ranked = sorted(
        ((s, i) for s, i in zip(scores, ids) if s > EPS),
        key=lambda p: (-p[0], p[1]),
    )
    groups, prev = [], None
    for s, i in ranked:
        if prev is None or abs(prev - s) > EPS:
            groups.append([i])
        else:
            groups[-1].append(i)
        prev = s
    return groups


def main():
    with open(os.path.join(TESTDATA, "corpus.json")) as f:
        corpus = json.load(f)

    docs = corpus["docs"]
    doc_ids = [d["id"] for d in docs]
    doc_toks = [tokenize(d["text"]) for d in docs]
    avgdl = sum(len(t) for t in doc_toks) / len(doc_toks)

    okapi = BM25Okapi(doc_toks, k1=K1, b=B)

    out_queries = []
    for q in corpus["queries"]:
        q_toks = tokenize(q["text"])
        ours = lucene_bm25(q_toks, doc_toks)
        theirs = list(okapi.get_scores(q_toks))

        g_ours = rank_groups(ours, doc_ids)
        g_theirs = rank_groups(theirs, doc_ids)
        if [sorted(g) for g in g_ours] != [sorted(g) for g in g_theirs]:
            print(f"FAIL {q['id']}: расхождение порядка с rank_bm25", file=sys.stderr)
            print(f"  ours:   {g_ours}", file=sys.stderr)
            print(f"  okapi:  {g_theirs}", file=sys.stderr)
            sys.exit(1)

        ranking = sorted(
            ((s, i) for s, i in zip(ours, doc_ids) if s > EPS),
            key=lambda p: (-p[0], p[1]),
        )
        out_queries.append({
            "id": q["id"],
            "text": q["text"],
            "tokens": q_toks,
            "ranking": [{"doc": i, "score": round(s, 6)} for s, i in ranking],
        })
        top = out_queries[-1]["ranking"][:3]
        print(f"{q['id']} {q['text']!r:30} -> {[(r['doc'], r['score']) for r in top]}")

    golden = {
        "params": {
            "k1": K1,
            "b": B,
            "idf": "ln(1 + (N - df + 0.5) / (df + 0.5))",
            "tf_norm": "tf*(k1+1) / (tf + k1*(1 - b + b*dl/avgdl))",
        },
        "tokenizer": "lowercase; runs of unicode letters/digits; kazakh letters (әғқңөұүһі) => no stem; cyrillic => snowball-ru; latin => snowball-en",
        "avgdl": round(avgdl, 6),
        "docs": [
            {"id": i, "tokens": t} for i, t in zip(doc_ids, doc_toks)
        ],
        "queries": out_queries,
    }
    out_path = os.path.join(TESTDATA, "golden.json")
    with open(out_path, "w") as f:
        json.dump(golden, f, ensure_ascii=False, indent=1)
        f.write("\n")
    print(f"\ngolden записан: {out_path} (сверка с rank_bm25 пройдена, {len(out_queries)} запросов)")


if __name__ == "__main__":
    main()
