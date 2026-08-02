#!/usr/bin/env python3
# derived_from_probe.py — ЗАМЕР ДО КОДА: чего стоит транзитивный отзыв.
#
# ЗАЧЕМ. Кандидат (в) из scripts/revocation_limits.py: связь «выведено из»
# штампует АДАПТЕР, а не агент, — тем же принципом, что и source. Адаптер видит,
# что он выдал агенту в recall и что тот записал следом, поэтому связь не
# зависит от добросовестности агента. Это закрывает L4 (производный вывод)
# принципиально: отзыв канала дотягивается до пересказа, у которого собственное
# происхождение уже чистое.
#
# 🚨ЧТО ЗДЕСЬ ПРОВЕРЯЕТСЯ, А ЧТО НЕТ. Работоспособность связи проверять нечего:
# она детерминирована, в отличие от похожести текстов, на которой погорел
# кандидат (а). Проверяется ЦЕНА. Адаптер не знает, что именно из показанного
# агент использовал для вывода, — он знает лишь, что показал. Значит предком
# станет и то, что агент увидел мимоходом, а транзитивный отзыв унесёт вывод,
# сделанный вовсе не из отравленного факта.
#
# ⭐В корпусе это ровно и стоит: у research-agent ДЕСЯТЬ законных выводов
# (res1..res10) и в варианте L4 один выведенный из подсадки (res11). Вопрос
# замера: сколько законных выводов уносит отзыв канала ради того, чтобы унести
# один отравленный. Порядок записи в корпусе перемешан (RNG.shuffle), поэтому
# часть законных выводов пишется уже ПОСЛЕ подсадок и видит их в выдаче —
# это не подстройка сценария, а то, как оно и бывает.
#
# ПОЛИТИКИ ПРЕДКОВ (что именно адаптер запишет в derived_from):
#   session — всё, что агент видел за свои обращения к памяти до этой записи;
#   last    — выдача последнего recall перед записью;
#   top1    — только первый результат последнего recall.
#
# ПОРОГИ, ОБЪЯВЛЕННЫЕ ДО ПРОГОНА:
#   · политика НЕ РЕШАЕТ ЗАДАЧУ, если не уносит res11 — тогда L4 остаётся
#     открытым и городить связь незачем;
#   · политика ГОДИТСЯ, если уносит res11 и не более 2 законных выводов:
#     цена сопоставима с пользой;
#   · политика НЕГОДНА, если уносит 6 и более законных выводов из десяти —
#     лечение дороже болезни, и это тот же провал, что у кандидата (а),
#     только с другой стороны.
#
# Использование: scripts/derived_from_probe.py [порт]

from __future__ import annotations

import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import multiagent_sim as ms  # noqa: E402
import revocation_limits as rl  # noqa: E402

ARGS = [a for a in sys.argv[1:] if not a.startswith("--")]
PORT = int(ARGS[0]) if ARGS else 6392
SCOPE = ms.SCOPE
POISONS = ms.POISONS
DERIVED = rl.DERIVED          # res11 — вывод из подсадки (вариант L4)
LEGIT_DERIVED = tuple(f"res{i}" for i in range(1, 11))
RECALL_K = 5                  # столько адаптер отдаёт по умолчанию

POLICIES = ("session", "last", "top1")


def fill_watching_reads(c: ms.Resp, plan: list[ms.Fact], broad: bool) -> dict:
    """Заполнение памяти с наблюдением за тем, ЧТО АДАПТЕР ПОКАЗАЛ БЫ агенту.

    Перед записью производного факта делается recall его текстом: так и
    выглядит работа агента — сначала посмотрел в память, потом записал вывод.
    Возвращает, для каждого производного факта, предков по каждой политике.
    Это моделирование адаптера, а не движка: движку остаётся принять готовый
    список, и именно поэтому цену можно померить, ничего не реализовав."""
    seen_by_source: dict[str, list[str]] = {}
    ancestors: dict[str, dict[str, list[str]]] = {}
    written: set[str] = set()
    # 🚨КОНТРОЛЬ ДИАГНОЗА. Нулевая цена значит одно из двух: политика точна ИЛИ
    # сценарий не создал риска. Различить их можно только так — считая, у
    # скольких законных выводов подсадка вообще МОГЛА попасть в выдачу (была
    # уже записана) и у скольких реально попала.
    exposed: list[str] = []
    caught: list[str] = []
    for f in plan:
        if f.kind == "derived":
            # ⭐Две модели чтения, и разница между ними — половина ответа.
            # Узкая: агент ищет ровно то, о чём собирается писать. Широкая:
            # «что вообще известно про проект», чем агенты и пользуются чаще,
            # — и тогда отравленный факт попадает в выдачу всякий раз.
            query = ms.SYMPTOM_QUERY if broad else f.text
            got = ms.recall_ids(c, SCOPE, RECALL_K, query)
            if f.fid != DERIVED and written & set(POISONS):
                exposed.append(f.fid)
                if set(got) & set(POISONS):
                    caught.append(f.fid)
            seen = seen_by_source.setdefault(f.source, [])
            ancestors[f.fid] = {
                "session": sorted(set(seen) | set(got)),
                "last": list(got),
                "top1": got[:1],
            }
            seen.extend(got)
        args = ["VMEM.REMEMBER", f.scope, "TEXT", f.text, "ID", f.fid,
                "SOURCE", f.source, "VALIDFROM", f.valid_from]
        if f.supersedes:
            args += ["SUPERSEDES", f.supersedes]
        c.call(*args)
        written.add(f.fid)
    return {"ancestors": ancestors, "exposed": exposed, "caught": caught}


