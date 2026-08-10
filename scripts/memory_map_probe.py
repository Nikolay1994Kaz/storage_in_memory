#!/usr/bin/env python3
# memory_map_probe.py — ЗАМЕР КАРТЫ: собирается ли из памяти ОБЗОР, а не пять
# точечных фактов. Продолжение scripts/memory_index_vs_recall.py, где померен
# точечный recall и найдено, что однострочный `description` догоняет рукописный
# MEMORY.md. Здесь меряется ровно то, ради чего вся затея и начиналась.
#
# ЗАЧЕМ. `MEMORY.md` держат не ради поиска — ради обзорности: «что делали за
# неделю», «что осталось не сделано», «что мы уже опровергли». Точечный recall
# на такие вопросы отвечает пятью фактами, и обзор теряется. Вопрос замера:
# ЧТО ИМЕННО не даёт движок — ранжирование или отсутствующий примитив.
#
# 🚨ГЛАВНОЕ РАЗЛИЧЕНИЕ, ради которого прибор и написан. Карты бывают ДВУХ родов,
# и мерить их одной меркой — та же ошибка шкал, что уже ловилась дважды:
#  · ТЕМАТИЧЕСКАЯ («что известно про трудоустройство») — у вопроса есть слова,
#    поиск в принципе может; меряем, насколько полно;
#  · СТРУКТУРНАЯ («что делали за неделю», «где открытые хвосты») — у вопроса
#    слов НЕТ, есть предикат по атрибуту. Поиск здесь проигрывает не потому,
#    что плох, а потому, что его об этом не спрашивали.
# Смешать их — значит объявить движок негодным за то, чего он не обещал.
#
# ЧЕМ МЕРЯЕТСЯ. Эталон каждой карты — МНОЖЕСТВО заметок, и оно построено
# МЕХАНИЧЕСКИ (окно дат из frontmatter, наличие маркера в теле, регексп по
# именам), а не отобрано руками под результат. Метрика — покрытие: какая доля
# эталона попала в выдачу. Точность считается рядом: карта, набранная мусором,
# бесполезна ровно так же, как пустая.
#
# ПОРОГИ, ОБЪЯВЛЕННЫЕ ДО ПРОГОНА:
#  · движок ГОДИТСЯ для тематической карты, если покрытие ≥ 0.6 при K=20 —
#    две трети темы достаточно, чтобы обзор был честным входом в работу;
#  · движок НЕ ГОДИТСЯ для структурной карты, если покрытие поиском вдвое ниже,
#    чем у того же корпуса с фильтром по атрибуту: это и будет доказательством,
#    что не хватает ПРИМИТИВА, а не качества ранжирования;
#  · рукописный MEMORY.md имеет ПОТОЛОК, равный доле эталона, на которую он
#    вообще ссылается; если потолок < 1.0 — обзор по индексу неполон в
#    принципе, и это меряется, а не предполагается.
#
# ГРАБЛИ, УЖЕ ОБОЙДЁННЫЕ ЗДЕСЬ:
#  · `VMEM.COVERAGE` НЕ является примитивом карты — она про покрытие
#    governance (sealed/declared/revocable). Проверено по docs/COMMANDS.md;
#  · у `RECALL` нет диапазона дат: `ASOF` — это ТОЧКА («что было истиной в
#    момент ts»), а не окно. `HALFLIFE` даёт мягкую свежесть, но не отбор;
#  · отдельный сервер во временном каталоге: личная память :6381 не трогается.
#
# Использование: scripts/memory_map_probe.py [порт]

from __future__ import annotations

import json
import os
import re
import sys
from datetime import datetime, timezone

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import memory_index_vs_recall as mi  # noqa: E402
import multiagent_sim as ms  # noqa: E402

ARGS = [a for a in sys.argv[1:] if not a.startswith("--")]
PORT = int(ARGS[0]) if ARGS else 6398
K_MAP = 20                      # бюджет обзора в начале сессии
NOW = None                      # берётся как максимум дат корпуса, не time()

# ─────────────────────── карты и их механические эталоны ─────────────────────
# Каждая карта: (id, род, вопрос, предикат эталона, TYPE-фильтр или None).
# Предикат получает (stem, text, ts) и возвращает bool.

WEEK = 7 * 86400
# 🚨считаем датой, а не константой: набранные руками таймстампы уже дали
# пустой эталон (окно уехало на год назад и промах был не виден по выдаче)
JULY_END = (int(datetime(2026, 7, 22, tzinfo=timezone.utc).timestamp()),
            int(datetime(2026, 8, 1, tzinfo=timezone.utc).timestamp()))

TOPIC_JOB = re.compile(r"job|ydb|alem|employer|market-pain|niche-search|"
                       r"startup-direction|inference-agent-pain")
TOPIC_BENCH = re.compile(r"bench|locomo|longmemeval|sift|gist|dbpedia|hnswlib|"
                         r"validation|real-data|search-quality|bm25|qps")

MAPS = [
    ("M1", "структурная", "что делали за последнюю неделю",
     lambda s, t, ts: ts >= (NOW - WEEK), None),
    ("M2", "структурная", "что делали в конце июля",
     lambda s, t, ts: JULY_END[0] <= ts < JULY_END[1], None),
    ("M3", "структурная", "что осталось не сделано какие открытые хвосты",
     lambda s, t, ts: "⏭" in t, "open"),
    ("M4", "структурная", "что мы опровергли отменили или отклонили",
     lambda s, t, ts: bool(re.search(r"ОПРОВЕРГНУТ|ОТМЕНЕН|ОТМЕНЁН|отклонён",
                                     t)), "refuted"),
    ("M5", "тематическая", "что известно про трудоустройство и вход в компании",
     lambda s, t, ts: bool(TOPIC_JOB.search(s)), None),
    ("M6", "тематическая", "что известно про качество поиска и бенчмарки",
     lambda s, t, ts: bool(TOPIC_BENCH.search(s)), None),
]


