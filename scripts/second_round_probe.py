#!/usr/bin/env python3
# second_round_probe.py — ЗАМЕР ДО КОДА: чего стоит подсказка второго круга.
#
# ЗАЧЕМ. Кандидат (а) из scripts/revocation_limits.py звучит дёшево: после
# отзыва спросить память ТЕКСТАМИ отозванных фактов, и то, что похоже, но
# пришло другим каналом, предъявить оператору как кандидатов на второй круг.
# Замер 31.07 показал, что это работает в L4 и L5 — там, где уцелевшая ложь
# содержательно связана с отозванной.
#
# 🚨НО ТОТ ЗАМЕР ОТВЕЧАЛ НЕ НА ТОТ ВОПРОС, НА КОТОРЫЙ ПРИДЁТСЯ ОТВЕЧАТЬ ПРОДУКТУ.
# `second_round_hint` фильтрует найденное по `looking_for` — списку лжи,
# известному харнессу из плана корпуса. То есть мерилось «попадает ли ИСКОМАЯ
# ложь в топ-K по тексту отозванного». Движок этого знания не имеет и иметь не
# может: он не отличает ложь от правды, это и есть граница, которую мы всюду
# называем вслух. Значит оператору он предъявит ВСЁ похожее, вперемешку с
# законным.
#
# Отсюда вопрос, который обязан быть измерен ДО кода: сколько кандидатов
# получится и какая их доля осмысленна. И главный риск, которого прежний замер
# не мог увидеть в принципе:
#
#   ⭐ЛОЖНАЯ ТРЕВОГА НА ЧИСТОМ ИСХОДЕ. В L0 и L2 лжи после лечения не остаётся
#   вовсе. Если подсказка и там выдаст список «посмотрите ещё вот на это», она
#   кричит всегда — а подсказка, которая срабатывает независимо от того, есть
#   ли что искать, не несёт информации. Это то же самое, что метрика, читающая
#   чужой флаг: выглядит как измерение, измерением не являясь.
#
# ПОРОГИ, ОБЪЯВЛЕННЫЕ ДО ПРОГОНА (иначе замер подгонится под результат):
#   · подсказка ГОДИТСЯ, если на L4/L5 она содержит уцелевшую ложь И при этом
#     на L0/L2 молчит либо выдаёт заметно меньше;
#   · подсказка БЕСПОЛЕЗНА, если число кандидатов на чистом исходе того же
#     порядка, что на грязном: оператор не сможет отличить одно от другого;
#   · подсказка ВРЕДНА, если кандидатов столько, что их просмотр дороже
#     ручного разбора канала (ориентир — 12 законных писем канала).
#
# Использование: scripts/second_round_probe.py [порт]

from __future__ import annotations

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import multiagent_sim as ms  # noqa: E402
import revocation_limits as rl  # noqa: E402

ARGS = [a for a in sys.argv[1:] if not a.startswith("--")]
PORT = int(ARGS[0]) if ARGS else 6393
SCOPE = ms.SCOPE
POISONS, MAILS, OUTSIDE = ms.POISONS, ms.MAILS, ms.OUTSIDE
OBSERVED = rl.OBSERVED
DERIVED = rl.DERIVED

# K — глубина поиска по тексту каждого отозванного факта. Две точки, потому что
# чувствительность к K и есть половина ответа: если полезное появляется только
# при большом K, вместе с ним придёт и весь шум.
K_VALUES = (1, 2, 3, 5, 10)


