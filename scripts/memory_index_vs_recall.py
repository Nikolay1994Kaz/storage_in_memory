#!/usr/bin/env python3
# memory_index_vs_recall.py — ЗАМЕР ДО ИМПОРТА: кто лучше отвечает на реальный
# вопрос начала сессии — рукописный индекс MEMORY.md или VMEM.RECALL по
# заметкам, и если recall, то КАКОЙ НАРЕЗКОЙ.
#
# ЗАЧЕМ. Файловая память Николая — 212 заметок и рукописный MEMORY.md, который
# уже занимает 84% потолка контекста и не упоминает каждую пятую заметку.
# Соблазн — залить всё в vmem и ходить туда recall'ом. Но recall отдаёт ПЯТЬ
# ТОЧЕЧНЫХ ФАКТОВ, а индекс даёт КАРТУ: заменив одно другим, теряешь обзорность.
# Прежде чем импортировать 212 файлов, надо узнать, проигрывает ли recall
# индексу на тех вопросах, ради которых память вообще открывают.
#
# 🚨ЧТО ЗДЕСЬ ПРОВЕРЯЕТСЯ, А ЧТО НЕТ.
#  · Проверяется ПОИСК: доводит ли выдача до нужной заметки и какой ценой.
#  · НЕ проверяется качество ответа: попадание считается по ЗАМЕТКЕ-РОДИТЕЛЮ,
#    а не по тому, что модель из неё вычитает. Смешивать эти шкалы уже стоило
#    нам путаницы «74.8% против 66.9%» (vmem-locomo-gap-and-priorities).
#  · НЕ проверяется, что важнее — карта или факт. Это вопрос применения;
#    замер лишь даёт цену обоих.
#
# ПОРОГИ, ОБЪЯВЛЕННЫЕ ДО ПРОГОНА (условие отмены записано в заметке 09.08):
#  · затея ОТМЕНЯЕТСЯ до импорта, если НИ ОДНА нарезка не догоняет индекс
#    по hit@5 — тогда рукописный MEMORY.md останется единственным входом;
#  · затея ПОДТВЕРЖДЕНА, если хотя бы одна нарезка берёт индекс по hit@5
#    ИЛИ равна ему при существенно меньшей цене чтения;
#  · развилка «чем резать» СЧИТАЕТСЯ НЕРЕШЁННОЙ этим замером, если лучшая и
#    худшая нарезки различаются меньше чем на 1 попадание из 10: 20 заметок
#    и 10 вопросов — слишком мелкая сетка, чтобы читать такую разницу.
#
# ГРАБЛИ, КОТОРЫЕ ЗДЕСЬ УЖЕ ОБОЙДЕНЫ:
#  · говорим по RESP сокетом (клиент из multiagent_sim), а не через redis-cli:
#    тот отдаёт ошибку сервера НУЛЕВЫМ кодом возврата;
#  · сверяем попадание по ID факта → заметке-родителю, НИКОГДА по подстроке:
#    BM25 хранит стемы, проверка вхождения слова была бы зелёной всегда;
#  · пишем БЕЗ VEC (ладдер шаг 0, детерминированный плейсхолдер) — прогон
#    воспроизводим побайтно, и меряется ровно лексическое плечо;
#  · поднимаем ОТДЕЛЬНЫЙ сервер во временном каталоге: личная память на :6381
#    не должна получить тысячу тестовых фактов в свой WAL и цепь аудита;
#  · дата факта берётся из frontmatter (`modified`), а не из mtime файла:
#    mtime у всех заметок съезжает при любом копировании каталога.
#
# Использование: scripts/memory_index_vs_recall.py [порт]
# Требуется собранный ./kvstore-server (go build -o kvstore-server ./kvstore/cmd/kvstore/).

from __future__ import annotations

import json
import os
import re
import sys
import time
from datetime import datetime, timezone

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import multiagent_sim as ms  # noqa: E402

ARGS = [a for a in sys.argv[1:] if not a.startswith("--")]
PORT = int(ARGS[0]) if ARGS else 6399
ALL_NOTES = "--all" in sys.argv   # весь каталог вместо отобранных двадцати
MEMDIR = os.path.expanduser(
    "~/.claude/projects/-home-nikolay-storage-in-memory/memory")
OUTDIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                      "scratch")
K = 5                      # столько отдаёт MCP-адаптер по умолчанию
HALFLIFE_OFF = 10 ** 6 * 86400   # затухание практически выключено

