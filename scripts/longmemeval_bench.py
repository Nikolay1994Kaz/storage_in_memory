#!/usr/bin/env python3
# longmemeval_bench.py — ЦИФРА КАЧЕСТВА ПОИСКА на LongMemEval_S.
#
# ЗАЧЕМ. У соседей есть опубликованное число (MemPalace: 96.6% R@5), у нас
# рядом положить нечего. Все наши замеры recall — это recall ANN-индекса против
# полного перебора, то есть качество ПРИБЛИЖЕНИЯ. Вопрос «насколько хорошо
# память находит нужное» ими не отвечается: разные величины. Здесь меряется
# та же величина тем же способом, что у них.
#
# ⭐ЧЕГО ЭТОТ ЗАМЕР НЕ ДОКАЗЫВАЕТ. При ~48 кандидатах на вопрос никакого
# приближения не происходит ни у них, ни у нас — оба перебирают всё. Значит
# число меряет ЭМБЕДДЕР, а не архитектуру памяти, и совпадение с 96.6%
# означает ПАРИТЕТ («наш поиск ничего не теряет»), а не превосходство. Это и
# нужно: вопрос задают как гейт, а не как соревнование.
#
# ПРОТОКОЛ ПОВТОРЁН ДОСЛОВНО по их benchmarks/longmemeval_bench.py:
#   • документ сессии = ТОЛЬКО реплики роли user, склеенные "\n"
#     (реплики ассистента выбрасываются целиком);
#   • сессии без реплик user в корпус не попадают;
#   • метрика recall_any@5 = «лежит ли ХОТЯ БЫ ОДНА размеченная сессия
#     (answer_session_ids) в топ-5»; LLM не вызывается нигде;
#   • эмбеддер all-MiniLM-L6-v2, max_seq_length=256 (сессия длиной ~9.6 тыс.
#     символов усекается до первой ~1/10 — их число меряется на ней же).
#
# РУКИ (arms):
#   exact  — точный косинус в numpy. Потолок эмбеддера и одновременно то, что
#            при 48 кандидатах делает ChromaDB. Независимая реализация →
#            служит ОРАКУЛОМ для движка: расхождение = наш дефект.
#   vsim   — VSIM.SEARCHFILTER: наш чистый векторный путь (один индекс на все
#            вопросы, отбор по колоночному атрибуту q).
#   vmem0  — VMEM.RECALL, все факты свежие: виден вклад слияния с BM25.
#   vmemd  — VMEM.RECALL, VALIDFROM = НАСТОЯЩАЯ дата сессии: виден вклад
#            затухания 2^(−age/halfLife). ⚠Именно здесь ожидается провал:
#            период полураспада 30 дней, а стог растянут на месяцы, и ответ
#            часто лежит в СТАРОЙ сессии. Замер обязан это показать, а не
#            обойти.
#
# КОНТРОЛИ (без них таблица не умеет краснеть):
#   shuffled — вектор вопроса подменён вектором ДРУГОГО вопроса. Ожидание:
#              recall падает до случайного. Если не падает — мерялось не то.
#   random   — аналитическое 5/N: что даёт слепой выбор.
#
# ⭐ЕДИНАЯ ТОЧНОСТЬ. Векторы округляются до 6 знаков ДО обеих рук: движок
# получает по RESP ровно те числа, на которых считает numpy. Иначе расхождение
# округления было бы неотличимо от дефекта движка.
#
# Использование (только --флаги: multiagent_sim читает позиционный argv):
#   scripts/longmemeval_bench.py --limit 20          # дымовой прогон
#   scripts/longmemeval_bench.py                     # все 500
#   scripts/longmemeval_bench.py --arms exact,vsim   # выбрать руки

from __future__ import annotations

import argparse
import hashlib
import json
import os
import random
import sys
import time
from datetime import datetime, timezone

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

# ⚠multiagent_sim читает sys.argv НА ИМПОРТЕ (`PORT = int(ARGS[0])`), поэтому
# наши собственные аргументы его роняют. Прячем argv на время импорта — иначе
# «--arms exact,vsim» падает до первой строки замера.
_argv, sys.argv = sys.argv, sys.argv[:1]
import multiagent_sim as ms  # noqa: E402
sys.argv = _argv

DATA = os.path.expanduser(
    "~/storage_in_memory/scratch/longmemeval/longmemeval_s_cleaned.json")
