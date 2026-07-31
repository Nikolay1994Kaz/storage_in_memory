#!/usr/bin/env python3
# recovery_negative_control.py — отрицательный контроль к многоагентному прогону.
#
# ЗАЧЕМ. `multiagent_sim.py` меряет ОДНУ стратегию восстановления (отзыв по
# происхождению) и показывает, что она отрабатывает. Но зелёное табло одной
# стратегии не говорит, что она чем-то лучше: пока рядом нет колонки «а как
# было бы без неё», все четырнадцать строк — самоописание. Здесь тот же корпус
# и тот же инцидент прогоняются через ПЯТЬ стратегий, и три из них не имеют к
# движку никакого отношения.
#
# ПОЧЕМУ ФАЙЛОВЫХ КОЛОНОК ТРИ, А НЕ ОДНА. Файловая память (markdown-файл, каким
# пользуется агент без движка) лечится тремя разными способами, и они дают
# РАЗНЫЕ ответы. Мерить только один — подтасовка в любую сторону: взять правку
# руками значит выбрать соломенное чучело, взять разбор поля source значит
# спрятать главную находку сценария. Поэтому меряются все три, и файлу всюду
# даётся ЛУЧШИЙ возможный случай (см. ниже).
#
# ЛУЧШИЙ СЛУЧАЙ ДЛЯ ФАЙЛА — иначе сравнение бессмысленно:
#   · снимок файла берётся ПОСЛЕ КАЖДОЙ записи (как если бы каталог памяти был
#     под git с коммитом на каждый факт), поэтому откат возможен в любую точку;
#   · момент инцидента считается известным точно — оператор откатывает ровно к
#     состоянию перед замеченной подсадкой, ни на шаг раньше;
#   · обновление факта (SUPERSEDES) выполняется заменой строки НА МЕСТЕ, а не
#     дописыванием второй: файл не остаётся с двумя противоречивыми записями;
#   · источник разбирается из ПОЛЯ строки, а не ищется подстрокой, поэтому
#     колонка «по источнику» не промахивается по чужим фактам;
#   · агент видит ВЕСЬ файл целиком — никакой поиск ничего не теряет.
# Разница, которая после этого остаётся, — не про удобство, а про то, что
# происходит с уликой, с историей веры и с соседями, которых инцидент не касался.
#
# ЧТО ИЗМЕРЯЕТСЯ. Не детекция: подсадка уже прошла мимо предотвращения, это
# предпосылка сценария. Меряется цена лечения — сколько законной работы не
# пережило восстановление и что осталось от улики.
#
# Использование: scripts/recovery_negative_control.py [порт]
# Требуется собранный ./kvstore-server.

from __future__ import annotations

import json
import os
import random
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import multiagent_sim as ms  # noqa: E402 — путь к модулю ставится выше

ARGS = [a for a in sys.argv[1:] if not a.startswith("--")]
PORT = int(ARGS[0]) if ARGS else 6396
BIN = os.environ.get("BIN", "./kvstore-server")

SCOPE = ms.SCOPE
POISONS, UNOBSERVED = ms.POISONS, ms.UNOBSERVED
MAILS, OUTSIDE, UPS = ms.MAILS, ms.OUTSIDE, ms.UPS
OBSERVED = "poison1"  # то, что человек УВИДЕЛ; про остальные две он не знает


# ═══ файловая память ════════════════════════════════════════════════════════

