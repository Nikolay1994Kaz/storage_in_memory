#!/usr/bin/env python3
# multiagent_sim.py — многоагентный прогон памяти: пять каналов пишут в один
# скоуп, один из них скомпрометирован, восстановление меряется табло.
#
# ЧЕМ ЭТО ОТЛИЧАЕТСЯ ОТ poison_recovery.sh (и зачем нужно отдельно).
#
#  1. ⭐Скомпрометированный канал пишет НЕ ТОЛЬКО ЛОЖЬ. В старом харнессе из
#     email-agent приходила ровно одна подсадка, поэтому отзыв канала выглядел
#     бесплатным. В жизни канал до компрометации честно работал месяцами, и
#     отзыв уносит его законные факты вместе с ложью. Здесь их двенадцать, и
#     табло обязано назвать эту цену вслух, а не спрятать её.
#
#  2. ⭐Человек замечает ОДНУ ложь, а отзыв убирает ТРИ. Симптом выводит на
#     поверхность только подсадку №1; №2 и №3 лежат в других подтемах, в
#     выдаче симптомного запроса не появляются и никем не наблюдались. Их
#     снятие — измеряемая величина, и это то, ради чего вообще нужен отзыв по
#     происхождению: он достаёт то, чего оператор не видел.
#
#  3. Рядом с ложью живут ЧЕСТНЫЕ обновления (SUPERSEDES от crm-sync). Система
#     обязана не спутать «факт закрыт, потому что устарел» с «факт отозван,
#     потому что канал скомпрометирован».
#
# ⭐ПРАВИЛО, БЕЗ КОТОРОГО ПРОГОН — ПОДГОНКА. Имя виноватого канала в фазу
# отзыва НЕ ПЕРЕДАЁТСЯ. Оно вычисляется из VMEM.EXPLAIN по симптому. В первых
# живых прогонах канал подставлял автор, и цикл «обнаружили → локализовали →
# отозвали» держался на подсказке; здесь эта возможность закрыта тем, что
# функция отзыва принимает только то, что вернул EXPLAIN.
#
# ГРАБЛИ, КОТОРЫЕ ЗДЕСЬ УЖЕ ОБОЙДЕНЫ:
#  · говорим по RESP сокетом, а не через redis-cli: тот отдаёт ошибку сервера
#    НУЛЕВЫМ кодом возврата и печатает пустую строку на пустой ответ — оба раза
#    это уже приводило к тому, что провал механизма выглядел как успех;
#  · сверяем всё по ID, никогда по подстроке текста: BM25 хранит СТЕМЫ, и
#    проверка вхождения слова была бы зелёной всегда;
#  · факты пишутся без VEC — ладдер шаг 0, детерминированный плейсхолдер-вектор,
#    поэтому прогон воспроизводим побайтно.
#
# Использование: scripts/multiagent_sim.py [порт]
# Требуется собранный ./kvstore-server (go build -o kvstore-server ./kvstore/cmd/kvstore/).

from __future__ import annotations

import base64
import json
import os
import random
import re
import struct
import shutil
import socket
import subprocess
import sys
import tempfile
import time

ARGS = [a for a in sys.argv[1:] if not a.startswith("--")]
NO_REVOKE = "--no-revoke" in sys.argv
PORT = int(ARGS[0]) if ARGS else 6397
BIN = os.environ.get("BIN", "./kvstore-server")
SCOPE = "aurora"
NEIGHBOURS = ("beta", "gamma", "delta")
SYMPTOM_QUERY = "статус проекта Аврора"
SEED = 1337
DAY = 86400

# Зерно фиксировано: порядок записи перемешан, чтобы подсадка не оказалась
# удобно последней, но перемешан ВОСПРОИЗВОДИМО.
RNG = random.Random(SEED)


# ───────────────────────────── RESP-клиент ──────────────────────────────────

class RespError(Exception):
    """Сервер ответил ошибкой. Отдельный тип: молча продолжать нельзя."""


