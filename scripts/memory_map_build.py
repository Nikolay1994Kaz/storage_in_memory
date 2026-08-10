#!/usr/bin/env python3
# memory_map_build.py — СОБИРАЕТ карту памяти автоматически и ТУТ ЖЕ ЕЁ ПОВЕРЯЕТ
# против рукописного MEMORY.md на тех же десяти вопросах.
#
# ЗАЧЕМ. Замеры 10.08 ([[memory-index-vs-recall]], [[memory-map-measured]])
# показали: рукописный индекс силён не тем, что он оглавление, а тем, что
# каждая строка НЕСЁТ ОТВЕТ; ровно такую строку даёт `description` из
# frontmatter, и он руку догнал (9/10 против 9/10). Значит карту можно собирать
# без руки. Причина спешки не в качестве, а в РАЗМЕРЕ: MEMORY.md занял 96%
# потолка контекста, дальше он просто не поместится.
#
# 🚨ЧТО ЗДЕСЬ ОПРОВЕРГНУТО ПО ХОДУ. Довод «слепое пятно 21%» СНЯТ: все заметки,
# которых нет в MEMORY.md, лежат в ARCHIVE-INDEX.md, сирот 0 из 213. Рука уже
# решила потолок ЯРУСАМИ — и генератор обязан делать то же, только по бюджету,
# а не на глаз. Остались две настоящие опоры: потолок и точность обзора (24%).
#
# 🚨ЧТО ЭТОТ СКРИПТ НЕ ДЕЛАЕТ. Он НЕ ПЕРЕЗАПИСЫВАЕТ MEMORY.md. Результат
# кладётся рядом, под другим именем; замена — отдельное решение человека после
# того, как пороги сойдутся. Два месяца ручной работы не заменяются молча.
#
# ПОРОГИ, ОБЪЯВЛЕННЫЕ ДО ПРОГОНА (те же, что назывались в предложении):
#  · покрытие 100% — каждая заметка каталога представлена ровно одной строкой;
#  · размер не больше нынешнего MEMORY.md;
#  · hit@5 на тех же 10 вопросах НЕ НИЖЕ рукописного (у него 9/10).
# Не сошлось — карта выбрасывается, возвращаемся к руке. Это не фигура речи:
# скрипт печатает вердикт сам и не подгоняет бюджет под результат.
#
# Использование: scripts/memory_map_build.py [порт] [--budget N] [--fresh D]

from __future__ import annotations

import json
import os
import re
import sys
from datetime import datetime, timezone

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import memory_index_vs_recall as mi  # noqa: E402
import multiagent_sim as ms  # noqa: E402


def opt(name: str, default: int) -> int:
    if name in sys.argv:
        return int(sys.argv[sys.argv.index(name) + 1])
    return default


ARGS = [a for a in sys.argv[1:] if not a.startswith("--") and not a.isdigit()
        or (a.isdigit() and sys.argv[sys.argv.index(a) - 1].startswith("--") is False)]
PORT = int(ARGS[0]) if ARGS and ARGS[0].isdigit() else 6397
FRESH_DAYS = opt("--fresh", 14)
OUT = os.path.join(mi.OUTDIR, "MEMORY.generated.md")

MARK_OPEN = "⏭"
MARK_REFUTED = "✗"
REFUTED_RE = re.compile(r"ОПРОВЕРГНУТ|ОТМЕНЕН|ОТМЕНЁН|отклонён")


DATE_SUFFIX = re.compile(r"-\d{8}$")


def title(stem: str) -> str:
    """Заголовок ссылки. 🚨Первая версия ставила имя файла И текстом ссылки, И
    целью — ~150 байт служебной нагрузки на заметку, 32 КБ на каталог, ноль
    информации. Дата не теряется: она осталась в цели ссылки."""
    return DATE_SUFFIX.sub("", stem).replace("-", " ")


def clip(text: str, n: int) -> str:
    """Обрезка по границе слова. 🚨Именно здесь теряется свойство «строка несёт
    ответ» — поэтому длина не выбирается на глаз, а меряется кривой ниже."""
    text = " ".join(text.split())
    if len(text) <= n:
        return text
    cut = text[:n].rsplit(" ", 1)[0]
    return cut + "…"


WIKI_RE = re.compile(r"\[\[([A-Za-z0-9._-]+)\]\]")


def inbound_links(notes: dict) -> dict:
    """⭐СИГНАЛ ВАЖНОСТИ, которого не хватало первой версии. Считается по
    входящим `[[ссылкам]]` между заметками: их ставит путь записи, они уже
    накоплены (202 заметки из 213 имеют хотя бы одну), и человек их не
    расставлял ради индекса — то есть сигнал не подогнан под этот замер."""
    cnt = {s: 0 for s in notes}
    for _s, (text, _ts, _ty, _d) in notes.items():
        for t in set(WIKI_RE.findall(text)):
            if t in cnt:
                cnt[t] += 1
    return cnt