CACHE_DIR = os.path.expanduser("~/storage_in_memory/scratch/longmemeval")
MODEL_NAME = "all-MiniLM-L6-v2"
VENV_HINT = "нужен sentence-transformers: см. scratchpad/.venv"
K = 5
ROUND = 6  # знаков после запятой в векторе, общих для numpy и движка

ALL_ARMS = ("exact", "vsim", "bm25", "vmem0", "vmemd", "vmema",
            "shuffled", "bm25s", "vmems")


# ─────────────────────────── корпус ─────────────────────────────────────────

def parse_date(s: str) -> int:
    """'2023/05/20 (Sat) 02:21' → unix. Формат стабилен во всём датасете."""
    head, _, tail = s.partition(" (")
    hhmm = tail.split(") ", 1)[1] if ") " in tail else "00:00"
    dt = datetime.strptime(f"{head} {hhmm}", "%Y/%m/%d %H:%M")
    return int(dt.replace(tzinfo=timezone.utc).timestamp())


class Question:
    __slots__ = ("qid", "qtype", "text", "date", "sids", "docs", "dates",
                 "correct")

    def __init__(self, e: dict) -> None:
        self.qid = e["question_id"]
        self.qtype = e["question_type"]
        self.text = e["question"]
        self.date = parse_date(e["question_date"])
        self.sids: list[str] = []
        self.docs: list[str] = []
        self.dates: list[int] = []
        self.correct = set(e["answer_session_ids"])
        for sid, sess, d in zip(e["haystack_session_ids"],
                                e["haystack_sessions"],
                                e["haystack_dates"]):
            # ⭐дословно как у них: только реплики user, пустые сессии выпадают
            user_turns = [t["content"] for t in sess if t["role"] == "user"]
            if not user_turns:
                continue
            self.sids.append(sid)
            self.docs.append("\n".join(user_turns))
            self.dates.append(parse_date(d))


def load_questions(limit: int | None) -> list[Question]:
    raw = json.load(open(DATA, encoding="utf-8"))
    if limit:
        raw = raw[:limit]
    return [Question(e) for e in raw]


# ─────────────────────────── эмбеддинги ─────────────────────────────────────

def embed_all(qs: list[Question], tag: str) -> tuple[dict[str, int], np.ndarray,
                                                     np.ndarray]:
    """Эмбеддит уникальные тексты документов + все вопросы. Кэш на диск:
    повторный прогон не платит 17 минут заново."""
    texts: list[str] = []
    index: dict[str, int] = {}
    for q in qs:
        for d in q.docs:
            if d not in index:
                index[d] = len(texts)
                texts.append(d)
    qtexts = [q.text for q in qs]

    sig = hashlib.sha256(
        (tag + "|" + MODEL_NAME + "|" + str(len(texts)) + "|" +
         str(len(qtexts))).encode()).hexdigest()[:16]
    cache = os.path.join(CACHE_DIR, f"emb_{sig}.npz")
    if os.path.exists(cache):
        z = np.load(cache)
        print(f"эмбеддинги из кэша: {cache}")
        return index, z["docs"], z["qs"]

    try:
        from sentence_transformers import SentenceTransformer
    except ImportError:
        sys.exit(f"нет sentence_transformers. {VENV_HINT}")

    m = SentenceTransformer(MODEL_NAME)
    print(f"эмбеддинг: {len(texts)} уникальных документов + {len(qtexts)} "
          f"вопросов (max_seq_length={m.max_seq_length})")
    t0 = time.time()
    dv = m.encode(texts, batch_size=64, normalize_embeddings=True,
                  show_progress_bar=False).astype(np.float32)
    qv = m.encode(qtexts, batch_size=64, normalize_embeddings=True,
                  show_progress_bar=False).astype(np.float32)
    print(f"  готово за {time.time() - t0:.1f} с "
          f"({len(texts) / max(time.time() - t0, 1e-9):.0f} док/с)")

    # ⭐единая точность для обеих рук
    dv = np.round(dv, ROUND).astype(np.float32)
    qv = np.round(qv, ROUND).astype(np.float32)
    np.savez(cache, docs=dv, qs=qv)
    return index, dv, qv


def fmt_vec(v: np.ndarray) -> list[str]:
    return [f"{x:.6f}" for x in v]


# ─────────────────────────── руки ───────────────────────────────────────────