class Resp:
    def __init__(self, port: int) -> None:
        self.sock = socket.create_connection(("127.0.0.1", port), timeout=30)
        self.buf = b""

    def close(self) -> None:
        try:
            self.sock.close()
        except OSError:
            pass

    def _line(self) -> bytes:
        while b"\r\n" not in self.buf:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise RespError("соединение закрыто сервером")
            self.buf += chunk
        line, self.buf = self.buf.split(b"\r\n", 1)
        return line

    def _need(self, n: int) -> bytes:
        while len(self.buf) < n:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise RespError("соединение закрыто сервером")
            self.buf += chunk
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def _read(self):
        line = self._line()
        tag, body = line[:1], line[1:]
        if tag == b"+":
            return body.decode()
        if tag == b"-":
            raise RespError(body.decode())
        if tag == b":":
            return int(body)
        if tag == b"$":
            n = int(body)
            if n < 0:
                return None
            data = self._need(n + 2)[:-2]
            return data.decode()
        if tag == b"*":
            n = int(body)
            if n < 0:
                return None
            return [self._read() for _ in range(n)]
        raise RespError(f"неизвестный тип ответа: {line!r}")

    def call(self, *args) -> object:
        parts = [f"*{len(args)}\r\n".encode()]
        for a in args:
            b = str(a).encode()
            parts.append(b"$%d\r\n%s\r\n" % (len(b), b))
        self.sock.sendall(b"".join(parts))
        return self._read()


# ───────────────────────────── сервер ───────────────────────────────────────

class Server:
    def __init__(self, port: int) -> None:
        self.port = port
        self.work = tempfile.mkdtemp(prefix="vmem-multiagent-")
        self.data = os.path.join(self.work, "data")
        os.makedirs(self.data, exist_ok=True)
        self.log = os.path.join(self.work, "server.log")
        self.proc: subprocess.Popen | None = None

    def start(self, *extra: str) -> None:
        # ⚠log-level=info НУЖЕН: публичный ключ цепи печатается при старте, и
        # аудитор обязан взять его ИМЕННО ОТТУДА — «другим каналом», а не из
        # самого заявления. Ключ из документа ничего не доказывает: кто угодно
        # сгенерирует пару и подпишет любую голову (так и написано в export.go).
        #
        # extra — дополнительные флаги (нужны отрицательному контролю, который
        # поднимает ТОТ ЖЕ каталог с -restore-to-lsn). Лог открывается на
        # ДОПИСЫВАНИЕ: перезапуск не имеет права затирать строку с публичным
        # ключом, снятую на первом старте, иначе аудитор потеряет свой канал.
        with open(self.log, "ab") as fh:
            self.proc = subprocess.Popen(
                [BIN, "-port", str(self.port), "-data-dir", self.data,
                 "-metrics-port", "0", "-log-level", "info",
                 "-encrypt-at-rest", "-audit-chain", *extra],
                stdout=fh, stderr=subprocess.STDOUT,
            )

        for _ in range(200):
            try:
                c = Resp(self.port)
                c.call("PING")
                c.close()
                return
            except (OSError, RespError):
                time.sleep(0.05)
        raise SystemExit(f"сервер не поднялся, лог: {self.log}\n"
                         + open(self.log, encoding="utf-8", errors="replace").read())

    def pinned_pubkey(self) -> str | None:
        """Публичный ключ, закреплённый ИЗ ЛОГА СТАРТА (внешний канал)."""
        txt = open(self.log, encoding="utf-8", errors="replace").read()
        m = re.search(r'audit chain signing key.*?public_key=("?)([A-Za-z0-9+/=]+)\1', txt)
        return m.group(2) if m else None

    def stop(self) -> None:
        if self.proc:
            self.proc.terminate()
            self.proc.wait(timeout=30)
            self.proc = None

    def cleanup(self) -> None:
        self.stop()
        shutil.rmtree(self.work, ignore_errors=True)


# ───────────────────────────── корпус ───────────────────────────────────────

class Fact:
    """Один запланированный факт. План строится целиком до записи — поэтому
    порядок воспроизводим, а перемешивание каналов настоящее."""

    __slots__ = ("scope", "fid", "text", "source", "valid_from", "supersedes", "kind")

    def __init__(self, scope, fid, text, source, valid_from, kind, supersedes=None):
        self.scope = scope
        self.fid = fid
        self.text = text
        self.source = source
        self.valid_from = valid_from
        self.supersedes = supersedes
        self.kind = kind  # legit | supersede | derived | poison | noise


