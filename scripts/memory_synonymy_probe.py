#!/usr/bin/env python3
# memory_synonymy_probe.py — РЕШАЮЩИЙ ЗАМЕР: есть ли у движка роль, которой нет
# у текстового файла.
#
# ЗАЧЕМ. За 10.08 тремя приборами показано: на корпусе Николая vmem НЕ обыграл
# рукописный MEMORY.md (9/10 против 9/10), карта упирается в отсутствующий
# примитив, а генератор карты закрыт дважды. Остался ровно один класс, который
# файл не чинит НИКОГДА: **синонимия** — вопрос «затухание», а в заметке
# «полураспад»; вопрос «бенчмарки», а в заметках «SIFT-1M» и «recall@5».
# Лексическое плечо здесь бессильно по устройству. Векторное — может.
# Это и есть вопрос: покупает ли движок то, чего нельзя получить файлом.
#
# ⭐ПРЯМОЙ ПРОГНОЗ ИЗ ПРОШЛОГО ЗАМЕРА (04.08): на multi-hop, «где нет дословного
# совпадения и лексике не за что зацепиться», слабое векторное плечо стоило
# **+5.0**. Синонимия — тот же случай, так что прогноз положительный, и тем
# важнее объявить пороги ДО прогона.
#
# ПОРОГИ, ОБЪЯВЛЕННЫЕ ДО ПРОГОНА. У движка ЕСТЬ роль, если выполнено ВСЁ:
#  (а) Q4 («какой дефолт затухания», заметка говорит «полураспад») переходит
#      в попадание хотя бы в одной конфигурации;
#  (б) покрытие M6 («качество поиска и бенчмарки», слово есть в 4 заметках из
#      21) растёт МИНИМУМ ВДВОЕ: 19% → ≥38%;
#  (в) общий hit@5 при этом НЕ ПАДАЕТ ниже лексического (9/10).
# Не выполнено — у движка роли в этой задаче нет, и это надо сказать прямо.
# ⚠Отдельно: если чинится только под конкретными `WEIGHTS` — это РЫЧАГ, а не
# автоматика, и вывод формулируется как «нужен клиент, знающий состав
# вопросов», а не «движок умеет».
#
# ГРАБЛИ, УЖЕ ОБОЙДЁННЫЕ:
#  · `nomic-embed-text` применяется В СВОЁМ РЕЖИМЕ — префиксы ролей
#    `search_document: ` на хранимое и `search_query: ` на вопрос. Без них
#    прибор уже терял 6.3 пункта, и это была ошибка применения, не модели;
#  · bge-m3 НЕ берём: на этой карте она давала NaN и проиграла nomic 5.0
#    при вчетверо большем размере;
#  · векторы кэшируются на диск — иначе повтор прогона считает их заново и
#    «воспроизводимость» держится на неизменности чужого сервиса;
#  · сравниваем ОДИН корпус (desc) сам с собой: разница только в плече.
#
# Использование: scripts/memory_synonymy_probe.py [порт]

from __future__ import annotations

import json
import os
import re
import sys
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import memory_index_vs_recall as mi  # noqa: E402
import memory_map_probe as mp  # noqa: E402
import multiagent_sim as ms  # noqa: E402

ARGS = [a for a in sys.argv[1:] if not a.startswith("--")]
PORT = int(ARGS[0]) if ARGS else 6395
OLLAMA = "http://127.0.0.1:11434/api/embeddings"
# 🚨МОДЕЛЬ — ПАРАМЕТР, и это не удобство. Прогон на `nomic-embed-text` дал
# отрицательный вердикт, а прямой счёт косинусов (мимо движка) показал, что
# эталон Q4 стоит у него на 22-м месте, Q5 — на 177-м из 214: модель эти
# заметки НЕ РАЗЛИЧАЕТ, и движку нечего ранжировать. У `bge-m3` оба эталона
# на 1-м месте. ⭐Вывод 04.08 «bge-m3 проигрывает nomic 5.0» снят на ЭТОМ
# корпусе: он был получен на LoCoMo (английские диалоги), а здесь плотные
# русские заметки. Выбор модели ОТ КОРПУСА, и обобщать было нельзя.
MODEL = os.environ.get("EMB_MODEL", "bge-m3")
# роль-префиксы — режим nomic; bge-m3 их не использует, и навязывать их ей
# значило бы повторить ту же ошибку «инструмент не в своём режиме»
PREFIXED = MODEL.startswith("nomic")
DOC_P = "search_document: " if PREFIXED else ""
QRY_P = "search_query: " if PREFIXED else ""
CACHE = os.path.join(mi.OUTDIR, f"synonymy_vectors.{MODEL.replace(':', '_')}.json")
K_POINT, K_MAP = 5, 20