def hit(q: Question, ranked_sids: list[str]) -> float:
    """recall_any@K — дословно их формула."""
    return float(any(s in q.correct for s in ranked_sids[:K]))


def shuffled_order(n: int) -> list[int]:
    """Перестановка «чей вопрос задаём». Общая для всех рук: контроль в numpy
    и контроль через движок обязаны быть одним и тем же возмущением, иначе их
    результаты несопоставимы."""
    rnd = random.Random(20260803)
    order = list(range(n))
    rnd.shuffle(order)
    if n > 1 and all(order[i] == i for i in range(n)):
        order = order[1:] + order[:1]
    return order


def arm_exact(qs, index, dv, qv, shuffle=False) -> list[float]:
    """Точный косинус в numpy. shuffle=True → отрицательный контроль."""
    order = shuffled_order(len(qs)) if shuffle else list(range(len(qs)))
    out = []
    for i, q in enumerate(qs):
        rows = np.array([index[d] for d in q.docs])
        sims = dv[rows] @ qv[order[i]]
        top = np.argsort(-sims)[:K]
        out.append(hit(q, [q.sids[j] for j in top]))
    return out


def pipeline(c: ms.Resp, cmds: list[tuple]) -> list:
    """Отправить пачку команд и прочитать все ответы. 24 тыс. вставок по
    одному round-trip были бы дороже самого замера."""
    res = []
    CHUNK = 200
    for s in range(0, len(cmds), CHUNK):
        batch = cmds[s:s + CHUNK]
        buf = []
        for args in batch:
            buf.append(f"*{len(args)}\r\n".encode())
            for a in args:
                b = str(a).encode()
                buf.append(b"$%d\r\n%s\r\n" % (len(b), b))
        c.sock.sendall(b"".join(buf))
        for _ in batch:
            res.append(c._read())
    return res


def arm_vsim(qs, index, dv, qv, port) -> list[float]:
    srv = ms.Server(port)
    srv.start()
    try:
        c = ms.Resp(port)
        cmds = []
        keymap = {}
        for i, q in enumerate(qs):
            for j, d in enumerate(q.docs):
                key = f"{i}_{j}"
                keymap[key] = (i, j)
                cmds.append(("VSIM.ADDATTR", key, "CAT", "q", str(i), "VEC",
                             *fmt_vec(dv[index[d]])))
        t0 = time.time()
        pipeline(c, cmds)
        print(f"  vsim: вставлено {len(cmds)} за {time.time() - t0:.1f} с")

        out = []
        short_out = 0
        for i, q in enumerate(qs):
            # ⚠НЕ VSIM.SEARCHFILTER: тот в KV-режиме дёргает GET "<field>:<key>"
            # и колоночных CAT-атрибутов не видит вовсе (вернул бы пусто).
            # Колоночный отбор — это VSIM.FILTER. Ответ плоский: key, dist, …
            r = c.call("VSIM.FILTER", K, "EQ", "q", str(i), "VEC",
                       *fmt_vec(qv[i]))
            flat = r or []
            if len(flat) // 2 < K:
                short_out += 1
            sids = []
            for t in range(0, len(flat), 2):
                qi, j = keymap[flat[t]]
                sids.append(qs[qi].sids[j])
            out.append(hit(q, sids))
        print(f"  {'🚨' if short_out else '✅'}vsim: выдач короче K={K}: "
              f"{short_out}")
        c.close()
        return out
    finally:
        srv.cleanup()