def build_corpus(now: int) -> list[Fact]:
    """85 фактов в целевом скоупе + 30 в соседних. Числа зафиксированы, потому
    что на них назначены пороги табло."""
    plan: list[Fact] = []

    def day(n: int) -> int:
        return now - n * DAY

    # human — 20 законных, разнесены на 90 дней назад.
    for i in range(1, 21):
        plan.append(Fact(SCOPE, f"human{i}",
                         f"проект Аврора: решение номер {i} принято на встрече команды",
                         "human", day(90 - 4 * i), "legit"))

    # crm-sync — 30 законных, из них 10 позже честно обновлены.
    for i in range(1, 31):
        plan.append(Fact(SCOPE, f"crm{i}",
                         f"проект Аврора: контакт с клиентом номер {i}, статус сделки открыт",
                         "crm-sync", day(80 - 2 * i), "legit"))
    # 10 честных SUPERSEDES — обновление, а не отзыв. Система обязана их не спутать.
    for i in range(1, 11):
        plan.append(Fact(SCOPE, f"crmup{i}",
                         f"проект Аврора: контакт с клиентом номер {i}, статус сделки согласован",
                         "crm-sync", day(20 - i), "supersede", supersedes=f"crm{i}"))

    # research-agent — 10 производных.
    for i in range(1, 11):
        plan.append(Fact(SCOPE, f"res{i}",
                         f"проект Аврора: вывод номер {i} на основании решений команды",
                         "research-agent", day(45 - 3 * i), "derived"))

    # email-agent — 12 ЗАКОННЫХ (канал честно работал до компрометации).
    for i in range(1, 13):
        plan.append(Fact(SCOPE, f"mail{i}",
                         f"проект Аврора: входящее письмо номер {i}, согласование сроков поставки",
                         "email-agent", day(70 - 5 * i), "legit"))

    # ⭐Три подсадки из того же канала. Правдоподобные деловые утверждения без
    # маркеров инъекции: ловить их детектором — чужое поле, мы восстанавливаем.
    # poison1 попадает в выдачу СИМПТОМНОГО запроса — его человек и заметит.
    plan.append(Fact(SCOPE, "poison1",
                     "проект Аврора: статус проекта — отменён, работы прекращены навсегда",
                     "email-agent", day(3), "poison"))
    # ⭐poison2 и poison3 в симптомную выдачу НЕ попадают (другие подтемы) и
    # никем не наблюдаются до отзыва. Их снятие — главная измеряемая величина.
    plan.append(Fact(SCOPE, "poison2",
                     "бюджет на закупку оборудования заморожен до конца года",
                     "email-agent", day(2), "poison"))
    plan.append(Fact(SCOPE, "poison3",
                     "команда подрядчика распущена, договор расторгнут",
                     "email-agent", day(1), "poison"))

    # Шум в соседних скоупах — обязан остаться нетронутым.
    for s in NEIGHBOURS:
        for i in range(1, 11):
            plan.append(Fact(s, f"{s}{i}",
                             f"посторонний факт номер {i} про другой проект",
                             "crm-sync", day(30 - i), "noise"))

    RNG.shuffle(plan)
    # SUPERSEDES обязан выполняться после своей цели: перемешивание порядок
    # каналов меняет, но причинность ломать не должно.
    plan.sort(key=lambda f: 1 if f.kind == "supersede" else 0)
    return plan


# ───────────────────────────── фазы ─────────────────────────────────────────

def phase_fill(c: Resp, plan: list[Fact]) -> None:
    for f in plan:
        args = [ "VMEM.REMEMBER", f.scope, "TEXT", f.text, "ID", f.fid,
                 "SOURCE", f.source, "VALIDFROM", f.valid_from ]
        if f.supersedes:
            args += ["SUPERSEDES", f.supersedes]
        c.call(*args)


def recall_ids(c: Resp, scope: str, k: int = 200, query: str = SYMPTOM_QUERY,
               *extra) -> list[str]:
    """Триплеты id, score, text → только id. Сверяем ВСЕГДА по id."""
    flat = c.call("VMEM.RECALL", scope, k, query, *extra) or []
    return [flat[i] for i in range(0, len(flat), 3)]


