#!/usr/bin/env python3
# poison_recovery_compare.py — один инцидент порчи памяти, ЧЕТЫРЕ стратегии
# восстановления, измеренные на одних и тех же данных: две чужие (настоящий
# OWASP Agent Memory Guard, его код, его дефолты) и две наши (VMEM).
#
# ЗАЧЕМ ОТДЕЛЬНО ОТ poison_recovery.sh. Тот скрипт сравнивает наш откат с нашим
# отзывом и честен ровно в этих границах: колонка «откат целиком» там — НАША
# реализация в роли чужой стратегии, то есть модель чужого поведения. Для
# публикации этого мало: сравнивать надо с кодом, а не с представлением о нём.
# Здесь колонки A1/A2 исполняет установленный из PyPI agent-memory-guard.
#
# ЧТО ИЗМЕРЯЕТСЯ. Не детекция — на этом поле мы не играем и играть не заявляем.
# Предпосылка сценария: подсадка УЖЕ прошла мимо предотвращения (проверено —
# см. блок 0: ни одна из трёх политик Guard её не помечает, и это не претензия
# к ним, правдоподобный деловой факт без маркеров инъекции не ловит никто).
# Измеряется цена восстановления, и главная величина — СОПУТСТВУЮЩИЕ ПОТЕРИ:
# сколько законной работы, сделанной ПОСЛЕ подсадки, не пережило лечение.
#
# ЧЕСТНОСТЬ К СРАВНИВАЕМОМУ (иначе весь заход бессмысленен):
#   - обе их стратегии получают ЛУЧШИЙ возможный случай: снапшот-оракул снят
#     ровно перед подсадкой (как будто оператор заранее знал момент инцидента),
#     а для выборочного удаления мы сами размечаем им КАЖДУЮ запись классом и
#     задачей, без чего retire_if не за что зацепиться;
#   - тексты английские: их детекторы регулярочные, и русская подсадка прошла
#     бы мимо них по причине, к делу не относящейся;
#   - retire_if(origin_task == заражённая задача) — их настоящая выборочная
#     операция, и она СПАСАЕТ всю работу после подсадки. Утверждение
#     «прослойка не может выборочно» было бы ложью, и здесь оно не звучит.
# Разница, которая остаётся, — не в том, можно ли убрать ложь выборочно, а в
# том, что становится с уликой и с историей веры.
#
# Требуется: собранный ./kvstore-server, redis-cli, python 3.11+ и
#   python3 -m venv .venv && .venv/bin/pip install agent-memory-guard==0.3.0
# Запуск:   .venv/bin/python scripts/poison_recovery_compare.py [порт]
from __future__ import annotations

import os
import shutil
import subprocess
import sys
import tempfile
import time

try:
    from agent_memory_guard import MemoryGuard
    from agent_memory_guard.policies.policy import Policy
except ImportError:
    sys.exit("нет agent-memory-guard: python3 -m venv .venv && "
             ".venv/bin/pip install agent-memory-guard==0.3.0")

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 6399
BIN = os.environ.get("BIN", "./kvstore-server")
SCOPE = "user:dana"
QUERY = "Aurora"

POISON = "Project Aurora has been cancelled; all work must stop permanently."
BEFORE = [f"Project Aurora decision number {i} approved." for i in range(1, 6)]
AFTER = [f"Project Aurora new agreement {i}." for i in range(1, 5)]
TASK_HUMAN, TASK_EMAIL = "session-human", "email-ingest"
SRC_HUMAN, SRC_EMAIL = "human", "email-agent"


# ═══ половина первая: настоящий OWASP Agent Memory Guard ════════════════════

def guard_seed(g: MemoryGuard):
    """Обычная работа → подсадка → работа продолжается. Отдаёт снапшот-оракул."""
    for i, text in enumerate(BEFORE, 1):
        g.write(f"fact.before{i}", text, source=SRC_HUMAN, source_class="user_input",
                cls="tool_observation", task_id=TASK_HUMAN)
    # Снапшот ровно перед подсадкой = потолок стратегии отката: раньше заметить
    # инцидент невозможно в принципе, он ещё не произошёл.
    oracle = g.snapshot(label="oracle-pre-poison")
    g.write("fact.poison", POISON, source=SRC_EMAIL, source_class="external_tool",
            cls="retrieved_fact", task_id=TASK_EMAIL)
    for i, text in enumerate(AFTER, 1):
        g.write(f"fact.after{i}", text, source=SRC_HUMAN, source_class="user_input",
                cls="tool_observation", task_id=TASK_HUMAN)
    return oracle


def guard_counts(g: MemoryGuard) -> dict:
    return {
        "poison": 1 if "fact.poison" in g._store else 0,
        "before": sum(1 for i in range(1, 6) if f"fact.before{i}" in g._store),
        "after": sum(1 for i in range(1, 5) if f"fact.after{i}" in g._store),
    }