# ─────────────────────────── корпус: 20 заметок ─────────────────────────────
# 10 несут ответы на вопросы ниже, 10 — соседи по теме (отвлекающие). Соседи
# выбраны НЕ случайно: это те, что лексически похожи на ответ и в реальной
# сессии всплывают рядом. Случайный набор занизил бы трудность.

NOTES = [
    # ── несут ответы
    "own-task-memory-system-20260809",
    "work-status-revision-20260806",
    "vmem-release-v040-20260806",
    "vmem-real-data-verdict-20260804",
    "vmem-decay-default-and-weights-lever-20260804",
    "vmem-longmemeval-number-20260803",
    "ydb-contribution-entry-20260808",
    "job-doors-map-20260808",
    "vmem-locomo-gap-and-priorities-20260803",
    "medical-memory-pivot-20260722",
    "agent-environment-pitfalls-20260806",
    # ── отвлекающие соседи
    "market-pain-scan-20260809",
    "startup-direction-four-doors-20260808",
    "vmem-locomo-measured-20260804",
    "vmem-embedder-prefixes-20260804",
    "vmem-first-run-walkthrough-done-20260806",
    "nikolay-self-reframe-20260807",
    "niche-search-three-refutations-20260808",
    "vmem-differentiator-priorities-20260802",
    "working-with-nikolay",
]

# ────────────────────── 10 реальных вопросов начала сессии ───────────────────
# Формулировки — как их задают живьём, а не как их удобно искать. Q1 нарочно
# почти не имеет лексического пересечения ни с одной заметкой: именно этот
# вопрос открывает сессию чаще всех прочих, и провал на нём — результат замера,
# а не дефект набора.

QUESTIONS = [
    ("Q1", "привет на чём мы остановились в прошлый раз какая точка возврата",
     {"own-task-memory-system-20260809", "work-status-revision-20260806"}),
    ("Q2", "что с релизом v0.4.0 можно ли откатиться на старую версию",
     {"vmem-release-v040-20260806"}),
    ("Q3", "дописывать ли VEC в адаптер что показали реальные данные",
     {"vmem-real-data-verdict-20260804"}),
    ("Q4", "какой дефолт затухания выбран и почему именно такой",
     {"vmem-decay-default-and-weights-lever-20260804"}),
    ("Q5", "какая у нас цифра качества поиска на LongMemEval",
     {"vmem-longmemeval-number-20260803"}),
    ("Q6", "почему индекс памяти протухает и что с ним делать",
     {"own-task-memory-system-20260809"}),
    ("Q7", "куда подавать резюме и какой вход в компанию через код",
     {"ydb-contribution-entry-20260808", "job-doors-map-20260808"}),
    ("Q8", "почему наши проценты нельзя сравнивать между собой разные шкалы",
     {"vmem-locomo-gap-and-priorities-20260803"}),
    ("Q9", "что мы решили про медицинское направление",
     {"medical-memory-pivot-20260722"}),
    ("Q10", "какие грабли среды у агента что врёт при проверке",
     {"agent-environment-pitfalls-20260806"}),
]

# ─────────────────────────────── нарезки ─────────────────────────────────────

FM_RE = re.compile(r"\A---\n(.*?)\n---\n", re.S)
MOD_RE = re.compile(r"^\s*modified:\s*(\S+)", re.M)
DESC_RE = re.compile(r"^description:\s*(.*?)(?=^\w+:)", re.M | re.S)
TYPE_RE = re.compile(r"^\s*type:\s*(\S+)", re.M)
LINK_RE = re.compile(r"\]\(([A-Za-z0-9._-]+)\.md\)")


def read_note(stem: str) -> tuple[str, int, str, str]:
    """Текст заметки целиком, дата (unix), тип и description из frontmatter."""
    path = os.path.join(MEMDIR, stem + ".md")
    if not os.path.exists(path):
        raise SystemExit(f"нет заметки {path} — корпус описан неверно, прогон отменён")
    text = open(path, encoding="utf-8").read()
    fm = FM_RE.match(text)
    body = fm.group(1) if fm else ""
    m = MOD_RE.search(body)
    if m:
        ts = int(datetime.fromisoformat(
            m.group(1).replace("Z", "+00:00")).timestamp())
    else:
        ts = int(os.path.getmtime(path))
    t = TYPE_RE.search(body)
    d = DESC_RE.search(body + "\nEND:")
    desc = " ".join((d.group(1) if d else "").split()).strip('"')
    return text, ts, (t.group(1) if t else "project"), desc