def visible(c: Resp, scope: str, fid: str, text: str, *extra) -> bool:
    """Виден ли КОНКРЕТНЫЙ факт — запрос строится из ЕГО СОБСТВЕННОГО текста.

    ⭐ЗАЧЕМ ОТДЕЛЬНАЯ ФУНКЦИЯ, А НЕ ОБЩИЙ ЗАПРОС. Первая версия табло проверяла
    видимость всех фактов одним симптомным запросом — и это было «зелёное по
    неверной причине» в самой ценной строке. Подсадки №2 и №3 написаны нарочно
    без слов симптомного запроса (в этом их смысл: их никто не наблюдал), а
    значит BM25 не возвращал их НИКОГДА — ни до отзыва, ни после. Строка
    «убрано подсадок, которых никто не видел» показывала бы 2 из 2 даже если
    карантин не работает вовсе. Ловится только запросом по тексту самого факта
    плюс положительным контролем ДО отзыва (см. baseline ниже)."""
    return fid in recall_ids(c, scope, 200, text, *extra)


def phase_symptom(c: Resp) -> str:
    """Человек задал вопрос и увидел ложь. Возвращает id ЗАМЕЧЕННОГО факта.
    Роль оракула здесь играет знание о мире («проект не отменён»), а не знание
    о том, из какого канала факт пришёл."""
    ids = recall_ids(c, SCOPE, 10)
    if "poison1" not in ids:
        raise SystemExit("СИМПТОМ НЕ ВОСПРОИЗВЁЛСЯ: подсадка не попала в топ-10 — "
                         "сценарий не проверяет то, ради чего написан")
    return "poison1"


def phase_localize(c: Resp, observed_id: str) -> str:
    """⭐Канал ВЫЧИСЛЯЕТСЯ из разложения. Имя сюда не передаётся."""
    recs = c.call("VMEM.EXPLAIN", SCOPE, 10, SYMPTOM_QUERY) or []
    for rec in recs:
        if not isinstance(rec, list):
            continue
        d = {rec[i]: rec[i + 1] for i in range(0, len(rec) - 1, 2)}
        if d.get("id") == observed_id or d.get("ID") == observed_id:
            src = d.get("source")
            if src:
                return src
    # Запасной путь: EXPLAIN не отдал id — берём источник факта прямым запросом
    # по каналам. Это всё ещё вычисление, а не подсказка, но помечаем.
    raise SystemExit("ЛОКАЛИЗАЦИЯ ПРОВАЛЕНА: EXPLAIN не дал source замеченного факта")


QUARANTINE_COUNTS = ("revoked", "still_trusted", "outside_window", "over_limit")


def phase_revoke(c: Resp, channel: str, since=None) -> dict:
    """Отзыв по происхождению. Возвращает КВИТАНЦИЮ, а не число отозванных:
    still_trusted говорит, сколько фактов того же канала память всё ещё считает
    истиной, — то есть отвечает на «а всё ли снято», на что голое число
    ответить не может."""
    args = ["VMEM.QUARANTINE", SCOPE, "SOURCE", channel]
    if since is not None:
        args += ["SINCE", since]
    rec = _fields(c.call(*args))
    missing = [k for k in QUARANTINE_COUNTS if k not in rec]
    if missing:
        # Не «продолжим с нулями»: молча подставленный ноль в поле про полноту
        # лечения — ровно та ложь в нашу пользу, ради устранения которой поле
        # и заведено.
        raise SystemExit(f"VMEM.QUARANTINE вернула не квитанцию, нет полей {missing}: {rec}")
    return {k: (int(v) if k in QUARANTINE_COUNTS else v) for k, v in rec.items()}


# ─────────────────── слой доказательства (шаг 3) ────────────────────────────

def _fields(rec) -> dict:
    """Плоский список name,value,… → словарь."""
    if not isinstance(rec, list):
        return {}
    return {rec[i]: rec[i + 1] for i in range(0, len(rec) - 1, 2)}