class FileMemory:
    """Память как один markdown-файл — ровно то, чем агент пользуется без движка.

    Снимок берётся после каждой записи: это лучший случай (память под git с
    коммитом на факт), и именно он делает стратегию отката вообще возможной."""

    def __init__(self) -> None:
        self.lines: list[str] = []
        self.snaps: list[tuple[str, list[str]]] = []  # (id записанного, состояние ПОСЛЕ)

    @staticmethod
    def _line(f: ms.Fact) -> str:
        return (f"- [{f.fid}] source={f.source} valid_from={f.valid_from} "
                f"scope={f.scope} — {f.text}")

    @staticmethod
    def _fid(line: str) -> str:
        return line.split("[", 1)[1].split("]", 1)[0]

    @staticmethod
    def _source(line: str) -> str:
        """Источник берётся РАЗБОРОМ ПОЛЯ, а не поиском подстроки: иначе колонка
        «по источнику» уносила бы чужие факты, в тексте которых встретилось имя
        канала, и её проигрыш был бы артефактом грубого инструмента."""
        return line.split("source=", 1)[1].split(" ", 1)[0]

    def remember(self, f: ms.Fact) -> None:
        if f.supersedes:
            # Обновление НА МЕСТЕ: в файле не остаётся двух противоречивых строк.
            for i, line in enumerate(self.lines):
                if self._fid(line) == f.supersedes:
                    self.lines[i] = self._line(f)
                    break
            else:
                self.lines.append(self._line(f))
        else:
            self.lines.append(self._line(f))
        self.snaps.append((f.fid, list(self.lines)))

    def state_before(self, fid: str) -> list[str]:
        """Состояние файла НЕПОСРЕДСТВЕННО ПЕРЕД записью с этим id."""
        for i, (written, _) in enumerate(self.snaps):
            if written == fid:
                return list(self.snaps[i - 1][1]) if i else []
        raise SystemExit(f"в файловой памяти нет записи {fid}")

    # ── три стратегии лечения; каждая отдаёт НОВОЕ состояние файла ──
    def cure_rollback(self) -> list[str]:
        return self.state_before(OBSERVED)

    def cure_manual(self) -> list[str]:
        """Человек убирает ТО, ЧТО УВИДЕЛ. Больше он ничего не знает."""
        return [l for l in self.lines if self._fid(l) != OBSERVED]

    def cure_by_source(self, channel: str) -> list[str]:
        return [l for l in self.lines if self._source(l) != channel]


def file_visible(lines: list[str], fid: str) -> bool:
    """Агент читает файл целиком, поэтому виден ровно тот факт, что в нём есть."""
    return any(FileMemory._fid(l) == fid for l in lines)


def seed_sweep(now: int, seeds: range) -> tuple[int, int]:
    """⭐Устойчивость главного вывода, а не удача одного зерна.

    Откат к моменту перед ЗАМЕЧЕННОЙ подсадкой достаёт только то, что записано
    позже неё. Замечена всегда одна (остальные две в симптомную выдачу не
    попадают — в этом их смысл), а порядок записи случаен. Если бы на зерне
    1337 просто повезло, вывод был бы артефактом харнесса; поэтому доля
    считается по многим зёрнам. Сервер не нужен: величина определяется одним
    порядком записи.

    Возвращает (на скольких зёрнах откат оставляет хотя бы одну подсадку, всего)."""
    saved = ms.RNG
    stays = 0
    try:
        for s in seeds:
            ms.RNG = random.Random(s)
            order = [f.fid for f in ms.build_corpus(now)]
            if any(order.index(p) < order.index(OBSERVED) for p in POISONS):
                stays += 1
    finally:
        ms.RNG = saved
    return stays, len(seeds)


def is_evidence(*, as_truth: bool, as_record: bool) -> bool:
    """Общее для всех пяти колонок определение улики: факт ДОСТУПЕН вылеченной
    памяти, но больше НЕ ВЫДАЁТСЯ как истина. Одно определение — потому что
    иначе каждая колонка мерила бы своё и таблица ничего не сравнивала бы."""
    return as_record and not as_truth


def file_localize(lines: list[str], observed: str) -> str:
    """Канал ВЫЧИСЛЯЕТСЯ из самой памяти, как и в движке: имя не подсказывается.

    У файла это работает только потому, что источник заранее записан в строку —
    ровно то, что стоит в графе «требуется от приложения заранее»."""
    for line in lines:
        if FileMemory._fid(line) == observed:
            return FileMemory._source(line)
    raise SystemExit("ЛОКАЛИЗАЦИЯ ПО ФАЙЛУ ПРОВАЛЕНА: замеченного факта нет в файле")