def cut_whole(text: str) -> list[str]:
    return [text]


def cut_sections(text: str) -> list[str]:
    """По заголовкам `## `. Всё до первого заголовка (frontmatter + вводный
    абзац) — отдельный кусок: там лежит рукописный description, и надо видеть,
    не он ли вытягивает всю выдачу."""
    out, cur = [], []
    for line in text.splitlines():
        if line.startswith("## "):
            if cur:
                out.append("\n".join(cur).strip())
            cur = [line]
        else:
            cur.append(line)
    if cur:
        out.append("\n".join(cur).strip())
    return [c for c in out if len(c) >= 40]


def cut_paragraphs(text: str) -> list[str]:
    return [p.strip() for p in re.split(r"\n\s*\n", text) if len(p.strip()) >= 40]


CUTS = {"whole": cut_whole, "sect": cut_sections, "para": cut_paragraphs}


def build_index_corpus(path: str | None = None) -> list[tuple[str, str, set[str], int]]:
    """MEMORY.md построчно: каждый пункт списка — отдельный факт. Родителями
    считаются ВСЕ заметки, на которые пункт ссылается, — это честное отражение
    того, что одна строка индекса действительно ведёт в несколько файлов
    (и заодно фора индексу, о которой надо помнить при чтении таблицы)."""
    path = path or os.path.join(MEMDIR, "MEMORY.md")
    facts = []
    for i, line in enumerate(open(path, encoding="utf-8").read().splitlines()):
        line = line.strip()
        if not line.startswith("-") or len(line) < 40:
            continue
        parents = {s for s in LINK_RE.findall(line)}
        facts.append((f"idx~{i}", line, parents, 0))
    return facts


# ────────────────────────────── прогон ───────────────────────────────────────

def ingest(c: ms.Resp, scope: str,
           facts: list[tuple[str, str, set[str], int]]) -> None:
    for fid, text, _parents, ts in facts:
        args = ["VMEM.REMEMBER", scope, "TEXT", text, "ID", fid,
                "SOURCE", "notes"]
        if ts:
            args += ["VALIDFROM", str(ts)]
        c.call(*args)


def recall(c: ms.Resp, scope: str, query: str, halflife: int | None):
    args = ["VMEM.RECALL", scope, str(K), query]
    if halflife is not None:
        args += ["HALFLIFE", str(halflife)]
    flat = c.call(*args) or []
    out = []
    for i in range(0, len(flat), 3):
        out.append((flat[i], float(flat[i + 1]), flat[i + 2] or ""))
    return out