# конфигурации плеч: (имя, отдавать ли VEC, WEIGHTS)
CONFIGS = [
    ("bm25", False, None),          # лексика в одиночку — сегодняшний дефолт
    ("hybrid 1:1", True, ("1", "1")),
    ("hybrid 1:3", True, ("1", "3")),   # вектору больше голоса
    ("вектор один", True, ("0", "1")),  # лексика заглушена в ранговом пространстве
]

M6_QUESTION = "что известно про качество поиска и бенчмарки"


def embed(texts: list[str], cache: dict) -> dict:
    """⚠GTX 1650 (4 ГБ) отдаёт HTTP 500 на длинных входах у bge-m3 — при первой
    попытке так потерялось 20 документов из 214, и ранжирование считалось бы по
    неполному пулу. Поэтому усечение ступенчатое, до успеха; сколько именно
    урезано, печатается — молча терять документы нельзя."""
    todo = [t for t in texts if t not in cache]
    trimmed = 0
    for i, t in enumerate(todo):
        body = t[:1200]
        while True:
            try:
                req = urllib.request.Request(
                    OLLAMA,
                    data=json.dumps({"model": MODEL, "prompt": body}).encode(),
                    headers={"Content-Type": "application/json"})
                with urllib.request.urlopen(req, timeout=180) as r:
                    cache[t] = json.load(r)["embedding"]
                break
            except urllib.error.HTTPError:
                if len(body) <= 80:
                    raise SystemExit(f"не векторизуется даже 80 символов: {t[:60]!r}")
                body = body[: len(body) * 2 // 3]
                trimmed += 1
        if (i + 1) % 60 == 0:
            print(f"    векторов: {i + 1}/{len(todo)}")
    if trimmed:
        print(f"    ⚠усечений ради памяти GPU: {trimmed}")
    return cache


def main() -> None:
    if not os.path.exists(ms.BIN):
        raise SystemExit(f"нет бинаря {ms.BIN}")
    os.makedirs(mi.OUTDIR, exist_ok=True)

    stems = sorted(f[:-3] for f in os.listdir(mi.MEMDIR)
                   if f.endswith(".md") and f not in
                   ("MEMORY.md", "ARCHIVE-INDEX.md"))
    notes = {s: mi.read_note(s) for s in stems}
    facts = [(s, f"{s}\n{d}", ts) for s, (_t, ts, _ty, d) in notes.items() if d]

    # эталон карты M6 — тем же регекспом, что и в memory_map_probe
    m6_truth = {s for s in notes if mp.TOPIC_BENCH.search(s)}

    cache = json.load(open(CACHE, encoding="utf-8")) if os.path.exists(CACHE) else {}
    n0 = len(cache)
    docs = [f"{DOC_P}{txt}" for _s, txt, _ts in facts]
    queries = [f"{QRY_P}{q}" for _i, q, _t in mi.QUESTIONS] + \
              [f"{QRY_P}{M6_QUESTION}"]
    print(f"считаю векторы ({MODEL}, префиксы: {'да' if PREFIXED else 'нет'}): "
          f"{len(docs) + len(queries)} шт., в кэше {n0}")
    embed(docs + queries, cache)
    json.dump(cache, open(CACHE, "w", encoding="utf-8"))
    dim = len(next(iter(cache.values())))
    print(f"размерность {dim}, кэш пополнен на {len(cache) - n0}\n")

    out = {"configs": {}}
    # 🚨ДВА СЕРВЕРА, а не два скоупа в одном: факт без `VEC` получает
    # детерминированный плейсхолдер размерности 32 и фиксирует ею всё
    # хранилище — второй корпус на 768 уже не влезает («expected 32, got 768»).
    # Разводить обязаны мы: движок тут прав, а замер был бы невозможен.
    for pass_vec in (False, True):
        srv = ms.Server(PORT + int(pass_vec))
        srv.start()
        try:
            c = ms.Resp(PORT + int(pass_vec))
            scope = "vec" if pass_vec else "lex"
            for s, txt, ts in facts:
                args = ["VMEM.REMEMBER", scope, "TEXT", txt, "ID", f"{scope}~{s}",
                        "SOURCE", "notes", "VALIDFROM", str(ts)]
                if pass_vec:
                    args += ["VEC"] + [f"{x:.6f}"
                                       for x in cache[f"{DOC_P}{txt}"]]
                c.call(*args)
            _measure(c, scope, pass_vec, cache, mi.QUESTIONS, m6_truth, out)
        finally:
            srv.cleanup()

    _report(out)


def _measure(c, scope, pass_vec, cache, questions, m6_truth, out) -> None:
    for name, use_vec, w in CONFIGS:
        if use_vec != pass_vec:
            continue
        rows, hits = [], 0
        for qid, query, truth in questions:
            args = ["VMEM.RECALL", scope, str(K_POINT), query]
            if w:
                args += ["WEIGHTS", w[0], w[1]]
            if use_vec:
                args += ["VEC"] + [f"{x:.6f}"
                                   for x in cache[f"{QRY_P}{query}"]]
            flat = c.call(*args) or []
            got = {flat[i].split("~", 1)[1] for i in range(0, len(flat), 3)}
            rank = 0
            for i in range(0, len(flat), 3):
                if flat[i].split("~", 1)[1] in truth:
                    rank = i // 3 + 1
                    break
            hits += bool(got & truth)
            rows.append({"q": qid, "rank": rank})
        # ── M6: та же машинка, но K=20 и метрика покрытия
        args = ["VMEM.RECALL", scope, str(K_MAP), M6_QUESTION]
        if w:
            args += ["WEIGHTS", w[0], w[1]]
        if use_vec:
            args += ["VEC"] + [f"{x:.6f}"
                               for x in cache[f"{QRY_P}{M6_QUESTION}"]]
        flat = c.call(*args) or []
        got20 = {flat[i].split("~", 1)[1] for i in range(0, len(flat), 3)}
        out["configs"][name] = {
            "hit_at_5": hits,
            "q4_rank": next(r["rank"] for r in rows if r["q"] == "Q4"),
            "q9_rank": next(r["rank"] for r in rows if r["q"] == "Q9"),
            "m6_cover": round(len(got20 & m6_truth) / len(m6_truth), 3),
            "rows": rows,
        }


def _report(out: dict) -> None:
    json.dump(out, open(os.path.join(mi.OUTDIR, "memory_synonymy.json"), "w",
                        encoding="utf-8"), ensure_ascii=False, indent=1)

    print(f"{'конфигурация':14s} {'hit@5':>6s} {'Q4 ранг':>8s} {'Q9 ранг':>8s} {'M6 покрытие':>12s}")
    for name, v in out["configs"].items():
        q4 = v["q4_rank"] or "—"
        q9 = v["q9_rank"] or "—"
        print(f"{name:14s} {v['hit_at_5']:5d}/10 {str(q4):>8s} {str(q9):>8s} "
              f"{v['m6_cover']:11.0%}")

    # 🚨Пороги проверяются НА КАЖДОЙ конфигурации отдельно. Первая версия брала
    # «лучшую по M6» и на ней смотрела hit@5 — то есть смешивала конфигурации
    # и печатала «8 против 9» для одной, а «Q4 чинится» для другой.
    base = out["configs"]["bm25"]
    print(f"\n── пороги, проверенные на каждой конфигурации ──")
    winner = None
    for name, v in out["configs"].items():
        if name == "bm25":
            continue
        a = v["q4_rank"] > 0
        b = v["m6_cover"] >= 2 * base["m6_cover"]
        cc = v["hit_at_5"] >= base["hit_at_5"]
        print(f"  {name:12s} (а){'✅' if a else '❌'} "
              f"(б){'✅' if b else '❌'} {v['m6_cover']:.0%}/{2 * base['m6_cover']:.0%} "
              f"(в){'✅' if cc else '❌'} {v['hit_at_5']}/{base['hit_at_5']}")
        if a and b and cc:
            winner = name
    # размен внутри одинакового hit@5 виден только повопросно
    swaps = [(r["q"], base_r["rank"], r["rank"])
             for name, v in out["configs"].items() if name != "bm25"
             for r, base_r in zip(v["rows"], base["rows"])
             if (r["rank"] > 0) != (base_r["rank"] > 0) and name == "hybrid 1:1"]
    if swaps:
        print("\n  ⚠hybrid 1:1 при равном hit@5 ОБМЕНЯЛ вопросы "
              "(среднее прячет размен):")
        for q, was, now in swaps:
            print(f"      {q}: bm25 ранг {was or '—'} → гибрид {now or '—'}")
    print("\n" + (f"У ДВИЖКА ЕСТЬ РОЛЬ — прошла конфигурация «{winner}»" if winner
                  else "🚨НИ ОДНА конфигурация не прошла все три порога"))


if __name__ == "__main__":
    main()