def build(notes: dict, budget: int, fresh_days: int, inbound: dict) -> str:
    """Подбирает УРОВЕНЬ ПОДРОБНОСТИ под бюджет двоичным поиском.
    🚨Первая версия делила «остаток после свежего яруса», а свежий ярус съедал
    бюджет целиком — все бюджеты давали один и тот же файл, и подгонка была
    невидима по выдаче. Теперь под бюджет двигаются ОБА яруса разом."""
    lo, hi = 0.0, 1.0
    best = _render(notes, fresh_days, 0.0, inbound)
    for _ in range(24):
        mid = (lo + hi) / 2
        txt = _render(notes, fresh_days, mid, inbound)
        if len(txt.encode()) <= budget:
            best, lo = txt, mid
        else:
            hi = mid
    return best


def _render(notes: dict, fresh_days: int, detail: float, inbound: dict) -> str:
    now = max(ts for _t, ts, _ty, _d in notes.values())
    fresh_edge = now - fresh_days * 86400
    rows = sorted(((s, ts, d, t) for s, (t, ts, _ty, d) in notes.items()),
                  key=lambda r: -r[1])
    fresh = [r for r in rows if r[1] >= fresh_edge]
    old = [r for r in rows if r[1] < fresh_edge]

    def mark(body: str) -> str:
        m = (MARK_REFUTED if REFUTED_RE.search(body) else "")
        m += (MARK_OPEN if "⏭" in body else "")
        return (m + " ") if m else ""

    head = (f"# Карта памяти — собрана автоматически, {len(rows)} заметок\n"
            f"{MARK_OPEN} открытый хвост · {MARK_REFUTED} опровергнуто или отменено\n\n"
            f"## Свежее — последние {fresh_days} дней ({len(fresh)})\n")

    # ⭐ДВА ЯРУСА, как у руки: одним достаётся предложение, остальным два слова.
    # Кто попадает в верхний — решает НЕ размер бюджета, а важность: свежесть
    # плюс входящие ссылки. Один рычаг `detail` двигает и длину, и ширину яруса.
    fresh_cap = int(70 + 330 * detail)
    top_n = int(10 + 70 * detail)
    ranked = sorted(old, key=lambda r: (-inbound.get(r[0], 0), -r[1]))
    top = ranked[:top_n]
    tail = ranked[top_n:]

    fresh_lines = [f"- {mark(t)}[{title(s)}]({s}.md) — {clip(d, fresh_cap)}\n"
                   for s, ts, d, t in fresh]
    out = [head, *fresh_lines]

    if top:
        out.append(f"\n## Часто цитируемое ({len(top)})\n")
        for s, ts, d, t in sorted(top, key=lambda r: -r[1]):
            out.append(f"- {mark(t)}[{title(s)}]({s}.md) — {clip(d, fresh_cap)}\n")

    # хвост — только имена, пачками по месяцам: ровно приём рукописного индекса
    months = sorted({datetime.fromtimestamp(ts, timezone.utc).strftime("%m.%Y")
                     for _s, ts, _d, _t in tail}, reverse=True)
    if tail:
        out.append(f"\n## Остальное — именами ({len(tail)})\n")
    for m in months:
        grp = [r for r in tail
               if datetime.fromtimestamp(r[1], timezone.utc).strftime("%m.%Y") == m]
        names = " · ".join(f"{mark(t)}[{title(s)}]({s}.md)" for s, ts, d, t in grp)
        out.append(f"- **{m}**: {names}\n")
    return "".join(out)


def verify(paths: dict, port: int) -> dict:
    """Те же 10 вопросов, что и в первом замере, — против всех сгенерированных
    карт и против рукописной, в ОДНОМ прогоне на одном сервере: иначе кривая
    «размер против качества» смешала бы разные условия."""
    corp = {name: mi.build_index_corpus(p) for name, p in paths.items()}
    corp["hand"] = mi.build_index_corpus()
    parent_of = {f[0]: f[2] for c in corp.values() for f in c}
    size_of = {f[0]: len(f[1]) for c in corp.values() for f in c}

    srv = ms.Server(port)
    srv.start()
    res = {}
    try:
        c = ms.Resp(port)
        for scope, facts in corp.items():
            for i, (fid, text, _p, _ts) in enumerate(facts):
                c.call("VMEM.REMEMBER", scope, "TEXT", text,
                       "ID", f"{scope}~{i}", "SOURCE", "map")
            # ⚠id перештампован под scope: build_index_corpus выдаёт одинаковые
            # `idx~<строка>` для обоих файлов, и без этого они бы столкнулись
            parent_of.update({f"{scope}~{i}": facts[i][2] for i in range(len(facts))})
            size_of.update({f"{scope}~{i}": len(facts[i][1]) for i in range(len(facts))})
        for scope in corp:
            hits, chars = 0, 0
            for _qid, query, truth in mi.QUESTIONS:
                flat = c.call("VMEM.RECALL", scope, "5", query) or []
                ids = [flat[i] for i in range(0, len(flat), 3)]
                got = set().union(*[parent_of[i] for i in ids]) if ids else set()
                hits += bool(got & truth)
                chars += sum(size_of[i] for i in ids)
            res[scope] = {"hit_at_5": hits, "chars": round(chars / len(mi.QUESTIONS))}
    finally:
        srv.cleanup()
    return res


