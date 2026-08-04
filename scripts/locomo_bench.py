#!/usr/bin/env python3
# locomo_bench.py — КАЧЕСТВО ПОИСКА на LoCoMo.
#
# ЗАЧЕМ. LongMemEval закрыт (scripts/longmemeval_bench.py, 97.4% R@5, паритет с
# 96.6% соседа). LoCoMo — второй бенчмарк, которым меряются в нише, и
# единственная оставшаяся дыра в сравнении. Здесь корпус устроен иначе, и это
# не «ещё одно число рядом»: см. ниже про затухание.
#
# 🚨ГЛАВНОЕ ПРО ПОДПИСЬ. Под именем «LoCoMo X%» в нише лежат РАЗНЫЕ ВЕЛИЧИНЫ:
# у одних это retrieval recall (нашлось ли нужное в выдаче), у других — QA
# accuracy, где ответ агента судит LLM. Числа отличаются в разы и не
# сопоставимы. Автор одного из публикуемых чисел прямо пишет, что методологии
# разнятся («pure token-overlap F1, pure LLM-as-judge, or hybrid»).
# ⭐Здесь меряется ТОЛЬКО retrieval recall. LLM не вызывается нигде. Класть эту
# строку рядом с чужой QA accuracy — тот же дефект, что чинили в SECURITY.md:
# измерение передаёт доверие утверждению, которого не делало.
#
# ⭐ЧЕМ ЭТОТ ЗАМЕР ОТЛИЧАЕТСЯ ОТ LongMemEval — ЗАТУХАНИЕ ВПЕРВЫЕ ИЗМЕРИМО.
# Там разброс дат внутри стога был медиана 11 дней при полураспаде 30, поэтому
# возраст кандидатов почти одинаков, добавка 5·age/halfLife в RRF их не
# переупорядочивает, и замер честно сказал: «рычага нет, затухание не
# проверяется». Здесь разговоры растянуты на 184–293 дня (медиана 238), то есть
# ответ на вопрос регулярно лежит в сессии восьмимесячной давности. Рука vmema
# (ASOF на дату вопроса) — первый случай, когда механизм может показать свою
# цену. Замер обязан её показать, а не обойти.
#
# ПРОТОКОЛ (решён ДО написания кода, по данным разведки):
#   • корпус — 10 разговоров, 272 сессии, 5882 реплики;
#   • ДОКУМЕНТ = РЕПЛИКА (--unit turn, основной режим): LoCoMo размечает
#     evidence на уровне dia_id, и это единственная гранулярность, где оракул
#     точен. Режим --unit session (документ = вся сессия) даётся вторым числом:
#     он мягче и ближе к протоколу LongMemEval, где документом была сессия;
#   • текст документа = «Имя: реплика» + «[фото: подпись]», если у реплики есть
#     картинка. ⭐Это не украшение: 909 из 2361 размеченной реплики (38%) несут
#     изображение, и ответ часто именно в подписи («take a look at this» +
#     "a photo of a painting of a sunset over a lake"). Без подписи такие
#     вопросы неотвечаемы по построению. Одинаково для всех рук;
#   • СКОУП = разговор: 588 реплик в среднем — общая память, много запросов.
#     Пул кандидатов в 12 раз больше, чем у LongMemEval (48);
#   • ВОПРОСЫ: категории 1–4 = 1540 штук. ⭐Категория 5 (adversarial, 446)
#     ИСКЛЮЧЕНА — так делают все, кто публикует числа, и по существу: её задача
#     «модель обязана отказаться отвечать», а не «найти». У неё и answer нет,
#     только adversarial_answer;
#   • МЕТРИКА any@5 — «лежит ли ХОТЯ БЫ ОДНА размеченная реплика в топ-5».
#     Рядом печатается cov@5 (какая ДОЛЯ размеченных реплик попала в топ-5):
#     у multi-hop evidence бывает до 19 реплик, и any@5 там льстит.
#
# ⭐ГИПОТЕЗЫ, ОБЪЯВЛЕННЫЕ ДО ПЕРВОГО ПРОГОНА (иначе замер подгоняется под
# результат задним числом):
#   H1  bm25 заметно ниже vsim. На LongMemEval они шли вровень (96.2 против
#       96.6), но там документ — склейка сессии ~9.6 тыс. знаков, а здесь
#       реплика: медиана 109 знаков. Лексическому стволу не на чем работать.
#   H2  vmem0 (гибрид) ≥ vsim, но прибавка МЕНЬШЕ, чем на LongMemEval,
#       по той же причине.
#   H3  vmema (ASOF) даёт СУЩЕСТВЕННУЮ просадку, ожидаю −10 пунктов и хуже.
#       Полураспад 30 дней против размаха 238: факт восьмимесячной давности
#       получает вес 2^(−8) от свежего. Если просадки НЕТ — первым делом
#       смотреть улику по скорам: скорее всего VALIDFROM/ASOF не долетел.
#   H4  абсолют turn-level: 60–80% any@5. Ниже LongMemEval (97.4%), потому что
#       пул в 12 раз больше, документы короче, а адрес ответа точнее.
#
# ПОРОГИ (назначены заранее, прогон их не двигает):
#   • оракул vsim ↔ exact: расхождение 0 вопросов, иначе прогон недействителен;
#   • контроли: подмена вопроса обязана уронить руку минимум ВДВОЕ;
#   • если vmem0 < vsim — гибрид на этом корпусе вредит. Это результат, а не
#     повод крутить ручки и перегонять.
#
# Использование:
#   scripts/locomo_bench.py --limit 40                 # дымовой прогон
#   scripts/locomo_bench.py                            # все 1540, unit=turn
#   scripts/locomo_bench.py --unit session             # второе число
#   scripts/locomo_bench.py --arms exact,vsim