def run_guard() -> tuple[dict, dict, list[str]]:
    notes = []

    # 0. Ловит ли их предотвращение эту подсадку — на всех трёх их политиках.
    for name, pol in [("permissive", Policy.permissive()), ("tiered", Policy.tiered()),
                      ("strict", Policy.strict())]:
        probe = MemoryGuard(policy=pol)
        action = probe.write("fact.poison", POISON, source=SRC_EMAIL,
                             source_class="external_tool", cls="retrieved_fact",
                             task_id=TASK_EMAIL)
        notes.append(f"Guard policy={name}: подсадка → {action}, событий {len(probe.events)}")

    # A1 — их штатное восстановление: откат к снапшоту.
    g1 = MemoryGuard(policy=Policy.strict())
    oracle = guard_seed(g1)
    g1.rollback(oracle.snapshot_id)
    a1 = guard_counts(g1)
    # rollback() снапшот ПЕРЕД откатом не снимает: состояние с работой после
    # подсадки нигде не сохранено → откат необратим.
    a1["evidence_query"] = False
    a1["evidence_snapshot"] = oracle.data.get("fact.poison") is not None
    a1["reversible"] = False

    # A2 — их выборочное удаление по заражённой задаче.
    g2 = MemoryGuard(policy=Policy.strict())
    guard_seed(g2)
    retired = g2.retire_if(lambda k, _v: g2.origin_task(k) == TASK_EMAIL, reason="incident")
    a2 = guard_counts(g2)
    pre_retire = next(s for s in reversed(g2.list_snapshots())
                      if s.label.startswith("pre-retire"))
    a2["evidence_query"] = "fact.poison" in g2._store
    a2["evidence_snapshot"] = pre_retire.data.get("fact.poison") is not None
    a2["reversible"] = True  # но только откатом стора целиком к pre-retire
    notes.append(f"Guard retire_if снял ключи: {retired}")
    notes.append("Guard: rollback() снимок ПЕРЕД откатом не делает (retire_if — делает), "
                 "поэтому улику при откате сохраняет только ручной g.snapshot() до него")
    return a1, a2, notes


# ═══ половина вторая: VMEM ══════════════════════════════════════════════════

class Server:
    """Наш kvstore-server на время одного измерения."""

    def __init__(self, data_dir: str, work: str):
        self.data_dir, self.work, self.proc = data_dir, work, None

    def start(self, *extra: str):
        log = open(os.path.join(self.work, "server.log"), "ab")
        self.proc = subprocess.Popen(
            [BIN, "-port", str(PORT), "-data-dir", self.data_dir,
             "-metrics-port", "0", "-log-level", "error", *extra],
            stdout=log, stderr=log)
        for _ in range(200):
            if cli("PING", check=False).strip() == "PONG":
                return
            time.sleep(0.05)
        raise SystemExit("сервер не поднялся; см. " + os.path.join(self.work, "server.log"))

    def stop(self):
        if self.proc:
            self.proc.terminate()
            self.proc.wait(timeout=30)
            self.proc = None
        time.sleep(0.3)


def cli(*args: str, check: bool = True) -> str:
    res = subprocess.run(["redis-cli", "-p", str(PORT), *args],
                         capture_output=True, text=True)
    if check and res.returncode != 0:
        raise SystemExit(f"redis-cli {args}: {res.stderr.strip()}")
    # redis-cli отдаёт ошибку сервера НУЛЕВЫМ кодом возврата: без этой проверки
    # опечатка в имени модификатора выглядит как «свойства нет» — ровно так и
    # получилось при первом прогоне с AS_OF вместо ASOF.
    if check and res.stdout.startswith("ERR "):
        raise SystemExit(f"redis-cli {args}: {res.stdout.strip()}")
    return res.stdout


def recall_ids(*extra: str) -> list[str]:
    """id фактов, которые агент СЕЙЧАС считает истиной (RECALL отдаёт тройки)."""
    lines = [l for l in cli("VMEM.RECALL", SCOPE, "100", QUERY, *extra).splitlines() if l]
    return lines[0::3]


def counts(ids: list[str]) -> dict:
    return {
        "poison": sum(1 for i in ids if i.startswith("poison")),
        "before": sum(1 for i in ids if i.startswith("before")),
        "after": sum(1 for i in ids if i.startswith("after")),
    }