def measure_file(lines: list[str], neighbours_total: int) -> dict:
    """Те же величины, что меряются у движка, но способом самой файловой памяти."""
    d = {
        "poison_left": sum(1 for f in POISONS if file_visible(lines, f)),
        "unobserved_gone": sum(1 for f in UNOBSERVED if not file_visible(lines, f)),
        "outside_alive": sum(1 for f in OUTSIDE if file_visible(lines, f)),
        "ups_alive": sum(1 for f in UPS if file_visible(lines, f)),
        "neigh_alive": sum(1 for s in ms.NEIGHBOURS
                           for i in range(1, 11) if file_visible(lines, f"{s}{i}")),
        "channel_lost": sum(1 for f in MAILS if not file_visible(lines, f)),
        "neighbours_total": neighbours_total,
    }
    # ⭐Улика — ВЫВОДИТСЯ ИЗ ИЗМЕРЕНИЯ по общему определению (см. is_evidence).
    # У файловой памяти «факт доступен» и «факт считается истиной» — одно и то
    # же поле: строка либо есть в тексте, либо её нет. Поэтому конъюнкция пуста
    # при любом исходе, и это не оценка, а устройство. Прежние состояния лежат
    # в снимках, но это вопрос к git, а не к памяти, и git не отличает правку
    # памяти от правки её истории.
    seen = file_visible(lines, OBSERVED)
    d["evidence_query"] = is_evidence(as_truth=seen, as_record=seen)
    d["belief_before"] = False  # вылеченный файл — одно состояние, прошлого не помнит
    d["reconcile"] = False      # состояние И ЕСТЬ журнал: расхождению неоткуда взяться
    d["signature"] = False
    return d


# ═══ движок ══════════════════════════════════════════════════════════════════

def wal_lsn_of(data_dir: str, fid: str) -> int:
    """LSN записи факта — то, что оператор ищет в журнале после обнаружения.

    ⚠Проверяется КОД ВОЗВРАТА: молча пустой stdout выглядел бы как «факта в
    журнале нет», хотя на деле бинарь не запустился. Тот же класс ошибки, что
    нулевой код возврата у redis-cli, из-за которого провал уже дважды
    выглядел успехом."""
    res = subprocess.run([BIN, "-data-dir", data_dir, "-wal-inspect"],
                         capture_output=True, text=True)
    if res.returncode != 0:
        raise SystemExit(f"-wal-inspect упал ({res.returncode}): {res.stderr.strip()}")
    for line in res.stdout.splitlines():
        parts = line.split()
        # SUPERSEDES пишется батчем (закрытие старой версии + новая), поэтому
        # операций записи факта две, и обе несут id в ключе.
        if len(parts) > 2 and parts[1] in ("VSIM.ADDDOC", "VSIM.ADDDOC.BATCH") \
                and parts[2] == fid:
            return int(parts[0])
    head = "\n".join(res.stdout.splitlines()[:5])
    raise SystemExit(f"в журнале нет записи {fid}; строк в журнале "
                     f"{len(res.stdout.splitlines())}, начало:\n{head}")


def measure_vmem(c: ms.Resp, texts: dict[str, str]) -> dict:
    """Видимость ВСЕГДА запросом по тексту самого факта — та же функция, что в
    основном прогоне (общий симптомный запрос давал бы зелёное по неверной
    причине: подсадки №2/№3 по нему не возвращаются никогда)."""
    v = lambda f: ms.visible(c, SCOPE, f, texts[f])  # noqa: E731
    return {
        "poison_left": sum(1 for f in POISONS if v(f)),
        "unobserved_gone": sum(1 for f in UNOBSERVED if not v(f)),
        "outside_alive": sum(1 for f in OUTSIDE if v(f)),
        "ups_alive": sum(1 for f in UPS if v(f)),
        "neigh_alive": sum(len(ms.recall_ids(c, s, 200, "посторонний факт"))
                           for s in ms.NEIGHBOURS),
        "channel_lost": sum(1 for f in MAILS if not v(f)),
        "neighbours_total": 30,
    }