from __future__ import annotations

import argparse
import hashlib
import json
import os
import random
import re
import sys
import time
from datetime import datetime, timezone

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

# ⚠multiagent_sim читает sys.argv НА ИМПОРТЕ — прячем свои аргументы.
_argv, sys.argv = sys.argv, sys.argv[:1]
import multiagent_sim as ms  # noqa: E402
sys.argv = _argv

DATA = os.path.expanduser("~/storage_in_memory/scratch/locomo/locomo10.json")
CACHE_DIR = os.path.expanduser("~/storage_in_memory/scratch/locomo")
MODEL_NAME = "all-MiniLM-L6-v2"
# ⭐Второй эмбеддер — не украшение, а прибор для отделения диагнозов.
# На MiniLM (384 dim, 2021) векторное плечо даёт 39.1% против 54.9% у BM25, и
# гибрид проигрывает чистой лексике. Вопрос: слияние плохое или плечо слабое?
# nomic-embed-text (768 dim, 2024) отвечает: 57.0%, то есть плечо. Держать
# обе модели обязательно — MiniLM для сопоставимости с нашим же LongMemEval,
# nomic для ответа на вопрос «а если эмбеддер не из позапрошлой эпохи».
OLLAMA_URL = os.environ.get("OLLAMA_URL", "http://localhost:11434") + "/api/embed"
NOMIC_MODEL = "nomic-embed-text"
VENV_HINT = "нужен sentence-transformers: scratch/.venv/bin/python"
K = 5
ROUND = 6  # знаков после запятой в векторе, общих для numpy и движка
ADVERSARIAL = 5  # категория, исключаемая всеми, кто публикует числа

CATEGORY_NAME = {
    1: "multi-hop",
    2: "temporal",
    3: "open-domain",
    4: "single-hop",
}

ALL_ARMS = ("exact", "vsim", "bm25", "vmem0", "vmemd", "vmema",
            "shuffled", "bm25s", "vmems")


# ─────────────────────────── корпус ─────────────────────────────────────────

def parse_date(s: str) -> int:
    """'1:56 pm on 8 May, 2023' → unix. Формат стабилен: 272/272 разобрались."""
    m = re.match(r"(\d+):(\d+)\s*(am|pm)\s+on\s+(\d+)\s+(\w+),\s*(\d+)",
                 s.strip(), re.I)
    if not m:
        raise ValueError(f"дата не разобрана: {s!r}")
    hh, mm, ap, day, mon, yr = m.groups()
    hh = int(hh) % 12 + (12 if ap.lower() == "pm" else 0)
    dt = datetime.strptime(f"{day} {mon} {yr} {hh}:{mm}", "%d %B %Y %H:%M")
    return int(dt.replace(tzinfo=timezone.utc).timestamp())


def norm_evidence(raw: str) -> list[str]:
    """⭐Разметка LoCoMo местами грязная, и грязь тихая: неразрешённый dia_id
    просто уменьшает число целей, то есть ЗАНИЖАЕТ recall — дефект, который
    выглядит как скромность. Чинится ровно три формы, всё остальное считается
    и печатается:
        'D8:6; D9:17'  → две ссылки в одной строке (разделители ; , пробел)
        'D:11:26'      → лишнее двоеточие
        'D30:05'       → ведущий ноль (D30:5 в корпусе есть)
    Не чинится и остаётся в счётчике: 'D10:19' при 16 репликах в сессии,
    'D4:36' при 25, обрубок 'D' — это дефекты самого датасета."""
    out = []
    for p in re.split(r"[;,\s]+", raw.strip()):
        p = p.strip()
        if not p:
            continue
        p = re.sub(r"^D:(\d+):(\d+)$", r"D\1:\2", p)
        m = re.match(r"^D(\d+):0*(\d+)$", p)
        if m:
            p = f"D{m.group(1)}:{m.group(2)}"
        out.append(p)
    return out