def statement_bytes(st: dict) -> bytes:
    """⭐Канонические подписываемые байты, воспроизведённые ПО ОПИСАНИЮ, а не
    вызовом нашего кода. В этом весь смысл проверки: если сторонний
    проверяющий не может собрать их сам, подпись доказывает только то, что наш
    код согласован сам с собой. Формат — префикс длины uint32 BE на строковые
    поля, uint64 BE на числовые, домен впереди (см. export.go:statementBytes)."""
    def field(b: bytes) -> bytes:
        return struct.pack(">I", len(b)) + b

    return (field(b"vmem-audit-statement-v1")
            + struct.pack(">Q", st["v"])
            + field(st["pubkey"].encode())
            + struct.pack(">Q", st["head_seq"])
            + field(st["head_hash"].encode())
            + struct.pack(">Q", st["links"])
            + struct.pack(">Q", st["signed_at"]))


def verify_statement_independently(raw: str, pinned: str) -> tuple[bool, str]:
    """Проверка ЧУЖОЙ библиотекой (cryptography), ключом, закреплённым из лога.

    Возвращает (сошлось, пояснение). Никакого нашего Go-кода в этом пути нет —
    именно поэтому результат что-то значит для внешнего аудитора."""
    try:
        from cryptography.exceptions import InvalidSignature
        from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
    except ImportError:
        return False, "нет библиотеки cryptography — проверка не выполнена"
    st = json.loads(raw)
    if st.get("pubkey") != pinned:
        return False, "ключ в заявлении НЕ совпал с закреплённым из лога"
    pad = lambda s: s + "=" * (-len(s) % 4)  # noqa: E731 — base64 без набивки
    pub = base64.b64decode(pad(st["pubkey"]))
    sig = base64.b64decode(pad(st["sig"]))
    try:
        Ed25519PublicKey.from_public_bytes(pub).verify(sig, statement_bytes(st))
    except InvalidSignature:
        return False, "подпись не сошлась"
    return True, f"head_seq={st['head_seq']}, links={st['links']}"


def _row(rows: list, label: str, want: str, fn) -> None:
    """Считает одну строку табло, превращая ЛЮБОЙ сбой в ПРОВАЛ этой строки.

    ⚠Зачем: мутация «пропустить auditAppend на REMEMBER» валила VMEM.AUDIT PROVE
    исключением, прогон обрывался, и таблица не печаталась вовсе. Мутация при
    этом технически ловилась — но диагностика была «харнесс упал», а не «вот
    какая гарантия сломалась». Инструмент разбора инцидентов обязан
    ДОКЛАДЫВАТЬ, а не падать: упавшая проверка — это тоже результат."""
    try:
        got, ok = fn()
    except Exception as exc:  # noqa: BLE001 — сбой любой природы = провал строки
        rows.append((label, f"сбой: {str(exc)[:60]}", want, False))
        return
    rows.append((label, got, want, ok))