def main() -> None:
    if not os.path.exists(ms.BIN):
        raise SystemExit(f"нет бинаря {ms.BIN}")
    os.makedirs(mi.OUTDIR, exist_ok=True)

    stems = sorted(f[:-3] for f in os.listdir(mi.MEMDIR)
                   if f.endswith(".md") and f not in
                   ("MEMORY.md", "ARCHIVE-INDEX.md"))
    notes = {s: mi.read_note(s) for s in stems}
    hand_bytes = os.path.getsize(os.path.join(mi.MEMDIR, "MEMORY.md"))

    inbound = inbound_links(notes)
    top = sorted(inbound.values(), reverse=True)[:5]
    budgets = [10000, 14000, hand_bytes, 26000, 40000]
    print(f"рукописный MEMORY.md: {hand_bytes} байт, заметок {len(notes)}")
    print(f"входящих ссылок: с ≥1 — {sum(1 for v in inbound.values() if v)} "
          f"из {len(notes)}, топ {top}\n")
    curve, paths = {}, {}
    for b in budgets:
        text = build(notes, b, FRESH_DAYS, inbound)
        name = f"g{b}"
        p = os.path.join(mi.OUTDIR, f"MEMORY.generated.{b}.md")
        open(p, "w", encoding="utf-8").write(text)
        paths[name] = p
        linked = set(mi.LINK_RE.findall(text))
        lines = [l for l in text.splitlines() if l.startswith("- ")]
        curve[name] = {"budget": b, "bytes": len(text.encode()),
                       "cover": round(len(linked & set(notes)) / len(notes), 3),
                       "avg_line": round(sum(len(l) for l in lines) / len(lines))}

    res = verify(paths, PORT)

    print(f"{'бюджет':>8s} {'вышло':>8s} {'покрытие':>9s} {'ср.строка':>10s} {'hit@5':>7s}")
    for name, v in curve.items():
        print(f"{v['budget']:8d} {v['bytes']:8d} {v['cover']:8.0%} "
              f"{v['avg_line']:10d} {res[name]['hit_at_5']:6d}/10")
    print(f"{'рука':>8s} {hand_bytes:8d} {'100%':>9s} {'—':>10s} "
          f"{res['hand']['hit_at_5']:6d}/10")

    # ── к вердикту идёт бюджет, равный нынешнему размеру: порог объявлен так
    main_name = f"g{hand_bytes}"
    text = open(paths[main_name], encoding="utf-8").read()
    open(OUT, "w", encoding="utf-8").write(text)
    got_bytes = curve[main_name]["bytes"]
    cover = curve[main_name]["cover"]

    json.dump({"curve": curve, "verify": res, "hand_bytes": hand_bytes},
              open(os.path.join(mi.OUTDIR, "memory_map_build.json"), "w",
                   encoding="utf-8"), ensure_ascii=False, indent=1)

    ok_cover = cover >= 1.0
    ok_size = got_bytes <= hand_bytes
    ok_hit = res[main_name]["hit_at_5"] >= res["hand"]["hit_at_5"]
    print(f"\n── вердикт по объявленным порогам ──")
    print(f"  покрытие 100%          : {'✅' if ok_cover else '❌'} {cover:.1%}")
    print(f"  размер ≤ рукописного   : {'✅' if ok_size else '❌'} "
          f"{got_bytes} против {hand_bytes}")
    print(f"  hit@5 не ниже руки     : {'✅' if ok_hit else '❌'} "
          f"{res[main_name]['hit_at_5']} против {res['hand']['hit_at_5']}")
    print(f"\n{'ГОДИТСЯ' if (ok_cover and ok_size and ok_hit) else '🚨НЕ ГОДИТСЯ — по объявленному правилу карта выбрасывается'}")
    print(f"\nкарта: {OUT}  (MEMORY.md НЕ тронут)")


if __name__ == "__main__":
    main()