class Conversation:
    """Один разговор = один скоуп памяти. Документы общие для всех его
    вопросов — в отличие от LongMemEval, где у каждого вопроса свой стог."""

    __slots__ = ("idx", "docs", "doc_ids", "doc_dates", "turn_to_doc",
                 "last_date")

    def __init__(self, idx: int, rec: dict, unit: str) -> None:
        self.idx = idx
        self.docs: list[str] = []      # тексты документов
        self.doc_ids: list[str] = []   # dia_id или sid — для отладки
        self.doc_dates: list[int] = []
        self.turn_to_doc: dict[str, int] = {}  # dia_id → номер документа

        conv = rec["conversation"]
        sess_keys = [k for k in conv
                     if k.startswith("session_") and not k.endswith("date_time")]
        # порядок сессий по номеру, а не по порядку ключей в JSON
        sess_keys.sort(key=lambda k: int(k.split("_")[1]))

        dates = []
        for sk in sess_keys:
            ts = parse_date(conv[f"{sk}_date_time"])
            dates.append(ts)
            turns = conv[sk]
            if unit == "turn":
                for t in turns:
                    self.turn_to_doc[t["dia_id"]] = len(self.docs)
                    self.docs.append(self._render(t))
                    self.doc_ids.append(t["dia_id"])
                    self.doc_dates.append(ts)
            else:
                di = len(self.docs)
                for t in turns:
                    self.turn_to_doc[t["dia_id"]] = di
                self.docs.append("\n".join(self._render(t) for t in turns))
                self.doc_ids.append(sk)
                self.doc_dates.append(ts)
        self.last_date = max(dates)

    @staticmethod
    def _render(t: dict) -> str:
        """⭐Подпись к фото — часть содержания реплики, а не украшение:
        38% размеченных реплик несут изображение, и ответ часто в подписи."""
        s = f"{t['speaker']}: {t.get('text', '')}".strip()
        cap = t.get("blip_caption")
        if cap:
            s += f" [фото: {cap}]"
        return s


class Question:
    __slots__ = ("conv", "cid", "cat", "text", "targets", "date", "over_k")

    def __init__(self, conv: Conversation, cid: int, q: dict,
                 targets: list[int]) -> None:
        self.conv = conv
        self.cid = cid
        self.cat = q["category"]
        self.text = q["question"]
        self.targets = set(targets)   # номера документов-целей
        # ⭐У вопроса LoCoMo нет своей даты: он задаётся ПОСЛЕ всего разговора.
        # Берём дату последней сессии — тогда возраст факта в руке vmema равен
        # его настоящему возрасту на момент вопроса (0…238 дней).
        self.date = conv.last_date
        # у скольких вопросов целей больше K: полное покрытие им недостижимо
        self.over_k = len(targets) > K


def load(unit: str, limit: int | None) -> tuple[list[Conversation],
                                                list[Question], dict]:
    raw = json.load(open(DATA, encoding="utf-8"))
    convs = [Conversation(i, rec, unit) for i, rec in enumerate(raw)]
    qs: list[Question] = []
    stat = {"adversarial": 0, "no_evidence": 0, "unresolved_refs": 0,
            "dropped": 0, "repaired": 0}
    for ci, rec in enumerate(raw):
        conv = convs[ci]
        for q in rec["qa"]:
            if q["category"] == ADVERSARIAL:
                stat["adversarial"] += 1
                continue
            if not q["evidence"]:
                stat["no_evidence"] += 1
                stat["dropped"] += 1
                continue
            tgt = []
            for e in q["evidence"]:
                parts = norm_evidence(e)
                if parts != [e.strip()]:
                    stat["repaired"] += 1
                for p in parts:
                    di = conv.turn_to_doc.get(p)
                    if di is None:
                        stat["unresolved_refs"] += 1
                    else:
                        tgt.append(di)
            if not tgt:
                stat["dropped"] += 1
                continue
            qs.append(Question(conv, ci, q, sorted(set(tgt))))
    if limit:
        # ⭐срез берётся ПО ВОПРОСАМ, но корпус остаётся полным: урезать стог
        # значило бы облегчить задачу и получить число, которое ничего не
        # предсказывает про полный прогон
        rnd = random.Random(20260804)
        qs = rnd.sample(qs, min(limit, len(qs)))
    return convs, qs, stat


# ─────────────────────────── эмбеддинги ─────────────────────────────────────

def _embed_ollama(texts: list[str], batch: int = 64) -> np.ndarray:
    """nomic-embed-text через локальную ollama. ⚠Проверять доступность надо
    ЗАПРОСОМ К API: `systemctl --user is-active ollama` отвечает inactive и
    при живом сервере — юнит врёт в обе стороны."""
    import json as _json
    import urllib.request
    out = []
    t0 = time.time()
    for s in range(0, len(texts), batch):
        req = urllib.request.Request(
            OLLAMA_URL,
            data=_json.dumps({"model": NOMIC_MODEL,
                              "input": texts[s:s + batch]}).encode(),
            headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=600) as r:
            out.extend(_json.load(r)["embeddings"])
    el = max(time.time() - t0, 1e-9)
    print(f"  готово за {el:.1f} с ({len(texts) / el:.0f} док/с)")
    v = np.array(out, dtype=np.float32)
    v /= np.linalg.norm(v, axis=1, keepdims=True)
    return v


