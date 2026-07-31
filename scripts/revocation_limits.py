#!/usr/bin/env python3
# revocation_limits.py — где отзыв по происхождению НЕ ДОСТАЁТ.
#
# ЗАЧЕМ. `recovery_negative_control.py` мерит чужие стратегии и показывает, что
# наша лучше. Такой замер обязан иметь пару: тот же корпус, тот же инцидент, но
# вопрос обратный — при каких условиях НАША стратегия оставляет ложь в памяти.
# Дыру у соседа нашли замером; было бы нечестно не поискать её у себя тем же
# инструментом.
#
# ОБЩАЯ СТРУКТУРА ВСЕХ ДЫР. Лечение не может быть полнее локализации: человек
# видит ОДНО проявление и выводит из него ПРИЗНАК, по которому убирает
# остальное. Дыра — везде, где инцидент шире признака. Откат берёт признаком
# ВРЕМЯ ЗАПИСИ (с инцидентом не связано ничем). Отзыв берёт ПРОИСХОЖДЕНИЕ
# (связано причинно), поэтому его дыры — не «признак не тот», а «признак
# покрывает не всё». Здесь измеряется, насколько не всё.
#
# ⭐ГЛАВНАЯ МЕТРИКА — не «сколько фактов осталось», а «ЛЖЁТ ЛИ ПАМЯТЬ ПОСЛЕ
# ЛЕЧЕНИЯ НА ТОТ ЖЕ ВОПРОС». Оператор задал вопрос, увидел ложь, вылечил и
# спрашивает снова. Если в выдаче снова ложь — лечение не состоялось, сколько
# бы фактов ни было отозвано.
#
# Случаи (каждый на своём сервере, корпус один и тот же):
#   L0 контроль — как в основном прогоне: три подсадки из одного канала
#   L1 два канала — подсадки разведены; локализация даёт только один
#   L2 окно SINCE — то же, но окно вычислено из ЗАМЕЧЕННОГО факта
#   L3 окно + датирование задним числом — атакующий знает про окно
#   L4 производный вывод — агент пересказал ложь от СВОЕГО имени
#
# Использование: scripts/revocation_limits.py [порт]

from __future__ import annotations

import os
import random
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import multiagent_sim as ms  # noqa: E402

ARGS = [a for a in sys.argv[1:] if not a.startswith("--")]
PORT = int(ARGS[0]) if ARGS else 6394
BIN = os.environ.get("BIN", "./kvstore-server")
SCOPE, DAY = ms.SCOPE, ms.DAY
POISONS, MAILS, OUTSIDE = ms.POISONS, ms.MAILS, ms.OUTSIDE
OBSERVED = "poison1"
DERIVED = "res11"


def fresh_plan(now: int) -> list[ms.Fact]:
    """Один и тот же корпус для каждого случая: зерно переустанавливается, а
    объекты создаются заново, поэтому правки одного случая не текут в другой."""
    ms.RNG = random.Random(ms.SEED)
    return ms.build_corpus(now)


# ───────────────────────── варианты инцидента ───────────────────────────────

def variant_two_channels(plan: list[ms.Fact]) -> str:
    """Компрометация задела ДВА канала. poison2/poison3 переезжают в crm-sync —
    в живой канал с 40 законными фактами, а не в пустой: отзыв второго канала
    обязан стоить столько же, сколько стоил бы в жизни."""
    for f in plan:
        if f.fid in ("poison2", "poison3"):
            f.source = "crm-sync"
    return "подсадки 2 и 3 переехали в crm-sync (40 законных фактов)"


def variant_spread_dates(plan: list[ms.Fact], now: int) -> str:
    """⭐Атака на окно, не требующая знания о нём. Окно вычисляется из
    ЗАМЕЧЕННОЙ подсадки, поэтому достаточно, чтобы у остальных время ДЕЙСТВИЯ
    было раньше — а это естественно и без злого умысла: «бюджет заморожен с
    начала месяца» датируется началом месяца. Даты остаются свежими, факты
    продолжают выдаваться; из окна они уходят, из канала — нет."""
    for f in plan:
        if f.fid in ("poison2", "poison3"):
            f.valid_from = now - 20 * DAY
    return "подсадки 2 и 3 датированы 20 днями назад (замеченная — 3 дня)"


def variant_backdated(plan: list[ms.Fact], now: int) -> str:
    """Грубое датирование задним числом: раньше всего, что канал писал законно.
    Прямое следствие измеренного 30.07 — время ДЕЙСТВИЯ объявляет пишущий."""
    for f in plan:
        if f.fid in POISONS:
            f.valid_from = now - 100 * DAY
    return "все подсадки датированы 100 днями назад (раньше всех писем канала)"