def arm_vmem(qs, index, dv, qv, port, dated: bool, novec: bool = False,
             asof: bool = False,
             shuffle: bool = False) -> tuple[list[float], list[str]]:
    """Возвращает (попадания, топ-1 скоры). Скоры нужны не для отчёта, а как
    улика: если датированная и недатированная руки дают ПОБАЙТНО те же скоры,
    значит VALIDFROM не долетел, и совпадение их recall ничего не значит.
    Тот же класс ловушки, что «согласованность двух счётчиков ≠ работа»."""
    srv = ms.Server(port)
    srv.start()
    try:
        c = ms.Resp(port)
        cmds = []
        owner = []  # параллельно cmds: (вопрос, номер документа)
        for i, q in enumerate(qs):
            for j, d in enumerate(q.docs):
                args = ["VMEM.REMEMBER", f"q{i}", "TEXT", q.docs[j]]
                if dated:
                    args += ["VALIDFROM", str(q.dates[j])]
                args += ["VEC", *fmt_vec(dv[index[d]])]
                cmds.append(tuple(args))
                owner.append((i, j))
        t0 = time.time()
        ids = pipeline(c, cmds)
        print(f"  vmem{'d' if dated else '0'}: вставлено {len(cmds)} за "
              f"{time.time() - t0:.1f} с")
        idmap = {}
        for fid, (i, j) in zip(ids, owner):
            idmap[(i, str(fid))] = qs[i].sids[j]

        # ⭐ПРИБОРЫ, а не только результат: сколько выдач короче K и сколько
        # вернувшихся id не разрешилось в сессию. Неразрешённый id молча
        # укорачивает выдачу и ЗАНИЖАЕТ recall — дефект, который выглядит как
        # скромность и потому не бросается в глаза.
        short_out = unresolved = 0
        order = shuffled_order(len(qs)) if shuffle else list(range(len(qs)))

        out, scores = [], []
        for i, q in enumerate(qs):
            # ⚠scope остаётся СВОЙ: пул кандидатов тот же, подменяется только
            # вопрос. Иначе контроль мерил бы пустую память, а не промах.
            src = qs[order[i]]
            args = ["VMEM.RECALL", f"q{i}", K, src.text]
            if asof:
                # ⭐ASOF двигает tEff на дату вопроса, а возраст считается как
                # tEff − valid_from. Без него все факты «старше на три года
                # разом»: добавка 5·age/halfLife в RRF почти одинакова для
                # всех кандидатов и порядок не меняет. Настоящая проверка
                # затухания — только здесь, где возраст измеряется днями и
                # месяцами, как в проде.
                args += ["ASOF", str(q.date)]
            if not novec:
                args += ["VEC", *fmt_vec(qv[order[i]])]
            r = c.call(*args)
            sids = []
            flat = r or []
            # ответ — тройки [id, score, text]
            got = len(flat) // 3
            if got < K:
                short_out += 1
            for t in range(0, len(flat), 3):
                sid = idmap.get((i, str(flat[t])))
                if sid:
                    sids.append(sid)
                else:
                    unresolved += 1
            scores.append(str(flat[1]) if len(flat) > 1 else "")
            out.append(hit(q, sids))
        c.close()
        tag = ("bm25" if novec else "vmem") + ("_shuf" if shuffle else "")
        if unresolved or short_out:
            print(f"  🚨{tag}: выдач короче K={K}: {short_out}; "
                  f"вернувшихся id без сессии: {unresolved} — recall занижен")
        else:
            print(f"  ✅{tag}: все {len(qs)} выдач полные (K={K}), "
                  f"все id разрешились")
        return out, scores
    finally:
        srv.cleanup()


# ─────────────────────────── отчёт ──────────────────────────────────────────

def asof_unwinnable(qs) -> list[int]:
    """Вопросы, у которых ASOF на дату вопроса отрезает ВСЕ размеченные сессии:
    в стоге есть сессии, датированные ПОЗЖЕ вопроса (1471 из 23796). Для них
    промах руки vmema — свойство датасета, а не движка, и валить их в общий
    процент значит наказать движок за чужое."""
    out = []
    for i, q in enumerate(qs):
        cor = [j for j, s in enumerate(q.sids) if s in q.correct]
        if cor and all(q.dates[j] > q.date for j in cor):
            out.append(i)
    return out