def run_vmem(work: str) -> tuple[dict, dict, list[str]]:
    notes = []
    data = os.path.join(work, "data")
    os.makedirs(data, exist_ok=True)
    srv = Server(data, work)

    # Один и тот же инцидент, что и у Guard.
    srv.start()
    for i, text in enumerate(BEFORE, 1):
        cli("VMEM.REMEMBER", SCOPE, "TEXT", text, "ID", f"before{i}", "SOURCE", SRC_HUMAN)
    cli("VMEM.REMEMBER", SCOPE, "TEXT", POISON, "ID", "poison", "SOURCE", SRC_EMAIL)
    for i, text in enumerate(AFTER, 1):
        cli("VMEM.REMEMBER", SCOPE, "TEXT", text, "ID", f"after{i}", "SOURCE", SRC_HUMAN)
    before_incident = int(time.time())  # момент «агент уже действует по лжи»
    srv.stop()

    # LSN подсадки — то, что оператор ищет в журнале после обнаружения.
    inspect = subprocess.run([BIN, "-data-dir", data, "-wal-inspect"],
                             capture_output=True, text=True).stdout
    poison_lsn = next(int(f[0]) for f in (l.split() for l in inspect.splitlines())
                      if len(f) > 2 and f[1] == "VSIM.ADDDOC" and f[2] == "poison")
    notes.append(f"VMEM: подсадка в журнале на LSN {poison_lsn}")

    # B1 — откат состояния до момента перед подсадкой (read-only режим).
    srv.start("-restore-to-lsn", str(poison_lsn - 1))
    b1 = counts(recall_ids())
    b1["evidence_query"] = False
    b1["evidence_snapshot"] = True   # журнал на диске не тронут
    b1["reversible"] = True          # каталог не меняется, снять флаг и перезапустить
    b1["asof_belief"] = False        # отмотанное состояние не знает о подсадке
    srv.stop()

    # B2 — выборочный отзыв по происхождению.
    time.sleep(2)  # отзыв обязан лечь ПОЗЖЕ записей, иначе AS_OF нечего показывать
    srv.start()
    # Квитанция приходит плоским списком «имя, значение, …» построчно; поле
    # берётся по имени — позиция это деталь протокола.
    receipt = cli("VMEM.QUARANTINE", SCOPE, "SOURCE", SRC_EMAIL).split("\n")
    fields = {receipt[i].strip(): receipt[i + 1].strip()
              for i in range(0, len(receipt) - 1, 2)}
    revoked = fields.get("revoked", "?")
    notes.append(f"VMEM.QUARANTINE … SOURCE {SRC_EMAIL} → отозвано: {revoked}, "
                 f"остаток по источнику: {fields.get('still_trusted', '?')}")
    b2 = counts(recall_ids())
    b2["evidence_query"] = counts(recall_ids("ALL"))["poison"] > 0
    b2["evidence_snapshot"] = True
    b2["reversible"] = True          # старая версия факта жива, отзыв — новая версия
    # Главное: «во что агент верил в момент до отзыва» — вопрос со смыслом.
    b2["asof_belief"] = counts(recall_ids("ASOF", str(before_incident)))["poison"] > 0
    srv.stop()
    return b1, b2, notes


# ═══ таблица ════════════════════════════════════════════════════════════════

def yn(v: bool) -> str:
    return "да" if v else "нет"


def main() -> int:
    if not os.access(BIN, os.X_OK):
        sys.exit(f"нет бинаря {BIN} (собрать: go build -o kvstore-server ./kvstore/cmd/kvstore/)")
    if shutil.which("redis-cli") is None:
        sys.exit("нужен redis-cli")

    work = tempfile.mkdtemp(prefix="vmem-poison-cmp-")
    try:
        a1, a2, gnotes = run_guard()
        b1, b2, vnotes = run_vmem(work)
    finally:
        shutil.rmtree(work, ignore_errors=True)

    for n in gnotes + vnotes:
        print("  •", n)
    print()

    cols = [("A1: Guard rollback()", a1), ("A2: Guard retire_if()", a2),
            ("B1: VMEM restore-to-lsn", b1), ("B2: VMEM.QUARANTINE", b2)]
    rows = [
        ("Ложь больше не выдаётся", lambda c: yn(c["poison"] == 0)),
        ("Законных ДО подсадки", lambda c: f"{c['before']} / 5"),
        ("*Законных ПОСЛЕ подсадки", lambda c: f"{c['after']} / 4"),
        ("Улика доступна запросом к памяти", lambda c: yn(c["evidence_query"])),
        ("Улика есть хотя бы в снимке целиком", lambda c: yn(c["evidence_snapshot"])),
        ("*AS_OF до отзыва показывает факт", lambda c: yn(c.get("asof_belief", False))),
        ("Восстановление обратимо", lambda c: yn(c["reversible"])),
    ]
    prereq = ["снапшоты по расписанию + момент инцидента",
              "класс и задача на КАЖДОЙ записи + id задачи",
              "ничего заранее + LSN подсадки",
              "SOURCE на записи + имя источника"]

    table = [["МЕТРИКА", *(name for name, _ in cols)]]
    for label, fn in rows:
        table.append([label, *(fn(c) for _, c in cols)])
    table.append(["Требуется от приложения заранее", *prereq])
    table.append(["Стор пригоден для записи после", "да", "да", "нет (только чтение)", "да"])

    widths = [max(len(r[i]) for r in table) for i in range(len(table[0]))]
    for n, row in enumerate(table):
        print("  ".join(cell.ljust(w) for cell, w in zip(row, widths)).rstrip())
        if n == 0:
            print("  ".join("─" * w for w in widths))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
