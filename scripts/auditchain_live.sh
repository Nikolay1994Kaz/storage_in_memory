#!/usr/bin/env bash
# Живой прогон цепи аудита на НАСТОЯЩЕМ бинаре.
#
# Зачем отдельно от Go-тестов. Тесты пакета main подставляют носитель сами
# (auditChain = ...), поэтому пропуск вызова startAuditChain в main() они не
# заметили бы вообще — ровно та дыра, ради которой писался
# snapshot_shred_live.sh. Здесь сервер поднимается как в бою, и проверяется
# то, что видит пользователь:
#
#   1. с флагом -audit-chain на диске появляются три файла носителя;
#   2. квитанция VMEM.SHRED несёт номер звена, а не "off";
#   3. ⭐события переживают ПЕРЕЗАПУСК и цепь после него сходится;
#   4. без флага не создаётся ничего и квитанция честно говорит "off";
#   5. ⭐штатная остановка не теряет последний батч (Close сбрасывает буфер) —
#      иначе «выключили аккуратно» ничем не отличалось бы от «выдернули шнур».
#
# Грабли те же, что в snapshot_shred_live.sh: redis-cli отдаёт ошибку сервера
# НУЛЕВЫМ кодом возврата, поэтому проверяется префикс "ERR " в выводе; на
# пустой ответ печатается пустая строка, её надо срезать.
set -u

PORT=6398   # НЕ 6380 (soak), НЕ 6381 (личная память), НЕ 6399 (снапшотный прогон)
DIR=$(mktemp -d)
BIN=$(mktemp -u)/kvstore
ROOT=$(cd "$(dirname "$0")/.." && pwd)
FAILED=0

cleanup() {
  [ -n "${SRV_PID:-}" ] && kill "$SRV_PID" 2>/dev/null
  wait "${SRV_PID:-}" 2>/dev/null
  rm -rf "$DIR" "$(dirname "$BIN")"
}
trap cleanup EXIT

say()  { printf '\n=== %s ===\n' "$1"; }
ok()   { printf '  ✅ %s\n' "$1"; }
bad()  { printf '  ❌ %s\n' "$1"; FAILED=1; }

cli() { redis-cli -p "$PORT" "$@" | sed '/^$/d'; }

call() {
  local out; out=$(cli "$@")
  if [[ "$out" == ERR\ * ]]; then bad "команда $* → $out"; fi
  printf '%s' "$out"
}

start_server() {
  "$BIN" -port "$PORT" -data-dir "$DIR" -encrypt-at-rest -idle-consolidate 0 "$@" \
    >>"$DIR/server.log" 2>&1 &
  SRV_PID=$!
  for _ in $(seq 1 50); do
    if redis-cli -p "$PORT" PING 2>/dev/null | grep -q PONG; then return 0; fi
    sleep 0.2
  done
  bad "сервер не поднялся, лог:"; tail -20 "$DIR/server.log"; exit 1
}

# stop_server — ШТАТНАЯ остановка (SIGTERM), как systemctl stop. Именно она
# обязана досбросить буфер цепи.
stop_server() {
  kill -TERM "$SRV_PID" 2>/dev/null
  wait "$SRV_PID" 2>/dev/null
  SRV_PID=
}

say "сборка"
mkdir -p "$(dirname "$BIN")"
(cd "$ROOT" && go build -o "$BIN" ./kvstore/cmd/kvstore) || { bad "сборка"; exit 1; }
ok "бинарь собран"

# ---------------------------------------------------------------------------
say "1. цепь включена: носитель появился на диске"
start_server -audit-chain
CHAIN="$DIR/auditchain"
for f in chain.log chain.head; do
  [ -f "$CHAIN/$f" ] && ok "есть $f" || bad "нет $CHAIN/$f — startAuditChain не вызван из main"
done
ls "$CHAIN"/leaves_*.log >/dev/null 2>&1 && ok "есть файл листьев" || bad "нет leaves_*.log"

# ---------------------------------------------------------------------------
say "2. события пишутся, квитанция несёт номер звена"
for i in 1 2 3; do
  call VMEM.REMEMBER alice TEXT "факт номер $i" SOURCE agent-a >/dev/null