def embed_all(convs: list[Conversation], qs: list[Question], tag: str,
              embedder: str = "minilm") -> tuple[list[np.ndarray], np.ndarray]:
    """Возвращает (векторы документов по разговорам, векторы вопросов)."""
    texts: list[str] = []
    spans: list[tuple[int, int]] = []
    for c in convs:
        spans.append((len(texts), len(texts) + len(c.docs)))
        texts.extend(c.docs)
    qtexts = [q.text for q in qs]

    model = MODEL_NAME if embedder == "minilm" else NOMIC_MODEL
    sig = hashlib.sha256(
        (tag + "|" + model + "|" + str(len(texts)) + "|" +
         str(len(qtexts))).encode()).hexdigest()[:16]
    cache = os.path.join(CACHE_DIR, f"emb_{embedder}_{sig}.npz")
    if os.path.exists(cache):
        z = np.load(cache)
        print(f"эмбеддинги из кэша: {cache}")
        dv, qv = z["docs"], z["qs"]
    else:
        print(f"эмбеддинг ({model}): {len(texts)} документов + "
              f"{len(qtexts)} вопросов")
        if embedder == "nomic":
            dv = _embed_ollama(texts)
            qv = _embed_ollama(qtexts)
        else:
            try:
                from sentence_transformers import SentenceTransformer
            except ImportError:
                sys.exit(f"нет sentence_transformers. {VENV_HINT}")
            m = SentenceTransformer(MODEL_NAME)
            t0 = time.time()
            dv = m.encode(texts, batch_size=64, normalize_embeddings=True,
                          show_progress_bar=False).astype(np.float32)
            qv = m.encode(qtexts, batch_size=64, normalize_embeddings=True,
                          show_progress_bar=False).astype(np.float32)
            el = max(time.time() - t0, 1e-9)
            print(f"  готово за {el:.1f} с ({len(texts) / el:.0f} док/с)")
        # ⭐единая точность для обеих рук: расхождение округления не должно
        # уметь притвориться дефектом движка
        dv = np.round(dv, ROUND).astype(np.float32)
        qv = np.round(qv, ROUND).astype(np.float32)
        np.savez(cache, docs=dv, qs=qv)
    return [dv[a:b] for a, b in spans], qv


def fmt_vec(v: np.ndarray) -> list[str]:
    return [f"{x:.6f}" for x in v]


# ─────────────────────────── метрики ────────────────────────────────────────

def score(q: Question, ranked: list[int]) -> tuple[float, float]:
    """(any@K, cov@K). ⭐Две метрики, потому что одна льстит: у multi-hop
    целей бывает до 19, и «нашлась хотя бы одна» — слабое утверждение."""
    top = ranked[:K]
    hits = sum(1 for d in top if d in q.targets)
    return float(hits > 0), hits / len(q.targets)


def shuffled_order(qs: list[Question]) -> list[int]:
    """Перестановка «чей вопрос задаём». ⭐Требование строже, чем в
    longmemeval: подменённый вопрос обязан быть ИЗ ДРУГОГО РАЗГОВОРА. Иначе
    чужой вопрос про тех же людей и те же события случайно попадает в цель, и
    контроль недобирает — то есть слабее, чем выглядит."""
    rnd = random.Random(20260804)
    n = len(qs)
    order = list(range(n))
    for _ in range(200):
        rnd.shuffle(order)
        if all(qs[order[i]].cid != qs[i].cid for i in range(n)):
            return order
    # запасной ход: сдвиг по кругу внутри отсортированного по разговору списка
    by = sorted(range(n), key=lambda i: qs[i].cid)
    shift = max(1, sum(1 for q in qs if q.cid == qs[by[0]].cid))
    order = [0] * n
    for pos, i in enumerate(by):
        order[i] = by[(pos + shift) % n]
    bad = sum(1 for i in range(n) if qs[order[i]].cid == qs[i].cid)
    if bad:
        print(f"  ⚠контроль: у {bad} вопросов подмена осталась внутри своего "
              f"разговора — контроль на них слабее")
    return order


# ─────────────────────────── руки ───────────────────────────────────────────

def arm_exact(convs, qs, dvs, qv, shuffle=False):
    """Точный косинус в numpy. Потолок эмбеддера и оракул для движка."""
    order = shuffled_order(qs) if shuffle else list(range(len(qs)))
    out, cov = [], []
    for i, q in enumerate(qs):
        sims = dvs[q.cid] @ qv[order[i]]
        top = np.argsort(-sims)[:K]
        a, c = score(q, [int(j) for j in top])
        out.append(a)
        cov.append(c)
    return out, cov


def pipeline(c: ms.Resp, cmds: list[tuple]) -> list:
    """Пачками: 5882 вставки по одному round-trip дороже самого замера."""
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