def probe_evidence(c: ms.Resp, texts: dict[str, str], at: int) -> dict:
    """Улика и доказуемость — измеряются, а не проставляются руками."""
    out = {}
    try:
        out["evidence_query"] = is_evidence(
            as_truth=ms.visible(c, SCOPE, OBSERVED, texts[OBSERVED]),
            as_record=ms.visible(c, SCOPE, OBSERVED, texts[OBSERVED], "ALL"))
    except ms.RespError:
        out["evidence_query"] = False
    try:
        # «Во что агент верил ДО лечения» — вопрос, заданный самой памяти.
        out["belief_before"] = sum(
            1 for f in (*MAILS, *POISONS)
            if ms.visible(c, SCOPE, f, texts[f], "ASOF", at - 1)) == 15
    except ms.RespError:
        out["belief_before"] = False
    try:
        rec = ms._fields((c.call("VMEM.AUDIT", "RECONCILE", SCOPE) or [None, [None]])[1][0])
        out["reconcile"] = rec.get("in_memory") == "85"
    except (ms.RespError, IndexError, TypeError):
        out["reconcile"] = False
    return out


def probe_signature(c: ms.Resp, srv: ms.Server) -> bool:
    """Подпись проверяется ЧУЖОЙ библиотекой, ключом из лога старта.

    ⚠Сервер здесь стартует трижды, а ключ берётся из ПЕРВОГО совпадения в
    логе. Это намеренно: если бы ключ подписи сменился между стартами,
    закреплённый не сошёлся бы с заявленным и строка стала бы красной. Ошибка
    уходит в безопасную сторону — молчаливо принять чужой ключ этот путь не
    может."""
    try:
        pinned = srv.pinned_pubkey()
        if not pinned:
            return False
        ok, _ = ms.verify_statement_independently(c.call("VMEM.AUDIT", "EXPORT"), pinned)
        return ok
    except (ms.RespError, json.JSONDecodeError, KeyError):
        return False


# ═══ прогон ══════════════════════════════════════════════════════════════════

def yn(v: bool) -> str:
    return "да" if v else "нет"