def variant_two_channels_same_claim(plan: list[ms.Fact]) -> str:
    """⭐Худший из исходов: два канала утверждают ОДНО И ТО ЖЕ. Так и выглядит
    подкрепление лжи — повторить её из второго места, чтобы она перестала быть
    единственным свидетельством. Отзыв канала-источника снимает одну копию, и
    симптомный запрос, которым человек нашёл ложь, продолжает её выдавать: с
    точки зрения оператора лечение прошло, а память лжёт по-прежнему."""
    for f in plan:
        if f.fid == "poison2":
            f.source = "crm-sync"
            f.text = ("проект Аврора: работы по проекту остановлены "
                      "до особого распоряжения, статус закрыт")
    return "второй канал утверждает то же самое (подкрепление лжи)"


def variant_derived(plan: list[ms.Fact], now: int) -> str:
    """⭐Агент прочитал подсадку и записал вывод ОТ СВОЕГО ИМЕНИ. Происхождение
    у вывода чистое: research-agent никто не компрометировал, он честно
    пересказал то, что нашёл в памяти. Отзыв канала-источника до этого факта не
    дотягивается — связи «выведено из» в модели нет вовсе."""
    plan.append(ms.Fact(
        SCOPE, DERIVED,
        "проект Аврора: вывод — планирование следующего квартала не требуется, "
        "работы по проекту прекращены",
        "research-agent", now - 1 * DAY, "derived"))
    return "research-agent записал вывод из подсадки от своего имени"


# ───────────────────────────── прогон случая ────────────────────────────────

def symptom_lies(c: ms.Resp, texts: dict[str, str], lies: tuple[str, ...]) -> list[str]:
    """⭐Главный вопрос: ЛЖЁТ ЛИ ПАМЯТЬ на тот же вопрос после лечения.
    Спрашивается ровно тем запросом, которым человек нашёл ложь в первый раз."""
    ids = ms.recall_ids(c, SCOPE, 10)
    return [i for i in ids if i in lies]


def second_round_hint(c: ms.Resp, texts: dict[str, str], revoked_ids: list[str],
                      looking_for: tuple[str, ...]) -> list[str]:
    """Проверка предлагаемого лечения, а не только дыры.

    Идея: после отзыва спросить память ТЕКСТАМИ ОТОЗВАННЫХ фактов — то, что на
    них похоже, но пришло из других каналов, и есть кандидаты на второй круг
    расследования. Если это работает, дыра закрывается подсказкой в ответе
    команды, а не новой моделью данных."""
    found: set[str] = set()
    for fid in revoked_ids:
        if fid not in texts:
            continue
        for got in ms.recall_ids(c, SCOPE, 5, texts[fid]):
            if got in looking_for:
                found.add(got)
    return sorted(found)


def run_case(name: str, note: str, plan: list[ms.Fact], texts: dict[str, str],
             use_since: bool, lies: tuple[str, ...], expect: dict | None = None) -> dict:
    """expect — измеренная сегодня граница, закреплённая как ожидание. Без неё
    скрипт был бы отчётом: через месяц никто не отличит «поведение то же» от
    «поведение изменилось, и никто не заметил»."""
    srv = ms.Server(PORT)
    try:
        srv.start()
        c = ms.Resp(PORT)
        ms.phase_fill(c, plan)

        # Симптом: попадает ли ложь в топ-10 ДО лечения. Если нет — лечение не
        # начнётся вовсе: оператор об инциденте не узнает. Это не поломка
        # сценария, а самостоятельный исход, и он требует своего замера —
        # достаётся ли непойманная ложь ПРИЦЕЛЬНЫМ запросом (то есть лежит ли
        # она в памяти, готовая всплыть на более узком вопросе).
        before_ids = ms.recall_ids(c, SCOPE, 10)
        if OBSERVED not in before_ids:
            aimed = [f for f in lies if ms.visible(c, SCOPE, f, texts[f])]
            return {"name": name, "note": note, "undetected": True, "aimed": aimed}

        channel = ms.phase_localize(c, OBSERVED)  # имя вычисляется, не подсказано
        args = ["VMEM.QUARANTINE", SCOPE, "SOURCE", channel]
        window = ""
        if use_since:
            # ⭐Окно вычисляется из ЗАМЕЧЕННОГО факта — «с того момента, каким
            # датирована найденная ложь». Оператор знает ровно это и ничего
            # больше; подставить сюда знание о корпусе было бы подгонкой.
            since = next(f.valid_from for f in plan if f.fid == OBSERVED)
            args += ["SINCE", since]
            window = f" SINCE {since} (valid_from замеченной подсадки)"
        revoked = int(c.call(*args))

        # Что именно отозвано — нужно и для подсказки второго круга.
        revoked_ids = [f.fid for f in plan if f.source == channel
                       and (not use_since or f.valid_from >= since)]

        left = [f for f in lies if ms.visible(c, SCOPE, f, texts[f])]
        lost = [f for f in MAILS if not ms.visible(c, SCOPE, f, texts[f])]
        outside = sum(1 for f in OUTSIDE if f in texts and ms.visible(c, SCOPE, f, texts[f]))
        still = symptom_lies(c, texts, lies)
        hint = second_round_hint(c, texts, revoked_ids, tuple(left))

        got = {"name": name, "note": note, "channel": channel + window,
               "revoked": revoked, "left": left, "lost": len(lost),
               "outside": outside, "symptom": still, "hint": hint}
        drift = {}
        for k, want in (expect or {}).items():
            cur = got[k]
            cur = len(cur) if isinstance(cur, list) else cur
            if cur != want:
                drift[k] = (want, cur)
        got["drift"] = drift
        return got
    finally:
        srv.cleanup()
        time.sleep(0.4)