def arm_vsim(convs, qs, dvs, qv, port):
    srv = ms.Server(port)
    srv.start()
    try:
        c = ms.Resp(port)
        cmds, keymap = [], {}
        for conv in convs:
            for j in range(len(conv.docs)):
                key = f"{conv.idx}_{j}"
                keymap[key] = j
                cmds.append(("VSIM.ADDATTR", key, "CAT", "c", str(conv.idx),
                             "VEC", *fmt_vec(dvs[conv.idx][j])))
        t0 = time.time()
        pipeline(c, cmds)
        print(f"  vsim: вставлено {len(cmds)} за {time.time() - t0:.1f} с")

        out, cov, short = [], [], 0
        for i, q in enumerate(qs):
            # ⚠VSIM.FILTER, не VSIM.SEARCHFILTER: последний в KV-режиме читает
            # ключ "<поле>:<ключ>" и колоночных CAT-атрибутов не видит вовсе.
            r = c.call("VSIM.FILTER", K, "EQ", "c", str(q.cid), "VEC",
                       *fmt_vec(qv[i]))
            flat = r or []
            if len(flat) // 2 < K:
                short += 1
            ranked = [keymap[flat[t]] for t in range(0, len(flat), 2)]
            a, cv = score(q, ranked)
            out.append(a)
            cov.append(cv)
        print(f"  {'🚨' if short else '✅'}vsim: выдач короче K={K}: {short}")
        c.close()
        return out, cov
    finally:
        srv.cleanup()


def sweep_halflife(convs, qs, dvs, qv, port, days: list[int]):
    """⭐Затухание сломано или настраивается? Дефолтный полураспад — 30 дней
    (vmem.go: vmemDefaultHalfLifeSec), а разговоры LoCoMo растянуты на 238.
    Штраф в знаменателе RRF равен λ·age/halfLife при λ=5 и rrfK=60, то есть
    факт возрастом 238 дней получает +39.7 — он проигрывает свежему факту,
    стоящему на СОРОКОВОМ месте по релевантности. Если политика управляема,
    рост полураспада обязан вернуть recall к базе; если нет — дефект в
    механизме, а не в настройке. Вставка одна, меняются только запросы."""
    srv = ms.Server(port)
    srv.start()
    try:
        c = ms.Resp(port)
        cmds, owner = [], []
        for conv in convs:
            for j, d in enumerate(conv.docs):
                cmds.append(("VMEM.REMEMBER", f"c{conv.idx}", "TEXT", d,
                             "VALIDFROM", str(conv.doc_dates[j]),
                             "VEC", *fmt_vec(dvs[conv.idx][j])))
                owner.append((conv.idx, j))
        ids = pipeline(c, cmds)
        idmap = {(ci, str(fid)): j for fid, (ci, j) in zip(ids, owner)}
        out = {}
        for dd in days:
            hits = []
            for i, q in enumerate(qs):
                args = ["VMEM.RECALL", f"c{q.cid}", K, q.text,
                        "ASOF", str(q.date),
                        "HALFLIFE", str(dd * 24 * 3600),
                        "VEC", *fmt_vec(qv[i])]
                flat = c.call(*args) or []
                ranked = [idmap[(q.cid, str(flat[t]))]
                          for t in range(0, len(flat), 3)
                          if (q.cid, str(flat[t])) in idmap]
                hits.append(score(q, ranked)[0])
            out[dd] = sum(hits) / len(hits)
            print(f"  halflife={dd:5d} дн: {out[dd] * 100:.1f}%")
        c.close()
        return out
    finally:
        srv.cleanup()