def report(qs, results: dict[str, list[float]]) -> None:
    npool = sum(len(q.docs) for q in qs) / len(qs)
    rnd = min(1.0, K / npool)
    print()
    print(f"вопросов: {len(qs)} · кандидатов на вопрос в среднем: {npool:.1f} "
          f"· K={K}")
    print(f"{'рука':10s} {'R@5':>8s}   пояснение")
    print("-" * 62)
    names = {
        "exact": "потолок эмбеддера (= ChromaDB при таком пуле)",
        "vsim": "наш векторный путь",
        "bm25": "VMEM.RECALL без VEC — один лексический ствол",
        "vmem0": "VMEM.RECALL, все факты свежие",
        "vmemd": "VMEM.RECALL, настоящие даты (затухание живое)",
        "vmema": "VMEM.RECALL + ASOF даты вопроса — затухание в упор",
        "shuffled": "КОНТРОЛЬ numpy: вопрос чужой",
        "bm25s": "КОНТРОЛЬ через движок: BM25, вопрос чужой",
        "vmems": "КОНТРОЛЬ через движок: VMEM.RECALL, вопрос чужой",
    }
    for a in ALL_ARMS:
        if a in results:
            v = sum(results[a]) / len(results[a])
            print(f"{a:10s} {v * 100:7.1f}%   {names[a]}")
    print(f"{'random':10s} {rnd * 100:7.1f}%   аналитическое 5/N")

    if "vmema" in results:
        bad = set(asof_unwinnable(qs))
        keep = [i for i in range(len(qs)) if i not in bad]
        v = sum(results["vmema"][i] for i in keep) / len(keep)
        byt = {}
        for i in bad:
            byt[qs[i].qtype] = byt.get(qs[i].qtype, 0) + 1
        print(f"\n⚠vmema: у {len(bad)} вопросов ASOF отрезает все размеченные "
              f"сессии (в стоге есть записи ПОЗЖЕ вопроса) — это потолок "
              f"датасета, не движка.")
        print("  все они одного типа: " +
              ", ".join(f"{k}×{v}" for k, v in sorted(byt.items())) +
              " — просадка этой строки в колонке vmema объясняется ими "
              "целиком, а не затуханием")
        print(f"  vmema на остальных {len(keep)}: {v * 100:.1f}%")
        if "vmemd" in results:
            b = sum(results["vmemd"][i] for i in keep) / len(keep)
            print(f"  vmemd на тех же {len(keep)}: {b * 100:.1f}%  "
                  f"← сравнимая пара, цена затухания = разница")

    # разбивка по типам: все руки сразу — среднее по всем вопросам скрывает,
    # что выигрыш на одном типе оплачен провалом на другом
    ctrl = {"shuffled", "bm25s", "vmems"}
    shown = [a for a in ALL_ARMS if a in results and a not in ctrl]
    if shown:
        print()
        print(f"{'тип вопроса':28s} {'n':>4s}" +
              "".join(f"{a:>9s}" for a in shown))
        print("-" * (32 + 9 * len(shown)))
        for t in sorted({q.qtype for q in qs}):
            idx = [i for i, q in enumerate(qs) if q.qtype == t]
            row = "".join(
                f"{sum(results[a][i] for i in idx) / len(idx) * 100:8.1f}%"
                for a in shown)
            print(f"{t:28s} {len(idx):4d}{row}")