done
ID=$(call VMEM.REMEMBER bob TEXT "факт боба" SOURCE agent-b)
call VMEM.FORGET bob "$ID" >/dev/null

RECEIPT=$(cli VMEM.SHRED alice)
SEQ=$(printf '%s\n' "$RECEIPT" | grep -A1 '^chain_seq$' | tail -1)
case "$SEQ" in
  ""|off|unrecorded|0) bad "квитанция без номера звена: chain_seq=${SEQ:-<нет>}" ;;
  *) ok "квитанция ссылается на звено $SEQ" ;;
esac

# Форс-флаш: сразу после SHRED звено обязано быть НА ДИСКЕ, без ожидания тика.
SIZE_AFTER_SHRED=$(stat -c%s "$CHAIN/chain.log")
[ "$SIZE_AFTER_SHRED" -gt 0 ] && ok "цепь на диске сразу после SHRED ($SIZE_AFTER_SHRED Б)" \
  || bad "цепь пуста сразу после SHRED — форс-флаш не сработал"

# ---------------------------------------------------------------------------
say "3. заявление проверяется БЕЗ секрета, доказательство не раскрывает соседей"
# Публичный ключ печатается при старте — аудитор закрепляет именно его.
PUB=$(grep -o 'public_key=[^ ]*' "$DIR/server.log" | head -1 | cut -d= -f2)
[ -n "$PUB" ] && ok "публичный ключ напечатан при старте" || bad "публичного ключа нет в логе"

ID_A=$(call VMEM.REMEMBER erin TEXT "мой факт" SOURCE agent-a)
call VMEM.REMEMBER erin TEXT "чужой факт в том же батче" SOURCE agent-b >/dev/null
sleep 1.5   # тик агрегации — 1 с

cli VMEM.AUDIT EXPORT > "$DIR/statement.json"
if grep -q '"sig"' "$DIR/statement.json" && grep -q "$PUB" "$DIR/statement.json"; then
  ok "заявление подписано ключом, объявленным при старте"
else
  bad "заявление не содержит подписи либо подписано другим ключом"
fi

cli VMEM.AUDIT PROVE erin ID "$ID_A" > "$DIR/proof.json"
if grep -q "$ID_A" "$DIR/proof.json"; then ok "доказательство про запрошенный факт"; else bad "в доказательстве нет запрошенного факта"; fi
if grep -q "чужой факт" "$DIR/proof.json"; then bad "в доказательстве виден соседний факт"; else ok "соседние события не раскрыты"; fi

VER=$(cli VMEM.AUDIT VERIFY | grep -A1 '^status$' | tail -1)
[ "$VER" = "ok" ] && ok "сверка цепи проходит" || bad "VERIFY вернула status=$VER"

# ---------------------------------------------------------------------------
say "4. сверка состояния с журналом"
REC=$(cli VMEM.AUDIT RECONCILE erin)
UNREC=$(printf '%s\n' "$REC" | grep -A1 '^unrecorded$' | tail -1)
RES=$(printf '%s\n' "$REC" | grep -A1 '^resurrected$' | tail -1)
if [ "$UNREC" = "0" ] && [ "$RES" = "0" ]; then
  ok "на согласованном состоянии расхождений нет"
else
  bad "сверка нашла расхождения там, где их нет: unrecorded=$UNREC resurrected=$RES"
fi

# ---------------------------------------------------------------------------
say "5. штатная остановка досбрасывает буфер"
call VMEM.REMEMBER carol TEXT "факт после стирания" SOURCE agent-c >/dev/null
BEFORE=$(stat -c%s "$CHAIN/chain.log")
stop_server
AFTER=$(stat -c%s "$CHAIN/chain.log")
[ "$AFTER" -gt "$BEFORE" ] && ok "остановка дописала звено ($BEFORE → $AFTER Б)" \
  || bad "остановка не сбросила буфер ($BEFORE → $AFTER Б): штатный стоп теряет доказуемость как авария"