def arm_vmem(convs, qs, dvs, qv, port, dated: bool, novec: bool = False,
             asof: bool = False, shuffle: bool = False, halflife: int = 0,
             weights: tuple[float, float] | None = None):
    """Возвращает (any, cov, топ-1 скоры). ⭐Скоры — не для отчёта, а улика:
    если датированная и недатированная руки дают ПОБАЙТНО те же скоры, значит
    VALIDFROM не долетел, и равенство их recall означает «не измерили», а не
    «не помешало»."""
    srv = ms.Server(port)
    srv.start()
    try:
        c = ms.Resp(port)
        cmds, owner = [], []
        for conv in convs:
            for j, d in enumerate(conv.docs):
                args = ["VMEM.REMEMBER", f"c{conv.idx}", "TEXT", d]
                if dated:
                    args += ["VALIDFROM", str(conv.doc_dates[j])]
                if not novec:
                    args += ["VEC", *fmt_vec(dvs[conv.idx][j])]
                cmds.append(tuple(args))
                owner.append((conv.idx, j))
        t0 = time.time()
        ids = pipeline(c, cmds)
        tag = ("bm25" if novec else "vmem") + ("_shuf" if shuffle else "")
        print(f"  {tag}: вставлено {len(cmds)} за {time.time() - t0:.1f} с")
        idmap = {(ci, str(fid)): j for fid, (ci, j) in zip(ids, owner)}

        order = shuffled_order(qs) if shuffle else list(range(len(qs)))
        out, cov, scores = [], [], []
        short = unresolved = 0
        for i, q in enumerate(qs):
            # ⚠скоуп остаётся СВОЙ: подменяется только вопрос, иначе контроль
            # мерил бы пустую память, а не промах
            src = qs[order[i]]
            args = ["VMEM.RECALL", f"c{q.cid}", K, src.text]
            if asof:
                # ⭐ASOF двигает tEff на дату вопроса: возраст = tEff −
                # valid_from. Без него все факты «старше на три года разом» и
                # добавка 5·age/halfLife почти одинакова — порядок не меняется.
                args += ["ASOF", str(q.date)]
            if halflife:
                args += ["HALFLIFE", str(halflife * 24 * 3600)]
            if weights and not novec:
                # ⭐Рычаг весов плеч: замер нашёл, что слияние стоит −18.3
                # пункта, когда одно плечо сильно слабее. Здесь проверяется,
                # ВОЗВРАЩАЕТ ли рычаг потерянное — на тех же вопросах, где
                # потеря и была измерена.
                args += ["WEIGHTS", str(weights[0]), str(weights[1])]
            if not novec:
                args += ["VEC", *fmt_vec(qv[order[i]])]
            r = c.call(*args)
            flat = r or []
            if len(flat) // 3 < K:
                short += 1
            ranked = []
            for t in range(0, len(flat), 3):
                di = idmap.get((q.cid, str(flat[t])))
                if di is None:
                    unresolved += 1
                else:
                    ranked.append(di)
            scores.append(str(flat[1]) if len(flat) > 1 else "")
            a, cv = score(q, ranked)
            out.append(a)
            cov.append(cv)
        c.close()
        if unresolved or short:
            print(f"  🚨{tag}: выдач короче K={K}: {short}; id без документа: "
                  f"{unresolved} — recall ЗАНИЖЕН")
        else:
            print(f"  ✅{tag}: все {len(qs)} выдач полные (K={K}), все id "
                  f"разрешились")
        return out, cov, scores
    finally:
        srv.cleanup()


# ─────────────────────────── отчёт ──────────────────────────────────────────

NAMES = {
    "exact": "потолок эмбеддера (точный косинус)",
    "vsim": "наш векторный путь",
    "bm25": "VMEM.RECALL без VEC — один лексический ствол",
    "vmem0": "VMEM.RECALL, все факты свежие",
    "vmemd": "VMEM.RECALL, настоящие даты сессий",
    "vmema": "VMEM.RECALL + ASOF на дату вопроса — затухание в упор",
    "shuffled": "КОНТРОЛЬ numpy: вопрос из чужого разговора",
    "bm25s": "КОНТРОЛЬ через движок: BM25, вопрос чужой",
    "vmems": "КОНТРОЛЬ через движок: VMEM.RECALL, вопрос чужой",
}
CTRL = {"shuffled", "bm25s", "vmems"}