def main() -> int:
    if not os.access(BIN, os.X_OK):
        print(f"нет бинаря {BIN}")
        return 2
    now = int(time.time())
    cases = []

    p = fresh_plan(now)
    cases.append(run_case("L0 контроль: один канал, без окна", "как в основном прогоне",
                          p, {f.fid: f.text for f in p}, False, POISONS,
                          {"revoked": 15, "left": 0, "lost": 12, "symptom": 0}))

    p = fresh_plan(now)
    note = variant_two_channels(p)
    cases.append(run_case("L1 два канала", note, p, {f.fid: f.text for f in p},
                          False, POISONS,
                          {"revoked": 13, "left": 2, "lost": 12, "hint": 0}))

    p = fresh_plan(now)
    cases.append(run_case("L2 окно SINCE", "то же, что L0, но отзыв ограничен окном",
                          p, {f.fid: f.text for f in p}, True, POISONS,
                          {"revoked": 3, "left": 0, "lost": 0}))

    p = fresh_plan(now)
    note = variant_spread_dates(p, now)
    cases.append(run_case("L3 окно + разные даты действия", note, p,
                          {f.fid: f.text for f in p}, True, POISONS,
                          {"revoked": 1, "left": 2, "lost": 0}))

    p = fresh_plan(now)
    note = variant_backdated(p, now)
    cases.append(run_case("L3б грубое датирование задним числом", note, p,
                          {f.fid: f.text for f in p}, True, POISONS))

    p = fresh_plan(now)
    note = variant_derived(p, now)
    cases.append(run_case("L4 производный вывод", note, p, {f.fid: f.text for f in p},
                          False, (*POISONS, DERIVED),
                          {"revoked": 15, "left": 1, "hint": 1}))

    p = fresh_plan(now)
    note = variant_two_channels_same_claim(p)
    cases.append(run_case("L5 два канала, одно утверждение", note, p,
                          {f.fid: f.text for f in p}, False, POISONS,
                          {"revoked": 14, "left": 1, "symptom": 1, "hint": 1}))

    print(f"порт {PORT}, зерно {ms.SEED}, корпус тот же, что в основном прогоне")
    print(f"замеченная подсадка везде одна: {OBSERVED}; канал вычисляется через EXPLAIN\n")
    for c in cases:
        print("─" * 78)
        print(f"{c['name']}  —  {c['note']}")
        if c.get("undetected"):
            print("  ⚠ИНЦИДЕНТ НЕ ОБНАРУЖЕН: замеченной подсадки нет в топ-10, "
                  "лечение не начинается")
            print(f"  но в памяти она лежит и достаётся прицельным запросом: "
                  f"{len(c['aimed'])} из {len(POISONS)} "
                  f"({', '.join(c['aimed']) if c['aimed'] else 'ни одной'})")
            continue
        print(f"  локализация → {c['channel']}, отозвано {c['revoked']}")
        print(f"  ⭐ложь осталась в памяти (прицельный запрос): {len(c['left'])} "
              f"({', '.join(c['left']) if c['left'] else 'ничего'})")
        print(f"  память лжёт на ТОТ ЖЕ вопрос (топ-10 симптома): "
              f"{'ДА — ' + ', '.join(c['symptom']) if c['symptom'] else 'нет'}")
        print(f"  ⚠цена: потеряно законных в канале {c['lost']} из 12; "
              f"вне канала цело {c['outside']}")
        if c["left"]:
            print(f"  второй круг (поиск текстами отозванных) нашёл: "
                  f"{', '.join(c['hint']) if c['hint'] else '⚠НИЧЕГО'}")
        if c.get("drift"):
            for k, (want, cur) in c["drift"].items():
                print(f"  ⚠ГРАНИЦА СДВИНУЛАСЬ: {k} было {want}, стало {cur}")
    print("─" * 78)
    moved = [c["name"] for c in cases if c.get("drift")]
    if moved:
        print(f"⚠поведение изменилось с момента замера: {', '.join(moved)}")
        return 1
    print("границы те же, что были измерены 31.07")
    return 0


if __name__ == "__main__":
    sys.exit(main())