def print_scope() -> None:
    """⭐Граница доказательной силы — ВМЕСТЕ С ЧИСЛАМИ, а не рядом.

    Оговорка, живущая в заметке, повторяет ошибку теста, который не
    запускается: она есть, пока о ней помнят. Здесь она печатается каждым
    прогоном, поэтому цитировать строку, не увидев её ограничения, нельзя.
    """
    print("""
── что отсюда можно цитировать ──────────────────────────────────────
БЕЗОПАСНО   exact / vsim — независимый оракул (сверка с точным перебором
            ПОВОПРОСНО, не по среднему) + отрицательный контроль; векторы
            округлены до 6 знаков ДО обеих рук, поэтому расхождение
            округления не может притвориться дефектом движка.
С ОГОВОРКОЙ vmem* — отрицательный контроль через движок есть, полнота выдач
            и разрешимость id проверены, но НЕЗАВИСИМОГО ОРАКУЛА НЕТ:
            сверить RRF-слияние можно только второй реализацией. И разница с
            exact — единицы вопросов из 500, это ПАРИТЕТ, не лидерство.
НЕЛЬЗЯ      строку bm25 — как сравнение с академическим BM25 (~70% у них):
            разрыв не сведён, утечку контроль снял, объяснение — нет.
НЕЛЬЗЯ      разбивку по типам — как сверку с ИХ разбивкой: дословно совпало
            только совокупное число, по корзинам расхождение 1–3 вопроса в
            обе стороны (вероятно разный рантайм MiniLM), отдельно не
            проверялось.
─────────────────────────────────────────────────────────────────────""")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--limit", type=int, default=0)
    ap.add_argument("--arms", default=",".join(ALL_ARMS))
    ap.add_argument("--port", type=int, default=6399)
    a = ap.parse_args()

    arms = [x for x in a.arms.split(",") if x]
    for x in arms:
        if x not in ALL_ARMS:
            sys.exit(f"неизвестная рука: {x} (есть: {', '.join(ALL_ARMS)})")

    qs = load_questions(a.limit or None)
    print(f"загружено вопросов: {len(qs)}, "
          f"документов: {sum(len(q.docs) for q in qs)}")
    index, dv, qv = embed_all(qs, tag=f"limit{a.limit}")

    res: dict[str, list[float]] = {}
    sc: dict[str, list[str]] = {}
    if "exact" in arms:
        res["exact"] = arm_exact(qs, index, dv, qv)
    if "shuffled" in arms:
        res["shuffled"] = arm_exact(qs, index, dv, qv, shuffle=True)
    if "vsim" in arms:
        res["vsim"] = arm_vsim(qs, index, dv, qv, a.port)
    if "bm25" in arms:
        res["bm25"], sc["bm25"] = arm_vmem(qs, index, dv, qv, a.port + 1,
                                           dated=False, novec=True)
    if "vmem0" in arms:
        res["vmem0"], sc["vmem0"] = arm_vmem(qs, index, dv, qv, a.port + 2,
                                             dated=False)
    if "vmemd" in arms:
        res["vmemd"], sc["vmemd"] = arm_vmem(qs, index, dv, qv, a.port + 3,
                                             dated=True)
    if "vmema" in arms:
        res["vmema"], sc["vmema"] = arm_vmem(qs, index, dv, qv, a.port + 4,
                                             dated=True, asof=True)
    if "bm25s" in arms:
        res["bm25s"], _ = arm_vmem(qs, index, dv, qv, a.port + 5, dated=False,
                                   novec=True, shuffle=True)
    if "vmems" in arms:
        res["vmems"], _ = arm_vmem(qs, index, dv, qv, a.port + 6, dated=False,
                                   shuffle=True)

    report(qs, res)

    # ⭐ОРАКУЛ: движок обязан совпасть с точным перебором вопрос-в-вопрос.
    # Сверяется не среднее (два разных распределения дают одно среднее), а
    # ПОВОПРОСНЫЙ вектор попаданий.
    rc = 0
    if "exact" in res and "vsim" in res:
        diff = [i for i in range(len(qs)) if res["exact"][i] != res["vsim"][i]]
        if diff:
            print(f"\n🚨ОРАКУЛ: vsim расходится с точным перебором на "
                  f"{len(diff)} вопросах: {diff[:10]}")
            rc = 1
        else:
            print("\n✅ОРАКУЛ: vsim совпал с точным перебором повопросно")
    # ⭐УЛИКА ПРОТИВ МОЛЧАЛИВОГО НО-ОПА: датированная рука обязана СЧИТАТЬ
    # иначе. Если скоры совпали побайтно — VALIDFROM не долетел, и равенство
    # recall двух рук не значит «затухание не мешает», оно значит «мы не
    # измерили затухание».
    if "vmem0" in sc and "vmemd" in sc:
        if sc["vmem0"] == sc["vmemd"]:
            print("\n🚨VALIDFROM НЕ ПОДЕЙСТВОВАЛ: скоры датированной и "
                  "недатированной рук совпали побайтно — затухание не измерено")
            rc = 1
        else:
            nd = sum(1 for x, y in zip(sc["vmem0"], sc["vmemd"]) if x != y)
            print(f"\n✅ДАТЫ ДОЛЕТЕЛИ: скоры разошлись на {nd} из "
                  f"{len(sc['vmemd'])} вопросов")

    # ⭐Контроль подмены обязан быть на КАЖДОМ пути, а не только в numpy.
    # Путь памяти дал БОЛЬШЕ потолка эмбеддера — именно там проверка, что
    # выдача зависит от вопроса, а не приходит правильной по построению,
    # нужнее всего.
    for base, ctl in (("exact", "shuffled"), ("bm25", "bm25s"),
                      ("vmem0", "vmems")):
        if base in res and ctl in res:
            e = sum(res[base]) / len(res[base])
            s = sum(res[ctl]) / len(res[ctl])
            if s >= e * 0.5:
                print(f"🚨КОНТРОЛЬ {ctl} НЕ СРАБОТАЛ: подмена вопроса дала "
                      f"{s * 100:.1f}% против {e * 100:.1f}% — эта рука мерит "
                      f"не то")
                rc = 1
            else:
                print(f"✅КОНТРОЛЬ {ctl}: {e * 100:.1f}% → {s * 100:.1f}%")

    print_scope()
    return rc


if __name__ == "__main__":
    raise SystemExit(main())