def main() -> int:
    if not os.access(BIN, os.X_OK):
        print(f"нет бинаря {BIN} — собрать: go build -o kvstore-server ./kvstore/cmd/kvstore/")
        return 2

    now = int(time.time())
    plan = ms.build_corpus(now)          # ⭐ОДИН план на все пять колонок
    texts = {f.fid: f.text for f in plan}
    print(f"порт {PORT}, зерно {ms.SEED}, фактов в плане: {len(plan)} "
          f"(целевой скоуп {SCOPE}, соседних {len(ms.NEIGHBOURS)})")

    # ── файловая память: то же наполнение, тот же порядок ──
    fm = FileMemory()
    for f in plan:
        fm.remember(f)
    file_before = list(fm.lines)
    if not file_visible(file_before, OBSERVED):
        print("СЦЕНАРИЙ НЕ ВОСПРОИЗВЁЛСЯ: подсадки нет в файловой памяти")
        return 1
    file_channel = file_localize(file_before, OBSERVED)
    # ⭐Положительный контроль файловой памяти ДО лечения — парный к тому, что
    # снимается у движка. Без него «60 из 60 цело» после лечения ничего не
    # значит: факт, которого не было и до, зачёлся бы как уцелевший.
    base_file = measure_file(file_before, 30)
    if (base_file["poison_left"], base_file["outside_alive"],
            base_file["ups_alive"], base_file["channel_lost"]) != (3, len(OUTSIDE), 10, 0):
        print("⭐КОНТРОЛЬ ФАЙЛА ДО ЛЕЧЕНИЯ НЕ СОШЁЛСЯ — колонки недействительны:", base_file)
        return 1
    print(f"файловая память: строк {len(file_before)} (115 записей − 10 обновлений на месте), "
          f"канал по строке замеченного факта → {file_channel}")
    print(f"⭐контроль файла до лечения: подсадок {base_file['poison_left']}/3, "
          f"законных вне канала {base_file['outside_alive']}/{len(OUTSIDE)}, "
          f"supersede {base_file['ups_alive']}/10, канал цел 12/12")

    # ⭐Порядок записи подсадок относительно замеченной — тот самый факт, из-за
    # которого откат по времени лечит не то, что от него ждут. Считается, не
    # предполагается.
    order = [w for w, _ in fm.snaps]
    later = [p for p in POISONS if order.index(p) > order.index(OBSERVED)]
    print(f"позиции подсадок в порядке записи: "
          + ", ".join(f"{p}={order.index(p)}" for p in POISONS)
          + f" · записаны ПОЗЖЕ замеченной: {len(later)} из 3")
    stays, total = seed_sweep(now, range(1000, 1200))
    print(f"⭐то же на {total} других зёрнах: откат оставляет хотя бы одну подсадку "
          f"в {stays} случаях из {total} ({100 * stays // total}%) — "
          f"свойство сценария, а не зерна 1337")
    print()

    cols: list[tuple[str, dict]] = []

    # Снимок на каждую запись = лучший случай, поэтому все три файловые
    # стратегии обратимы и оставляют память пригодной для записи.
    A1 = measure_file(fm.cure_rollback(), 30)
    A1.update(reversible=True, writable=True,
              prereq="снимок на каждую запись + момент инцидента")
    cols.append(("файл: откат", A1))

    A2 = measure_file(fm.cure_manual(), 30)
    A2.update(reversible=True, writable=True,
              prereq="человек должен УВИДЕТЬ каждую ложь")
    cols.append(("файл: правка", A2))

    A3 = measure_file(fm.cure_by_source(file_channel), 30)
    A3.update(reversible=True, writable=True,
              prereq="источник в каждой строке + стабильный формат")
    cols.append(("файл: по источнику", A3))

    # ── движок: тот же корпус, тот же инцидент ──
    srv = ms.Server(PORT)
    try:
        srv.start()
        c = ms.Resp(PORT)
        ms.phase_fill(c, plan)
        base = measure_vmem(c, texts)
        if base["poison_left"] != 3 or base["outside_alive"] != len(OUTSIDE):
            print("⭐КОНТРОЛЬ ДО ЛЕЧЕНИЯ НЕ СОШЁЛСЯ — остальные колонки недействительны:",
                  base)
            return 1
        print(f"⭐контроль до лечения: подсадок видно {base['poison_left']}/3, "
              f"законных вне канала {base['outside_alive']}/{len(OUTSIDE)}, "
              f"канал цел {12 - base['channel_lost']}/12")
        c.close()
        srv.stop()
        time.sleep(0.5)

        lsn = wal_lsn_of(srv.data, OBSERVED)
        print(f"подсадка {OBSERVED} в журнале на LSN {lsn} → откат к {lsn - 1}")

        # B — откат состояния до момента перед замеченной подсадкой.
        srv.start("-restore-to-lsn", str(lsn - 1))
        c = ms.Resp(PORT)
        B = measure_vmem(c, texts)
        B.update(probe_evidence(c, texts, int(time.time())))
        B["signature"] = probe_signature(c, srv)
        B.update(reversible=True,   # каталог не изменён побайтово: снять флаг и поднять
                 writable=False,    # форензическая сессия только читает
                 prereq="ничего заранее + LSN подсадки")
        cols.append(("VMEM: restore-to-lsn", B))
        c.close()
        srv.stop()
        time.sleep(1.2)  # отзыв обязан лечь ПОЗЖЕ записей, иначе ASOF нечего показывать

        # C — отзыв по происхождению; канал вычисляется из EXPLAIN, а не подсказан.
        srv.start()
        c = ms.Resp(PORT)
        observed = ms.phase_symptom(c)
        channel = ms.phase_localize(c, observed)
        at = int(time.time())
        n = ms.phase_revoke(c, channel)
        print(f"VMEM: EXPLAIN назвал канал → {channel} (имя не подсказано), отозвано {n}")
        C = measure_vmem(c, texts)
        C.update(probe_evidence(c, texts, at))
        C["signature"] = probe_signature(c, srv)
        C.update(reversible=True, writable=True, prereq="SOURCE на записи")
        cols.append(("VMEM: QUARANTINE", C))
        c.close()
    finally:
        srv.cleanup()

    # ── таблица ──
    rows = [
        ("Ложь больше не выдаётся", lambda d: yn(d["poison_left"] == 0)),
        ("⭐Снято подсадок, которых никто не видел", lambda d: f"{d['unobserved_gone']} / 2"),
        ("Законных вне канала цело", lambda d: f"{d['outside_alive']} / {len(OUTSIDE)}"),
        ("Честных supersede цело", lambda d: f"{d['ups_alive']} / 10"),
        ("⭐Соседних скоупов цело (инцидент их не касался)",
         lambda d: f"{d['neigh_alive']} / {d['neighbours_total']}"),
        ("⚠ЦЕНА: законных потеряно в канале", lambda d: f"{d['channel_lost']} / 12"),
        ("Улика: ложь доступна запросом к памяти", lambda d: yn(d["evidence_query"])),
        ("«Во что верили до лечения» восстановимо", lambda d: yn(d["belief_before"])),
        ("Расхождение состояния с журналом обнаружимо", lambda d: yn(d["reconcile"])),
        ("Доказуемо третьей стороне (подпись)", lambda d: yn(d["signature"])),
        # ⚠Три последние строки — УСТРОЙСТВО, а не замер. Помечены явно: смешать
        # измеренное с заявленным в одной таблице значит обесценить измеренное.
        ("Лечение обратимо ‹устройство›", lambda d: yn(d["reversible"])),
        ("Память пригодна для записи после ‹устройство›", lambda d: yn(d["writable"])),
        ("Требуется от приложения заранее ‹устройство›", lambda d: d["prereq"]),
    ]

    table = [["МЕТРИКА", *(name for name, _ in cols)]]
    for label, fn in rows:
        table.append([label, *(fn(d) for _, d in cols)])
    widths = [max(len(r[i]) for r in table) for i in range(len(table[0]))]
    print()
    for n, row in enumerate(table):
        print("  ".join(cell.ljust(w) for cell, w in zip(row, widths)).rstrip())
        if n == 0:
            print("  ".join("─" * w for w in widths))

    print()
    print(f"ИНВАРИАНТЫ (без них таблица — отчёт, а не проверка; провал = код возврата 1)")
    bad = []
    by = dict(cols)
    keys = ("poison_left", "unobserved_gone", "outside_alive",
            "ups_alive", "neigh_alive", "channel_lost")

    # ⭐Главный инвариант харнесса: откат по времени — один принцип, реализованный
    # дважды и независимо (список строк в питоне и реплей журнала в Go). Числа
    # обязаны совпасть; расхождение означает ошибку в одной из реализаций, и
    # неизвестно в какой — то есть недействительны обе колонки.
    same = all(by["файл: откат"][k] == by["VMEM: restore-to-lsn"][k] for k in keys)
    bad += [] if same else ["откат файла и откат журнала разошлись в числах"]
    print(f"  откат файла ≡ откат журнала (перекрёстная проверка)      "
          f"{'OK' if same else 'ПРОВАЛ'}")

    # Отрицательный контроль: правка руками ОБЯЗАНА оставлять ненаблюдавшуюся
    # ложь. Если она вдруг «вылечила» — харнесс меряет не то, что заявляет.
    manual_blind = (by["файл: правка"]["unobserved_gone"] == 0
                    and by["файл: правка"]["poison_left"] == 2)
    bad += [] if manual_blind else ["правка руками сняла то, чего человек не видел"]
    print(f"  правка руками слепа к ненаблюдавшимся подсадкам           "
          f"{'OK' if manual_blind else 'ПРОВАЛ'}")

    # ⚠Перекрёстная проверка выше молчит, если ОБЕ реализации сломаются в одну
    # сторону — например, обе перестанут откатывать вовсе. Прикрыто структурным
    # свойством корпуса: честные обновления пишутся последними (build_corpus
    # сортирует supersede в конец), поэтому откат к любой подсадке обязан унести
    # все десять. Бесплатного отката по времени в этом сценарии не бывает.
    costly = all(by[n]["ups_alive"] == 0
                 for n in ("файл: откат", "VMEM: restore-to-lsn"))
    bad += [] if costly else ["откат оказался бесплатным — он обязан унести 10 supersede"]
    print(f"  откат по времени не бесплатен (уносит все 10 supersede)   "
          f"{'OK' if costly else 'ПРОВАЛ'}")

    # ⚠Эта строка добавлена ПОСЛЕ мутационного прогона: мутация «разбор по
    # источнику ничего не удаляет» прошла мимо всех прежних инвариантов —
    # колонка считалась, печаталась и не была прикрыта ничем. Тот же класс, что
    # незащищённый предикат карантина (a5d43ed): работающий механизм без
    # проверки, которая заметит его отказ.
    a3 = by["файл: по источнику"]
    grep_ok = (a3["poison_left"] == 0 and a3["channel_lost"] == 12
               and a3["outside_alive"] == len(OUTSIDE))
    bad += [] if grep_ok else ["разбор по источнику перестал снимать канал целиком"]
    print(f"  разбор по источнику снимает канал целиком (и платит 12)   "
          f"{'OK' if grep_ok else 'ПРОВАЛ'}")

    # Наши гарантии: отзыв по происхождению обязан держать всё это одновременно.
    c = by["VMEM: QUARANTINE"]
    ours = (c["poison_left"] == 0 and c["unobserved_gone"] == 2
            and c["outside_alive"] == len(OUTSIDE) and c["ups_alive"] == 10
            and c["neigh_alive"] == 30 and c["evidence_query"]
            and c["belief_before"] and c["reconcile"] and c["signature"])
    bad += [] if ours else ["отзыв по происхождению не держит заявленные гарантии"]
    print(f"  отзыв: ложь снята, чужое цело, улика и подпись на месте   "
          f"{'OK' if ours else 'ПРОВАЛ'}")

    # Вывод про откат не должен зависеть от зерна: ждём около 2/3 — таков ранг
    # замеченной подсадки среди трёх при равномерном перемешивании. Заметное
    # отклонение означало бы, что перемешивание в харнессе неравномерно.
    share = 100 * stays // total
    seed_ok = 50 <= share <= 80
    bad += [] if seed_ok else [f"доля по зёрнам {share}% вне ожидаемых 2/3"]
    print(f"  доля по зёрнам {share}% ≈ теоретические 67%                   "
          f"{'OK' if seed_ok else 'ПРОВАЛ'}")
    print()

    print("Читать так:")
    print("  · первые шесть строк — ЗАМЕР на одном корпусе и одном инциденте;")
    print("    строки про улику и подпись — тоже замер (вызовы к вылеченной памяти);")
    print("    три последние помечены ‹устройство› — это свойство механизма, не измерение.")
    print("  · «файл: откат» и «VMEM: restore-to-lsn» обязаны совпадать по числам:")
    print("    это один принцип в двух независимых реализациях, расхождение означало бы")
    print("    ошибку в одной из них. Совпадение — перекрёстная проверка харнесса.")
    print("  · у файловой памяти прежние состояния лежат в снимках (git), но это вопрос")
    print("    к git, а не к памяти: он не отличает правку памяти от правки её истории")
    print("    и не сверяет состояние с журналом — состояние И ЕСТЬ журнал.")
    print("  · «файл: по источнику» решает задачу так же, как отзыв. Разница не в том,")
    print("    можно ли убрать канал выборочно — можно, — а в четырёх строках после.")
    if bad:
        print()
        for b in bad:
            print("ПРОВАЛ:", b)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