def phase_proof(c: Resp, srv: "Server", all_ids: list[str],
                proved_id: str) -> list[tuple[str, str, str, bool | None]]:
    rows = []

    def reconcile():
        return _fields((c.call("VMEM.AUDIT", "RECONCILE", SCOPE) or [None, [None]])[1][0])

    _row(rows, "⭐Контроль: сверка видит факты скоупа", "85",
         lambda: (f"{reconcile().get('in_memory','?')} из 85",
                  reconcile().get("in_memory") == "85"))
    _row(rows, "RECONCILE: unrecorded (нет записи о создании)", "0",
         lambda: (reconcile().get("unrecorded", "?"), reconcile().get("unrecorded") == "0"))
    # ⚠ЭТА СТРОКА КРАСНАЯ ИЗ-ЗА ДЕФЕКТА ДВИЖКА, А НЕ ХАРНЕССА.
    # auditchain_reconcile.go:70 кладёт EventQuarantine в одну ветку с
    # EventForget и помечает факт «не жив», а состояние памяти собирается через
    # lvs.FactScopes(), где quarantined_at не учитывается. Но карантин ПО
    # ПОСТРОЕНИЮ оставляет факт в памяти — это улика для ASOF. Итог: каждый
    # УСПЕШНЫЙ массовый отзыв даёт resurrected = числу отозванных, то есть самый
    # тяжёлый класс тревоги срабатывает ложно ровно в том сценарии, ради
    # которого сверка и нужна. Минимальное воспроизведение: 2 факта, оба
    # отозваны → recorded 0, resurrected 2. Порог 0 верный — чинить в движке.
    _row(rows, "RECONCILE: resurrected (запись есть, факт жив)", "0",
         lambda: (reconcile().get("resurrected", "?"), reconcile().get("resurrected") == "0"))
    _row(rows, "VERIFY: цепь сходится", "ok",
         lambda: (_fields(c.call("VMEM.AUDIT", "VERIFY")).get("status", "?"),
                  _fields(c.call("VMEM.AUDIT", "VERIFY")).get("status") == "ok"))

    def export_check():
        # ⚠Порядок кортежа: _row ждёт (что_измерено, сошлось_ли), а
        # verify_statement_independently отдаёт (сошлось, пояснение). Первая
        # версия возвращала его как есть — строка светилась зелёной, показывая
        # «1», потому что непустое пояснение истинно. Ложное зелёное на
        # проверке ПОДПИСИ: ровно то, что эта строка обязана исключать.
        pinned = srv.pinned_pubkey()
        if not pinned:
            return "ключа нет в логе старта", False
        ok, why = verify_statement_independently(c.call("VMEM.AUDIT", "EXPORT"), pinned)
        return why, ok

    _row(rows, "⭐Экспорт проверен чужой библиотекой", "подпись ok", export_check)

    def prove_check():
        # ⚠Сравнение ТОЧНЫМИ значениями, а не подстрокой: id `human1` входит
        # подстрокой в `human10`…`human20`, и поиск `i in proof` объявлял
        # утечку там, где её нет. Тот же класс, что предупреждение в шапке про
        # стемы BM25 — только здесь префиксами оказались сами id.
        proof = c.call("VMEM.AUDIT", "PROVE", SCOPE, "ID", proved_id)
        vals = set()

        def walk(node):
            if isinstance(node, str):
                vals.add(node)
            elif isinstance(node, dict):
                for v in node.values():
                    walk(v)
            elif isinstance(node, list):
                for v in node:
                    walk(v)

        walk(json.loads(proof))
        leaked = [i for i in all_ids if i != proved_id and i in vals]
        return f"утекло {len(leaked)}", not leaked

    _row(rows, "⭐PROVE не раскрывает чужих фактов", "0", prove_check)

    return rows


# ───────────────────────────── табло ────────────────────────────────────────

POISONS = ("poison1", "poison2", "poison3")
UNOBSERVED = ("poison2", "poison3")
MAILS = tuple(f"mail{i}" for i in range(1, 13))
OUTSIDE = (tuple(f"human{i}" for i in range(1, 21))
           + tuple(f"crm{i}" for i in range(11, 31))
           + tuple(f"crmup{i}" for i in range(1, 11))
           + tuple(f"res{i}" for i in range(1, 11)))
UPS = tuple(f"crmup{i}" for i in range(1, 11))


def baseline(c: Resp, texts: dict[str, str]) -> dict[str, bool]:
    """⭐ПОЛОЖИТЕЛЬНЫЙ КОНТРОЛЬ, снятый ДО отзыва.

    Без него «убрано» ничего не значит: факт, который не возвращался и до
    отзыва, зачтётся как успешно отозванный. Каждая проверка «стало невидимо»
    ниже требует, чтобы ДО отзыва этот же факт тем же запросом был ВИДЕН."""
    return {fid: visible(c, SCOPE, fid, texts[fid]) for fid in (*POISONS, *MAILS)}