def probe(name: str, note: str, plan: list[ms.Fact], use_since: bool,
          lies: tuple[str, ...]) -> dict:
    """Один случай: заполнить, локализовать, отозвать, затем спросить память
    текстами отозванных — БЕЗ фильтра по плану, ровно как это сделал бы движок."""
    texts = {f.fid: f.text for f in plan}
    srv = ms.Server(PORT)
    try:
        srv.start()
        c = ms.Resp(PORT)
        ms.phase_fill(c, plan)

        before = ms.recall_ids(c, SCOPE, 10)
        if OBSERVED not in before:
            return {"name": name, "note": note, "undetected": True}

        channel = ms.phase_localize(c, OBSERVED)
        since = None
        if use_since:
            since = next(f.valid_from for f in plan if f.fid == OBSERVED)
        receipt = ms.phase_revoke(c, channel, since=since)

        # Что реально отозвано — по плану, тем же предикатом, что и приговор.
        revoked_ids = [f.fid for f in plan if f.source == channel
                       and (since is None or f.valid_from >= since)]
        # Что уцелело из лжи: это знает харнесс, и только для СУДЕЙСТВА, а не
        # для отбора кандидатов.
        survived = [f for f in lies if ms.visible(c, SCOPE, f, texts[f])]

        out = {"name": name, "note": note, "channel": channel,
               "revoked": receipt["revoked"],
               "still_trusted": receipt["still_trusted"],
               "survived": survived}
        for k in K_VALUES:
            # ⭐Ровно то, что смог бы посчитать движок: поиск текстами
            # отозванных, всё найденное — кандидат. Отозванные из выдачи
            # выпадают сами (RECALL их не отдаёт), поэтому фильтровать по
            # источнику не нужно и не нужно знать, кто из них лгал.
            cands: set[str] = set()
            for fid in revoked_ids:
                if fid in texts:
                    cands.update(ms.recall_ids(c, SCOPE, k, texts[fid]))
            cands.discard(OBSERVED)
            useful = sorted(cands & set(survived))
            out[k] = {"total": len(cands), "useful": useful,
                      "noise": len(cands) - len(useful)}
        # ⭐Отделяется ли сигнал ПОЗИЦИЕЙ. Если уцелевшая ложь стоит первой по
        # запросу текстом отозванного, а шум ниже, подсказку спасает узкий K.
        # Если она вперемешку — не спасает ничто, кроме знания, которого у
        # движка нет.
        best = {}
        for fid in revoked_ids:
            if fid not in texts:
                continue
            for pos, got in enumerate(ms.recall_ids(c, SCOPE, 10, texts[fid]), 1):
                if got == OBSERVED:
                    continue
                if got not in best or pos < best[got]:
                    best[got] = pos
        out["rank_useful"] = {f: best[f] for f in survived if f in best}
        out["rank_noise"] = sorted(p for f, p in best.items() if f not in survived)
        c.close()
        return out
    finally:
        srv.cleanup()


def main() -> int:
    if not os.path.exists(rl.BIN):
        print(f"нет бинаря {rl.BIN}")
        return 1
    now = int(__import__("time").time())
    cases = []

    p = rl.fresh_plan(now)
    cases.append(probe("L0 один канал, без окна", "лжи не остаётся — контроль на ложную тревогу",
                       p, False, POISONS))

    p = rl.fresh_plan(now)
    note = rl.variant_two_channels(p)
    cases.append(probe("L1 два канала", note, p, False, POISONS))

    p = rl.fresh_plan(now)
    cases.append(probe("L2 окно SINCE", "лжи не остаётся — контроль на ложную тревогу",
                       p, True, POISONS))

    p = rl.fresh_plan(now)
    note = rl.variant_spread_dates(p, now)
    cases.append(probe("L3 окно + разные даты", note, p, True, POISONS))

    p = rl.fresh_plan(now)
    note = rl.variant_derived(p, now)
    cases.append(probe("L4 производный вывод", note, p, False, (*POISONS, DERIVED)))

    p = rl.fresh_plan(now)
    note = rl.variant_two_channels_same_claim(p)
    cases.append(probe("L5 два канала, одно утверждение", note, p, False, POISONS))

    print(f"порт {PORT}, зерно {ms.SEED}, корпус тот же, что в основном прогоне")
    print("кандидаты считаются БЕЗ знания плана — ровно как смог бы движок\n")
    hdr = f"{'случай':34} {'отозв':>5} {'ост':>4} {'выжило лжи':>10}"
    for k in K_VALUES:
        hdr += f" {'K=' + str(k) + ' всего':>10} {'полезных':>8} {'шум':>5}"
    print(hdr)
    print("─" * len(hdr))
    for c in cases:
        if c.get("undetected"):
            print(f"{c['name']:34} инцидент не обнаружен")
            continue
        row = (f"{c['name']:34} {c['revoked']:5} {c['still_trusted']:4} "
               f"{len(c['survived']):10}")
        for k in K_VALUES:
            row += f" {c[k]['total']:10} {len(c[k]['useful']):8} {c[k]['noise']:5}"
        print(row)
    print()
    print("ПОЗИЦИЯ: на каком месте выдачи стоит уцелевшая ложь и где начинается шум")
    for c in cases:
        if c.get("undetected"):
            continue
        ru = c["rank_useful"]
        noise = c["rank_noise"]
        # Сколько шума стоит ВЫШЕ лучшего полезного — цена того, чтобы увидеть
        # полезное вообще. Если она велика, узкий K сигнал не спасает.
        if ru:
            top = min(ru.values())
            above = sum(1 for p in noise if p <= top)
            print(f"  {c['name']}: полезное {ru}, выше него шума {above} "
                  f"(всего шума {len(noise)})")
        else:
            print(f"  {c['name']}: полезного нет; шума {len(noise)}, "
                  f"первый на позиции {noise[0] if noise else '—'}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