def main() -> None:
    if not os.path.exists(ms.BIN):
        raise SystemExit(f"нет бинаря {ms.BIN}: "
                         "go build -o kvstore-server ./kvstore/cmd/kvstore/")
    os.makedirs(OUTDIR, exist_ok=True)

    # ── план корпусов строится ЦЕЛИКОМ до записи: прогон воспроизводим
    if ALL_NOTES:
        # 🚨На двадцати заметках выдача из пяти — четверть корпуса, и целая
        # заметка выигрывает по построению. Полный каталог убирает эту фору;
        # взамен появляется своя цена: часть «промахов» окажется законной
        # альтернативой, которую эталон не знает, — поэтому промахи полного
        # прогона читаются глазами, а не только по счётчику.
        stems = sorted(f[:-3] for f in os.listdir(MEMDIR)
                       if f.endswith(".md") and f not in
                       ("MEMORY.md", "ARCHIVE-INDEX.md"))
    else:
        stems = NOTES
    notes = {stem: read_note(stem) for stem in stems}
    corpora: dict[str, list[tuple[str, str, set[str], int]]] = {}
    for scope, cut in CUTS.items():
        facts = []
        for stem, (text, ts, _typ, _d) in notes.items():
            for j, chunk in enumerate(cut(text)):
                facts.append((f"{scope}~{stem}~{j}", chunk, {stem}, ts))
        corpora[scope] = facts
    corpora["idx"] = build_index_corpus()
    # ⭐desc — АВТОМАТИЧЕСКИЙ аналог строки индекса: имя файла + однострочный
    # `description` из frontmatter, который пишется при сохранении заметки.
    # Если он догоняет рукописный idx, карту можно собирать без руки.
    corpora["desc"] = [(f"desc~{stem}~0", f"{stem}\n{desc}", {stem}, ts)
                       for stem, (_t, ts, _ty, desc) in notes.items() if desc]

    parent_of = {fid: parents for facts in corpora.values()
                 for fid, _t, parents, _ts in facts}
    size_of = {fid: len(t) for facts in corpora.values()
               for fid, t, _p, _ts in facts}

    srv = ms.Server(PORT)
    srv.start()
    results: dict = {"corpora": {}, "runs": {}}
    try:
        c = ms.Resp(PORT)
        for scope, facts in corpora.items():
            t0 = time.time()
            ingest(c, scope, facts)
            results["corpora"][scope] = {
                "facts": len(facts),
                "avg_chars": round(sum(len(t) for _f, t, _p, _s in facts) / len(facts)),
                "ingest_sec": round(time.time() - t0, 1),
            }
            print(f"  {scope:5s}: {len(facts):5d} фактов, "
                  f"средний {results['corpora'][scope]['avg_chars']:6d} симв., "
                  f"запись {results['corpora'][scope]['ingest_sec']:5.1f} с")

        # decay-7d — не режим применения, а ПРОВЕРКА ЖИВОСТИ РУЧКИ: если
        # семидневный период полураспада не сдвинул ни одной выдачи, значит
        # HALFLIFE не доехал до движка и колонка decay-off ничего не мерила.
        for mode, hl in (("decay-on", None), ("decay-off", HALFLIFE_OFF),
                         ("decay-7d", 7 * 86400)):
            per_scope = {}
            for scope in corpora:
                rows = []
                for qid, query, truth in QUESTIONS:
                    hits = recall(c, scope, query, hl)
                    parents = [parent_of[h[0]] for h in hits]
                    rank = next((i + 1 for i, p in enumerate(parents)
                                 if p & truth), 0)
                    rows.append({
                        "q": qid,
                        "rank": rank,
                        "hit": rank > 0,
                        "chars": sum(size_of[h[0]] for h in hits),
                        "notes_touched": len(set().union(*parents)) if parents else 0,
                        "top": [{"id": h[0], "score": round(h[1], 3)} for h in hits],
                    })
                per_scope[scope] = {
                    "hit_at_5": sum(r["hit"] for r in rows),
                    "mrr": round(sum(1 / r["rank"] for r in rows if r["rank"]) / len(rows), 3),
                    "avg_chars": round(sum(r["chars"] for r in rows) / len(rows)),
                    "avg_notes": round(sum(r["notes_touched"] for r in rows) / len(rows), 1),
                    "rows": rows,
                }
            results["runs"][mode] = per_scope
    finally:
        srv.cleanup()

    idx_bytes = os.path.getsize(os.path.join(MEMDIR, "MEMORY.md"))
    results["memory_md_bytes"] = idx_bytes
    out = os.path.join(OUTDIR, "memory_index_vs_recall.json")
    json.dump(results, open(out, "w", encoding="utf-8"),
              ensure_ascii=False, indent=1)

    for mode in results["runs"]:
        print(f"\n── {mode} ── (K={K}, попадание = нужная заметка среди 5)")
        print(f"{'нарезка':8s} {'hit@5':>6s} {'MRR':>6s} "
              f"{'симв./вопрос':>13s} {'заметок':>8s}")
        for scope, r in results["runs"][mode].items():
            print(f"{scope:8s} {r['hit_at_5']:5d}/10 {r['mrr']:6.3f} "
                  f"{r['avg_chars']:13d} {r['avg_notes']:8.1f}")
        print(f"{'MEMORY.md':8s} — читается целиком: {idx_bytes} байт")

    print("\n── промахи (decay-on) ──")
    for scope, r in results["runs"]["decay-on"].items():
        miss = [row["q"] for row in r["rows"] if not row["hit"]]
        print(f"  {scope:5s}: {', '.join(miss) if miss else 'нет'}")

    # ── живость HALFLIFE: сравниваем СКОРЫ, а не только попадания
    on = results["runs"]["decay-on"]
    hard = results["runs"]["decay-7d"]
    moved = sum(1 for scope in on for a, b in
                zip(on[scope]["rows"], hard[scope]["rows"])
                if [x["score"] for x in a["top"]] != [x["score"] for x in b["top"]])
    total = sum(len(on[scope]["rows"]) for scope in on)
    print(f"\nживость HALFLIFE: скоры изменились в {moved} выдачах из {total} "
          f"→ ручка {'ЖИВА' if moved else '🚨НЕ ДОЕХАЛА, колонки затухания недействительны'}")
    print(f"\nсырьё: {out}")


if __name__ == "__main__":
    main()
