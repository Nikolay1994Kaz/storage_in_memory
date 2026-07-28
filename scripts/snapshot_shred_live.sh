#!/usr/bin/env bash
# Живой прогон второй половины криптостирания на НАСТОЯЩЕМ бинаре.
#
# Зачем отдельно от тестов. Go-тесты ставят шифрование снапшота сами, поэтому
# пропуск вызова SetSnapshotCrypto в main они не заметили бы. Здесь сервер
# поднимается как в бою, и проверяется то, что видит пользователь: после
# VMEM.SHRED факт не возвращается ПОСЛЕ ПЕРЕЗАПУСКА, то есть его нет ни в
# журнале, ни в бинарном снапшоте.
#
# Грабли, уже стоившие времени и заложенные здесь:
#   - redis-cli отдаёт ошибку сервера НУЛЕВЫМ кодом возврата → проверяем
#     префикс "ERR " в выводе, а не $?;
#   - redis-cli печатает на пустой ответ одну пустую строку → её надо срезать,
#     иначе «фактов 1» там, где их ноль.
set -u

PORT=6399   # НЕ 6380 (soak) и НЕ 6381 (личная память)
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

# call — как cli, но валит прогон на ошибке сервера (нулевой код возврата!)
call() {
  local out; out=$(cli "$@")
  if [[ "$out" == ERR\ * ]]; then bad "команда $* → $out"; fi
  printf '%s' "$out"
}

start_server() {
  "$BIN" -port "$PORT" -data-dir "$DIR" -encrypt-at-rest -idle-consolidate 0 \
    >"$DIR/server.log" 2>&1 &
  SRV_PID=$!
  for _ in $(seq 1 50); do
    [ "$(redis-cli -p "$PORT" PING 2>/dev/null)" = "PONG" ] && return 0
    sleep 0.2
  done
  bad "сервер не поднялся, см. $DIR/server.log"; cat "$DIR/server.log"; exit 1
}

stop_server() {
  kill "$SRV_PID" 2>/dev/null; wait "$SRV_PID" 2>/dev/null; SRV_PID=
}

say "Сборка"
go build -o "$BIN" "$ROOT/kvstore/cmd/kvstore" || { bad "сборка"; exit 1; }
ok "бинарь собран"

say "Шаг 1: сервер с -encrypt-at-rest, два скоупа"
start_server
call VMEM.REMEMBER alice TEXT "aurora contract signed with the steering committee" >/dev/null
call VMEM.REMEMBER bob   TEXT "standup happens every morning" >/dev/null
ALICE_BEFORE=$(cli VMEM.RECALL alice 10 aurora ALL | awk 'NR%3==1' | wc -l)
[ "$ALICE_BEFORE" -ge 1 ] && ok "факт alice записан и находится" || bad "факт alice не находится"

say "Шаг 2: снапшот снят ДО стирания — та самая копия, которую удаление не догоняет"
call COMPACT >/dev/null
sleep 1
[ -s "$DIR/graph_leveled.bin" ] || bad "graph_leveled.bin не создан"
SIZE=$(stat -c%s "$DIR/graph_leveled.bin")
ok "graph_leveled.bin: $SIZE Б"

# Содержания в снапшоте быть не должно; ключ факта — должен (иначе проверка
# ниже прошла бы по неверной причине: пустой файл не содержит ничего).
#
# ⚠Термы ищутся в СТЕММИРОВАННОЙ форме: BM25 хранит выход стеммера, и поиск
# слова «steering» не находил бы его никогда — строка была бы зелёной всегда,
# независимо от шифрования. Поймано мутацией: при отключённом шифровании
# 'aurora' и 'standup' нашлись, а 'steering' — нет.
for term in aurora steer standup; do
  if grep -aqF "$term" "$DIR/graph_leveled.bin"; then
    bad "терм '$term' лежит в снапшоте ОТКРЫТЫМ текстом"
  else
    ok "терм '$term' в снапшоте не найден"
  fi
done
if grep -aqF "vmem" "$DIR"/wal_*.log 2>/dev/null || [ "$SIZE" -gt 200 ]; then
  ok "снапшот непустой — проверка выше искала в файле с данными"
else
  bad "снапшот подозрительно пуст: проверки прошли бы по неверной причине"
fi

say "Шаг 3: VMEM.SHRED alice"
RECEIPT=$(call VMEM.SHRED alice)
printf '%s\n' "$RECEIPT" | sed 's/^/    /'
call COMPACT >/dev/null   # снапшот ПОСЛЕ стирания: alice уже нет в памяти
sleep 1

say "Шаг 4: перезапуск — факт не должен вернуться НИ ИЗ ЧЕГО"
stop_server
start_server
ALICE_AFTER=$(cli VMEM.RECALL alice 10 aurora ALL | awk 'NR%3==1' | wc -l)
BOB_AFTER=$(cli VMEM.RECALL bob 10 standup ALL | awk 'NR%3==1' | wc -l)

[ "$ALICE_AFTER" -eq 0 ] && ok "после перезапуска фактов alice: 0" \
                         || bad "после перезапуска фактов alice: $ALICE_AFTER — воскрес"
[ "$BOB_AFTER" -ge 1 ] && ok "соседний скоуп bob цел ($BOB_AFTER)" \
                       || bad "соседний скоуп bob потерян — это потеря данных, а не стирание"

say "Итог"
if [ "$FAILED" -eq 0 ]; then
  echo "  Стирание действует на бинарный снапшот: факт не вернулся после перезапуска,"
  echo "  соседний скоуп не пострадал."
else
  echo "  ЕСТЬ ПРОВАЛЫ (см. ❌ выше)"
fi
exit "$FAILED"