def transitive_hit(ancestors: dict[str, dict[str, list[str]]], policy: str,
                   revoked: set[str]) -> list[str]:
    """Кого унесёт транзитивный отзыв при политике «отозвать вывод, если отозван
    ЛЮБОЙ его предок». Замыкание считается до неподвижной точки: вывод из
    вывода — тот же случай, и обрывать цепь на первом шаге значило бы лечить
    ровно на один уровень глубины."""
    hit: set[str] = set()
    changed = True
    while changed:
        changed = False
        for fid, byp in ancestors.items():
            if fid in hit:
                continue
            if set(byp[policy]) & (revoked | hit):
                hit.add(fid)
                changed = True
    return sorted(hit)


def run(broad: bool) -> dict:
    """Один прогон на свежем сервере и свежем корпусе. Свежесть обязательна:
    во втором режиме память уже была бы вылечена первым, и мерился бы не режим
    чтения, а последствия чужого лечения."""
    now = int(time.time())
    plan = rl.fresh_plan(now)
    note = rl.variant_derived(plan, now)
    srv = ms.Server(PORT)
    try:
        srv.start()
        c = ms.Resp(PORT)
        watched = fill_watching_reads(c, plan, broad)
        before = ms.recall_ids(c, SCOPE, 10)
        if rl.OBSERVED not in before:
            raise SystemExit("инцидент не обнаружен — сценарий не воспроизвёлся")
        channel = ms.phase_localize(c, rl.OBSERVED)
        receipt = ms.phase_revoke(c, channel)
        revoked = {f.fid for f in plan if f.source == channel}
        c.close()
    finally:
        srv.cleanup()
    watched.update(note=note, receipt=receipt, revoked=revoked)
    return watched


def report(title: str, w: dict) -> None:
    ancestors, revoked = w["ancestors"], w["revoked"]
    print(f"── {title} " + "─" * max(0, 60 - len(title)))
    print(f"   отозвано {w['receipt']['revoked']}, производных под наблюдением "
          f"{len(ancestors)} (законных {len(LEGIT_DERIVED)}, из подсадки 1)")
    hdr = (f"   {'политика':9} {'предков ср.':>12} {'унесено':>8} {'res11':>6} "
           f"{'законных':>9} {'вердикт':>11}")
    print(hdr)
    for pol in POLICIES:
        sizes = [len(a[pol]) for a in ancestors.values()]
        avg = sum(sizes) / len(sizes) if sizes else 0
        hit = transitive_hit(ancestors, pol, revoked)
        got_poison = DERIVED in hit
        legit_lost = len([f for f in hit if f in LEGIT_DERIVED])
        if not got_poison:
            v = "НЕ РЕШАЕТ"
        elif legit_lost <= 2:
            v = "ГОДИТСЯ"
        elif legit_lost >= 6:
            v = "НЕГОДНА"
        else:
            v = "серая зона"
        print(f"   {pol:9} {avg:12.1f} {len(hit):8} {'да' if got_poison else 'НЕТ':>6} "
              f"{legit_lost:9} {v:>11}")
    exposed, caught = w["exposed"], w["caught"]
    print(f"   контроль диагноза: риск был у {len(exposed)} из {len(LEGIT_DERIVED)}, "
          f"подсадку увидели {len(caught)}")
    print()


def main() -> int:
    if not os.path.exists(rl.BIN):
        print(f"нет бинаря {rl.BIN}")
        return 1
    narrow = run(broad=False)
    broad = run(broad=True)
    print(f"порт {PORT}, зерно {ms.SEED}, вариант: {narrow['note']}")
    print("предки — то, что адаптер ПОКАЗАЛ агенту перед записью вывода\n")
    report("УЗКОЕ чтение: агент ищет то, о чём пишет", narrow)
    report("ШИРОКОЕ чтение: агент спрашивает «что известно про проект»", broad)

    if not broad["caught"]:
        print("⚠ШИРОКИЙ РЕЖИМ НЕ СОЗДАЛ РИСКА — замер не проверил то, ради чего")
        print("  затевался: подсадка не попала в выдачу даже по общему запросу.")
        return 1
    print("⭐Читать так: цена транзитивного отзыва зависит не от движка, а от")
    print("  того, НАСКОЛЬКО ШИРОКО агент читает память перед записью. Узкое")
    print("  чтение делает связь почти бесплатной; широкое — тащит отравленный")
    print("  факт в предки всему, что агент записал следом.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