# ---------------------------------------------------------------------------
say "6. перезапуск: цепь продолжается, а не начинается заново"
LINKS_BEFORE=$AFTER
start_server -audit-chain
grep -q "audit chain opened" "$DIR/server.log" && ok "носитель поднят при старте" \
  || bad "в логе нет 'audit chain opened'"
if grep -q "audit chain recovered after an unclean stop" "$DIR/server.log"; then
  bad "после ШТАТНОЙ остановки заявлено восстановление после аварии"
else
  ok "штатная остановка не выглядит как авария"
fi

call VMEM.REMEMBER dave TEXT "факт после перезапуска" SOURCE agent-d >/dev/null
stop_server
LINKS_AFTER=$(stat -c%s "$CHAIN/chain.log")
[ "$LINKS_AFTER" -gt "$LINKS_BEFORE" ] && ok "цепь дописана поверх старой ($LINKS_BEFORE → $LINKS_AFTER Б)" \
  || bad "цепь после перезапуска не выросла"

# ---------------------------------------------------------------------------
say "7. форензическая сессия НЕ ЧИНИТ улику"
# Открытие носителя отрезает оборванный хвост, то есть ПИШЕТ в журнал. На
# сессии -restore-to-lsn, которая обязана только смотреть, это была бы правка
# улики при попытке её прочесть. Проверяем на настоящем оборванном хвосте, а
# не по строчке в логе.
printf '\x00\x00\x01\x2c\x07\x07\x07' >> "$CHAIN/chain.log"
TORN_SIZE=$(stat -c%s "$CHAIN/chain.log")
start_server -audit-chain -restore-to-lsn 1
grep -q "audit chain not opened" "$DIR/server.log" && ok "цепь не открыта на форензической сессии" \
  || bad "в логе нет отказа открывать цепь под -restore-to-lsn"
NOW_SIZE=$(stat -c%s "$CHAIN/chain.log")
[ "$NOW_SIZE" -eq "$TORN_SIZE" ] && ok "оборванный хвост не тронут ($NOW_SIZE Б)" \
  || bad "форензическая сессия ИЗМЕНИЛА журнал ($TORN_SIZE → $NOW_SIZE Б): улика правится при чтении"
stop_server

# ПАРНЫЙ КОНТРОЛЬ: обычный старт тот же хвост чинит — иначе проверка выше
# была бы зелёной и на носителе, который вообще ничего не умеет.
start_server -audit-chain
FIXED_SIZE=$(stat -c%s "$CHAIN/chain.log")
[ "$FIXED_SIZE" -lt "$TORN_SIZE" ] && ok "обычный старт хвост отрезал ($TORN_SIZE → $FIXED_SIZE Б)" \
  || bad "обычный старт не починил оборванный хвост — проверка выше ничего не значит"
grep -q "torn_tail_bytes=7" "$DIR/server.log" && ok "об оборванном хвосте сказано вслух" \
  || bad "оборванный хвост починен молча"
stop_server

# ---------------------------------------------------------------------------
say "8. без флага не пишется ничего и квитанция это признаёт"
DIR2=$(mktemp -d); trap 'rm -rf "$DIR2"' EXIT
"$BIN" -port "$PORT" -data-dir "$DIR2" -encrypt-at-rest -idle-consolidate 0 \
  >"$DIR2/server.log" 2>&1 &
SRV_PID=$!
for _ in $(seq 1 50); do
  redis-cli -p "$PORT" PING 2>/dev/null | grep -q PONG && break; sleep 0.2
done
call VMEM.REMEMBER erin TEXT "факт без цепи" SOURCE agent-e >/dev/null
SEQ2=$(cli VMEM.SHRED erin | grep -A1 '^chain_seq$' | tail -1)
[ "$SEQ2" = "off" ] && ok "chain_seq=off без флага" || bad "без цепи chain_seq=$SEQ2, ожидалось off"
[ -d "$DIR2/auditchain" ] && bad "каталог носителя создан без флага" || ok "носитель не создан"
stop_server

printf '\n'
if [ "$FAILED" -eq 0 ]; then
  printf '✅ живой прогон цепи аудита пройден\n'
else
  printf '❌ живой прогон цепи аудита ПРОВАЛЕН\n'; tail -30 "$DIR/server.log"
fi
exit "$FAILED"