def report(convs, qs, res, cov, unit, stat, embedder="minilm",
           halflife=0, weights=None) -> None:
    npool = sum(len(c.docs) for c in convs) / len(convs)
    print()
    print(f"единица извлечения: {unit} · вопросов: {len(qs)} · документов: "
          f"{sum(len(c.docs) for c in convs)} · кандидатов на вопрос: "
          f"{npool:.0f} · K={K}")
    # ⭐конфигурация печатается ВМЕСТЕ с числом: строка «LoCoMo 57%» без
    # эмбеддера и полураспада ничего не значит — оба меняют её на 18 пунктов
    print(f"эмбеддер: {MODEL_NAME if embedder == 'minilm' else NOMIC_MODEL} · "
          f"полураспад: {str(halflife) + ' дн' if halflife else 'дефолт движка'}"
          f" · веса плеч: {weights if weights else 'по умолчанию (1,1)'}")
    over = sum(1 for q in qs if q.over_k)
    print(f"целей на вопрос: медиана {int(np.median([len(q.targets) for q in qs]))}"
          f", у {over} вопросов целей больше K — полное покрытие им "
          f"недостижимо по построению")
    print()
    print(f"{'рука':10s} {'any@5':>8s} {'cov@5':>8s}   пояснение")
    print("-" * 74)
    for a in ALL_ARMS:
        if a in res:
            v = sum(res[a]) / len(res[a]) * 100
            cv = sum(cov[a]) / len(cov[a]) * 100
            print(f"{a:10s} {v:7.1f}% {cv:7.1f}%   {NAMES[a]}")
    print(f"{'random':10s} {min(1.0, K / npool) * 100:7.1f}% {'':8s}   "
          f"аналитическое K/N")

    shown = [a for a in ALL_ARMS if a in res and a not in CTRL]
    if shown:
        print()
        print(f"{'категория':16s} {'n':>5s}" + "".join(f"{a:>9s}" for a in shown))
        print("-" * (22 + 9 * len(shown)))
        for cat in sorted(CATEGORY_NAME):
            idx = [i for i, q in enumerate(qs) if q.cat == cat]
            if not idx:
                continue
            row = "".join(
                f"{sum(res[a][i] for i in idx) / len(idx) * 100:8.1f}%"
                for a in shown)
            print(f"{CATEGORY_NAME[cat]:16s} {len(idx):5d}{row}")

    # ⭐Цена затухания — ради чего этот корпус и брали.
    # ⚠БАЗА — vmem0, а не vmemd. Дымовой прогон показал, почему это не
    # придирка: vmem0 → vmemd уронило 40.0% → 22.5%, то есть ОСНОВНОЙ удар
    # наносит само датирование, а ASOF добавляет к нему немного. Считать
    # «цену затухания» как vmemd → vmema значило бы отчитаться за 2 пункта
    # вместо 20 — измерение, занижающее собственную находку.
    #   vmem0  все факты свежие  → затухание одинаково для всех, порядок не
    #          трогает: это НОЛЬ, от которого меряется цена;
    #   vmemd  настоящие даты, tEff = сейчас → возраст 2–4 года: сценарий
    #          «спросили в 2026 про разговор 2023»;
    #   vmema  + ASOF на дату вопроса → возраст 0–238 дней: сценарий прода,
    #          где спрашивают по свежим следам. ⭐Вот это и есть цена.
    base = "vmem0" if "vmem0" in res else None
    if base and ("vmemd" in res or "vmema" in res):
        b = sum(res[base]) / len(res[base]) * 100
        print(f"\nцена затухания (база {base} = все факты свежие: {b:.1f}%)")
        for arm, what in (("vmemd", "tEff=сейчас, возраст 2–4 года"),
                          ("vmema", "ASOF на дату вопроса, возраст 0–238 дн")):
            if arm in res:
                v = sum(res[arm]) / len(res[arm]) * 100
                print(f"  → {arm}: {v:.1f}%  = {v - b:+.1f} пункта   ({what})")
        # разбивка по возрасту цели: где именно платим
        ages = []
        for i, q in enumerate(qs):
            youngest = min((q.conv.doc_dates[t] for t in q.targets),
                           default=q.date)
            ages.append((q.date - youngest) / 86400.0)
        arms_age = [a for a in ("vmemd", "vmema") if a in res]
        bins = [(0, 30), (30, 90), (90, 180), (180, 1e9)]
        print(f"{'возраст цели':16s} {'n':>5s} {base:>9s}" +
              "".join(f"{a:>9s}{'Δ':>7s}" for a in arms_age))
        print("-" * (32 + 16 * len(arms_age)))
        for lo, hi in bins:
            idx = [i for i in range(len(qs)) if lo <= ages[i] < hi]
            if not idx:
                continue
            bv = sum(res[base][i] for i in idx) / len(idx) * 100
            row = ""
            for a in arms_age:
                av = sum(res[a][i] for i in idx) / len(idx) * 100
                row += f"{av:8.1f}%{av - bv:+7.1f}"
            label = f"{lo:.0f}–{hi:.0f} дн" if hi < 1e9 else f">{lo:.0f} дн"
            print(f"{label:16s} {len(idx):5d} {bv:8.1f}%{row}")

    print(f"\nразметка: исключено adversarial {stat['adversarial']}, "
          f"без evidence {stat['no_evidence']}, "
          f"выпало без разрешимой цели {stat['dropped']}; "
          f"починено ссылок {stat['repaired']}, "
          f"осталось неразрешимых {stat['unresolved_refs']}")