def stamp_type(text: str) -> str:
    """Атрибут, который ПУТЬ ЗАПИСИ мог бы проставлять сам. Здесь он выведен из
    тела заметки — именно чтобы показать, что для этого не нужен ни человек, ни
    модель: маркеры уже стоят в тексте. Порядок важен: «отменено» сильнее
    «не сделано», иначе всякая заметка с ⏭ съест категорию опровержений."""
    if re.search(r"ОПРОВЕРГНУТ|ОТМЕНЕН|ОТМЕНЁН|отклонён", text):
        return "refuted"
    if "⏭" in text:
        return "open"
    return "plain"


def main() -> None:
    global NOW
    if not os.path.exists(ms.BIN):
        raise SystemExit(f"нет бинаря {ms.BIN}")
    os.makedirs(mi.OUTDIR, exist_ok=True)

    stems = sorted(f[:-3] for f in os.listdir(mi.MEMDIR)
                   if f.endswith(".md") and f not in
                   ("MEMORY.md", "ARCHIVE-INDEX.md"))
    notes = {s: mi.read_note(s) for s in stems}
    NOW = max(ts for _t, ts, _ty, _d in notes.values())

    # ── корпуса: только победители первого замера + тело для контроля
    corpora = {
        "whole": [(f"whole~{s}~0", t, {s}, ts, stamp_type(t))
                  for s, (t, ts, _ty, _d) in notes.items()],
        "desc": [(f"desc~{s}~0", f"{s}\n{d}", {s}, ts, stamp_type(t))
                 for s, (t, ts, _ty, d) in notes.items() if d],
    }
    idx = mi.build_index_corpus()
    # ⚠строки индекса штампуются ПО СВОЕМУ ЖЕ ТЕКСТУ: маркеры ⏭ и
    # «ОПРОВЕРГНУТО» стоят и в них, и захардкоженный `plain` дал бы индексу
    # нулевой TYPE-прогон не по существу, а по недосмотру прибора
    corpora["idx"] = [(fid, txt, par, ts or NOW, stamp_type(txt))
                      for fid, txt, par, ts in idx]

    parent_of = {f[0]: f[2] for c in corpora.values() for f in c}
    size_of = {f[0]: len(f[1]) for c in corpora.values() for f in c}

    # ── потолок рукописного индекса: на какую долю эталона он вообще ссылается
    linked = set().union(*[f[2] for f in corpora["idx"]]) if corpora["idx"] else set()

    truth = {}
    for mid, kind, q, pred, tfilt in MAPS:
        truth[mid] = {s for s, (t, ts, _ty, _d) in notes.items() if pred(s, t, ts)}

    srv = ms.Server(PORT)
    srv.start()
    out = {"now": NOW, "k": K_MAP, "maps": {}, "corpora": {}}
    try:
        c = ms.Resp(PORT)
        for scope, facts in corpora.items():
            for fid, text, _p, ts, typ in facts:
                c.call("VMEM.REMEMBER", scope, "TEXT", text, "ID", fid,
                       "SOURCE", "notes", "VALIDFROM", str(ts), "TYPE", typ)
            out["corpora"][scope] = len(facts)

        for mid, kind, q, _pred, tfilt in MAPS:
            T = truth[mid]
            rec = {"kind": kind, "question": q, "truth": len(T),
                   "idx_ceiling": round(len(T & linked) / len(T), 3) if T else 0,
                   "runs": {}}
            for scope in corpora:
                for label, extra in (("plain", []),
                                     ("hl7d", ["HALFLIFE", str(7 * 86400)]),
                                     ("type", ["TYPE", tfilt] if tfilt else None)):
                    if extra is None:
                        continue
                    flat = c.call("VMEM.RECALL", scope, str(K_MAP), q, *extra) or []
                    ids = [flat[i] for i in range(0, len(flat), 3)]
                    got = set().union(*[parent_of[i] for i in ids]) if ids else set()
                    rec["runs"][f"{scope}/{label}"] = {
                        "returned": len(ids),
                        "notes": len(got),
                        "cover": round(len(got & T) / len(T), 3) if T else 0,
                        "prec": round(len(got & T) / len(got), 3) if got else 0,
                        "chars": sum(size_of[i] for i in ids),
                    }
            # ── отсутствующий примитив: точный отбор по предикату (клиентом)
            exact = [f for f in corpora["desc"] if f[2] & T]
            rec["runs"]["desc/ФИЛЬТР"] = {
                "returned": len(exact), "notes": len(exact),
                "cover": 1.0, "prec": 1.0,
                "chars": sum(len(f[1]) for f in exact),
            }
            out["maps"][mid] = rec
    finally:
        srv.cleanup()

    json.dump(out, open(os.path.join(mi.OUTDIR, "memory_map_probe.json"),
                        "w", encoding="utf-8"), ensure_ascii=False, indent=1)

    for mid, kind, q, _p, tfilt in MAPS:
        r = out["maps"][mid]
        print(f"\n═══ {mid} [{kind}] «{q}»")
        print(f"    эталон {r['truth']} заметок; "
              f"потолок рукописного индекса {r['idx_ceiling']:.0%}")
        print(f"    {'способ':16s} {'покрытие':>9s} {'точность':>9s} {'симв.':>8s}")
        for name, v in r["runs"].items():
            print(f"    {name:16s} {v['cover']:9.0%} {v['prec']:9.0%} {v['chars']:8d}")
    print(f"\nсырьё: {os.path.join(mi.OUTDIR, 'memory_map_probe.json')}")


if __name__ == "__main__":
    main()