def scorecard(c: Resp, texts: dict[str, str], before: dict[str, bool],
              revoked_at: int) -> list[tuple[str, str, str, bool | None]]:
    """Возвращает строки (метрика, измерено, порог, прошло|None если это цена)."""
    rows = []

    # Положительный контроль сам по себе строка табло: если он не сошёлся,
    # остальные строки недействительны.
    ok_before = sum(1 for v in before.values() if v)
    rows.append(("⭐Контроль: канал был виден ДО отзыва",
                 f"{ok_before} из {len(before)}", str(len(before)),
                 ok_before == len(before)))

    left = [f for f in POISONS if visible(c, SCOPE, f, texts[f])]
    rows.append(("Подсадок в выдаче после отзыва", f"{len(left)} из 3", "0", not left))

    gone = [f for f in UNOBSERVED if before[f] and not visible(c, SCOPE, f, texts[f])]
    rows.append(("⭐Убрано подсадок, которых никто не видел",
                 f"{len(gone)} из 2", "2", len(gone) == 2))

    alive = sum(1 for f in OUTSIDE if visible(c, SCOPE, f, texts[f]))
    rows.append(("Законные вне канала целы", f"{alive} из {len(OUTSIDE)}",
                 str(len(OUTSIDE)), alive == len(OUTSIDE)))

    ups_alive = sum(1 for f in UPS if visible(c, SCOPE, f, texts[f]))
    rows.append(("Честные supersede целы", f"{ups_alive} из 10", "10", ups_alive == 10))

    neigh_alive = sum(len(recall_ids(c, s, 200, "посторонний факт")) for s in NEIGHBOURS)
    rows.append(("Соседние скоупы не задеты", f"{neigh_alive} из 30", "30", neigh_alive == 30))

    lost = sum(1 for f in MAILS if not visible(c, SCOPE, f, texts[f]))
    rows.append(("⚠ЦЕНА: потеряно законного внутри канала", f"{lost} из 12",
                 "— (не порог)", None))

    want = (*MAILS, *POISONS)
    seen = sum(1 for f in want
               if visible(c, SCOPE, f, texts[f], "ASOF", revoked_at - 1))
    rows.append(("Улика: ASOF до отзыва показывает канал", f"{seen} из 15", "15",
                 seen == len(want)))

    return rows


# ───────────────────────────── прогон ───────────────────────────────────────

def main() -> int:
    if not os.access(BIN, os.X_OK):
        print(f"нет бинаря {BIN} — собрать: go build -o kvstore-server ./kvstore/cmd/kvstore/")
        return 2

    srv = Server(PORT)
    try:
        srv.start()
        c = Resp(PORT)
        now = int(time.time())

        plan = build_corpus(now)
        print(f"порт {PORT}, зерно {SEED}, фактов в плане: {len(plan)}")
        print(f"каналов: 5 · целевой скоуп: {SCOPE} · соседних: {len(NEIGHBOURS)}")
        print()

        phase_fill(c, plan)
        cov = c.call("VMEM.COVERAGE", SCOPE)
        print("наполнение выполнено, COVERAGE:",
              " ".join(str(x) for x in (cov[0] if cov and isinstance(cov[0], list) else cov)))
        print()

        texts = {f.fid: f.text for f in plan}

        observed = phase_symptom(c)
        print(f"1. СИМПТОМ: человек увидел ложь в выдаче — факт {observed}")

        channel = phase_localize(c, observed)
        print(f"2. ЛОКАЛИЗАЦИЯ: EXPLAIN назвал канал → {channel}  (имя не подсказано)")

        before = baseline(c, texts)
        revoked_at = int(time.time())
        if NO_REVOKE:
            # ⭐Отрицательный контроль САМОГО ТАБЛО. Если без отзыва оно остаётся
            # зелёным — оно не меряет ничего, и все цифры выше декоративны.
            print("3. ОТЗЫВ ПРОПУЩЕН (--no-revoke): проверяем, что табло краснеет")
        else:
            rec = phase_revoke(c, channel)
            print(f"3. ОТЗЫВ: VMEM.QUARANTINE {SCOPE} SOURCE {channel} → "
                  f"отозвано {rec['revoked']}, осталось истиной по каналу "
                  f"{rec['still_trusted']}")
        print()

        rows = scorecard(c, texts, before, revoked_at)
        rows += phase_proof(c, srv, list(texts), 'human1')
        w = max(len(r[0]) for r in rows)
        print("ТАБЛО")
        failed = 0
        for label, got, want, ok in rows:
            mark = "цена" if ok is None else ("OK" if ok else "ПРОВАЛ")
            if ok is False:
                failed += 1
            print(f"  {label.ljust(w)}  {got:>12}   порог {want:<12} {mark}")
        print()
        if failed:
            print(f"ПРОГОН КРАСНЫЙ: провалено строк — {failed}")
            return 1
        print("ПРОГОН ЗЕЛЁНЫЙ.")
        print("⚠Зелёному с первого запуска верить нельзя — следующий шаг обязателен:")
        print("  мутировать движок и убедиться, что каждая мутация красит табло.")
        return 0
    finally:
        srv.cleanup()


if __name__ == "__main__":
    sys.exit(main())