def print_scope(unit: str) -> None:
    """⭐Граница доказательной силы печатается ВМЕСТЕ С ЧИСЛАМИ, а не живёт в
    заметке: оговорка, о которой надо помнить, повторяет судьбу теста, который
    не запускается."""
    print(f"""
── что отсюда можно цитировать ──────────────────────────────────────
ЭТО RETRIEVAL RECALL, НЕ QA ACCURACY. LLM не вызывался нигде. Публикуемые
   в нише числа «LoCoMo N%» бывают и тем и другим, и разница в разы —
   класть эту строку рядом с чужой QA accuracy НЕЛЬЗЯ.
БЕЗОПАСНО   exact / vsim — независимый оракул (повопросная сверка с точным
            перебором) + отрицательный контроль; векторы округлены до 6
            знаков ДО обеих рук.
С ОГОВОРКОЙ vmem* — контроль через движок есть, полнота выдач и разрешимость
            id проверены, но НЕЗАВИСИМОГО ОРАКУЛА НЕТ: сверить RRF-слияние
            можно только второй реализацией.
ЕДИНИЦА     сейчас {unit}. У систем, хранящих СЖАТЫЕ факты, единица выдачи
            своя, и их recall@5 считается не по этим же документам. Сравнение
            с ними — контекст, а не рейтинг.
─────────────────────────────────────────────────────────────────────""")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--limit", type=int, default=0)
    ap.add_argument("--unit", choices=("turn", "session"), default="turn")
    ap.add_argument("--arms", default=",".join(ALL_ARMS))
    ap.add_argument("--port", type=int, default=6499)
    ap.add_argument("--sweep-halflife", default="",
                    help="через запятую, в днях: 30,90,365,3650")
    ap.add_argument("--embedder", choices=("minilm", "nomic"),
                    default="minilm")
    ap.add_argument("--halflife", type=int, default=0,
                    help="полураспад в днях для рук vmemd/vmema; 0 = дефолт "
                         "движка (365 дней)")
    ap.add_argument("--weights", default="",
                    help="веса плеч RRF через запятую: <текст>,<вектор>. "
                         "Например 1,0 — лексика без векторного голоса")
    a = ap.parse_args()

    arms = [x for x in a.arms.split(",") if x]
    for x in arms:
        if x not in ALL_ARMS:
            sys.exit(f"неизвестная рука: {x} (есть: {', '.join(ALL_ARMS)})")

    convs, qs, stat = load(a.unit, a.limit or None)
    print(f"разговоров: {len(convs)}, документов: "
          f"{sum(len(c.docs) for c in convs)}, вопросов: {len(qs)}")
    dvs, qv = embed_all(convs, qs, tag=f"{a.unit}|limit{a.limit}",
                        embedder=a.embedder)

    if a.sweep_halflife:
        days = [int(x) for x in a.sweep_halflife.split(",") if x]
        print(f"свип полураспада (единица {a.unit}, {len(qs)} вопросов, "
              f"ASOF на дату вопроса):")
        sweep_halflife(convs, qs, dvs, qv, a.port + 7, days)
        return 0

    res: dict[str, list[float]] = {}
    cov: dict[str, list[float]] = {}
    sc: dict[str, list[str]] = {}

    wts = None
    if a.weights:
        parts = [float(x) for x in a.weights.split(",")]
        if len(parts) != 2:
            sys.exit("--weights ждёт ровно два числа: <текст>,<вектор>")
        wts = (parts[0], parts[1])

    def vmem(name, port_off, **kw):
        kw.setdefault("halflife", a.halflife)
        kw.setdefault("weights", wts)
        res[name], cov[name], sc[name] = arm_vmem(convs, qs, dvs, qv,
                                                  a.port + port_off, **kw)

    if "exact" in arms:
        res["exact"], cov["exact"] = arm_exact(convs, qs, dvs, qv)
    if "shuffled" in arms:
        res["shuffled"], cov["shuffled"] = arm_exact(convs, qs, dvs, qv,
                                                     shuffle=True)
    if "vsim" in arms:
        res["vsim"], cov["vsim"] = arm_vsim(convs, qs, dvs, qv, a.port)
    if "bm25" in arms:
        vmem("bm25", 1, dated=False, novec=True)
    if "vmem0" in arms:
        vmem("vmem0", 2, dated=False)
    if "vmemd" in arms:
        vmem("vmemd", 3, dated=True)
    if "vmema" in arms:
        vmem("vmema", 4, dated=True, asof=True)
    if "bm25s" in arms:
        vmem("bm25s", 5, dated=False, novec=True, shuffle=True)
    if "vmems" in arms:
        vmem("vmems", 6, dated=False, shuffle=True)

    report(convs, qs, res, cov, a.unit, stat, a.embedder, a.halflife, wts)

    rc = 0
    # ⭐ОРАКУЛ: движок обязан совпасть с точным перебором ВОПРОС-В-ВОПРОС.
    # Среднее не годится: два разных распределения дают одно среднее.
    if "exact" in res and "vsim" in res:
        diff = [i for i in range(len(qs)) if res["exact"][i] != res["vsim"][i]]
        if diff:
            print(f"\n🚨ОРАКУЛ: vsim расходится с точным перебором на "
                  f"{len(diff)} вопросах: {diff[:10]}")
            rc = 1
        else:
            print("\n✅ОРАКУЛ: vsim совпал с точным перебором повопросно")

    # ⭐УЛИКА ПРОТИВ МОЛЧАЛИВОГО НО-ОПА: датированная рука обязана СЧИТАТЬ иначе.
    for base, other in (("vmem0", "vmemd"), ("vmemd", "vmema")):
        if base in sc and other in sc:
            if sc[base] == sc[other]:
                print(f"\n🚨{base} и {other} дали ПОБАЙТНО те же скоры — "
                      f"параметр не долетел, эта пара ничего не измерила")
                rc = 1
            else:
                nd = sum(1 for x, y in zip(sc[base], sc[other]) if x != y)
                print(f"✅{base} → {other}: скоры разошлись на {nd} из "
                      f"{len(sc[other])} вопросов")

    # ⭐Контроль подмены обязан быть на КАЖДОМ пути, а не только в numpy.
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

    print_scope(a.unit)
    return rc


if __name__ == "__main__":
    raise SystemExit(main())
