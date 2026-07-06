#!/usr/bin/env bash
# soak.sh — локальный многочасовой soak-прогон (hosted CI-раннер капает 6ч,
# поэтому длинные прогоны живут здесь; логика зеркалит .github/workflows/soak-test.yml).
#
# Сценарий:
#   build → seed oracle → фаза 1 → рестарт A + verify → фаза 2 → рестарт B + verify
#   → дрилл 1: restore из shipped WAL (file://) в ЧИСТЫЙ каталог + полный verify
#   → дрилл 2: битый CRC graph_leveled.bin → громкий rebuild-from-WAL + kv-only verify
#
# Линзы: дрейф RSS/goroutines/heap-vs-sys/tombstones/сегментов (drift.log, сэмплер
# каждые SAMPLE сек) + окно p50/p95/p99 в stress-логах + durability-оракул
# (lost vs resurrect vs searchmiss vs tenant-leak) на чекпойнтах.
#
# Использование:
#   scripts/soak.sh                          # дефолт: 2 фазы × 3ч ≈ 7ч всего
#   PHASE_DUR=30s CONC=8 NKEYS=40 SAMPLE=10 scripts/soak.sh   # смоук ~3 мин
#
# Параметры (env):
#   PHASE_DUR (3h)      длительность ОДНОЙ фазы нагрузки (их две)
#   CONC (32)           параллельных клиентов нагрузки
#   NKEYS (500)         контрольных ключей оракула
#   SAMPLE (120)        интервал drift-сэмплера, сек
#   PORT (6380)         порт сервера; MPORT (9090) порт метрик; дриллы берут +1
#   MODE_A (graceful)   рестарт A: graceful|crash (graceful — сопоставимо с прошлыми
#                       прогонами, где всплывал lost=5)
#   MODE_B (crash)      рестарт B: второй режим для покрытия SIGKILL-пути
#   WORKDIR             рабочий каталог (дефолт scratch/soak_<timestamp>)
set -euo pipefail

cd "$(dirname "$0")/.."
REPO_ROOT=$(pwd)

PHASE_DUR=${PHASE_DUR:-3h}
CONC=${CONC:-32}
NKEYS=${NKEYS:-500}
SAMPLE=${SAMPLE:-120}
PORT=${PORT:-6380}
MPORT=${MPORT:-9090}
MODE_A=${MODE_A:-graceful}
MODE_B=${MODE_B:-crash}
WORKDIR=${WORKDIR:-scratch/soak_$(date +%Y%m%d_%H%M%S)}

# Свежий каталог обязателен: старый data/ship смешал бы состояния прогонов
# (оракул мог бы «пройти» на данных прошлого запуска).
if [ -d "$WORKDIR/data" ] || [ -d "$WORKDIR/restore" ]; then
  echo "WORKDIR=$WORKDIR уже содержит data/restore прежнего прогона — удали его или задай другой WORKDIR."
  exit 2
fi
mkdir -p "$WORKDIR/logs" "$WORKDIR/ship"
WORK=$(cd "$WORKDIR" && pwd)
LOGS="$WORK/logs"
FAILURES=()

say()  { printf '\n\033[1;36m=== [%s] %s ===\033[0m\n' "$(date +%H:%M:%S)" "$*"; }
fail() { printf '\033[1;31mFAIL: %s\033[0m\n' "$*"; FAILURES+=("$*"); }

# ─── подготовка ──────────────────────────────────────────────────────────────
say "Сборка бинарников (server / stress / oracle)"
go build -o "$WORK/kvstore-server" ./kvstore/cmd/kvstore/
go build -o "$WORK/kvstore-stress" ./kvstore/cmd/stress_test/
go build -o "$WORK/soak-oracle"    ./kvstore/cmd/soak_oracle/

if curl -s -o /dev/null --max-time 2 "localhost:$MPORT/health" 2>/dev/null; then
  echo "На localhost:$MPORT уже кто-то живёт — останови прежний сервер или задай PORT/MPORT."
  exit 2
fi

SERVER_PID=""
SAMPLER_PID=""

cleanup() {
  [ -n "$SAMPLER_PID" ] && kill "$SAMPLER_PID" 2>/dev/null || true
  [ -n "$SERVER_PID" ]  && kill -9 "$SERVER_PID" 2>/dev/null || true
  printf '\nАртефакты прогона: %s\n  drift:  %s/drift.log\n  логи:   %s\n' "$WORK" "$WORK" "$LOGS"
}
trap cleanup EXIT

# start_server <logfile> [доп. флаги…] — сервер стартует с cwd=$WORK (данные в
# $WORK/data), шиппинг в file://$WORK/ship. Ждём /ready (не порт: он открывается
# ДО реплея WAL); после многочасовой фазы recovery может занять минуты.
# exec ОБЯЗАТЕЛЕН: без него $! — pid сабшелла, stop_server убивает обёртку, сервер
# осиротевает и продолжает держать порты, а «рестарт» тихо не происходит
# (следующий инстанс падает на bind, /ready отвечает старый — смоук это поймал).
start_server() {
  local log="$LOGS/$1"; shift
  ( cd "$WORK" && exec ./kvstore-server --port "$PORT" --metrics-port "$MPORT" \
      --partition-attr tenant --pprof \
      --ship-url "file://$WORK/ship" --ship-interval 1s "$@" > "$log" 2>&1 ) &
  SERVER_PID=$!
  wait_ready "$SERVER_PID" "$MPORT" "$log"
}

wait_ready() { # pid mport logfile
  for _ in $(seq 1 600); do
    kill -0 "$1" 2>/dev/null || { echo "сервер умер при старте:"; tail -20 "$3"; return 1; }
    [ "$(curl -s -o /dev/null -w '%{http_code}' "localhost:$2/ready" 2>/dev/null)" = "200" ] && return 0
    sleep 1
  done
  echo "сервер не готов за 600с"; tail -20 "$3"; return 1
}

stop_server() { # graceful|crash
  if [ "$1" = "crash" ]; then
    sleep 2                     # барьер: syncInterval=100ms → WAL fsync'нут
    kill -9 "$SERVER_PID" 2>/dev/null || true
  else
    kill -TERM "$SERVER_PID" 2>/dev/null || true
    for _ in $(seq 1 60); do kill -0 "$SERVER_PID" 2>/dev/null || break; sleep 1; done
    kill -0 "$SERVER_PID" 2>/dev/null && { fail "graceful shutdown завис"; kill -9 "$SERVER_PID"; }
  fi
  wait "$SERVER_PID" 2>/dev/null || true
  SERVER_PID=""
}

# drift-сэмплер: тот же набор рядов, что в CI (см. soak-test.yml, разводка гипотез
# RSS-дрейфа: Go-куча vs off-heap tcmalloc vs tombstones/сегменты vs flush-storm).
drift() { # метка
  { echo "=== $(date -u +%H:%M:%S) $1 ==="
    [ -n "$SERVER_PID" ] && ps -o rss= -p "$SERVER_PID" 2>/dev/null | awk '{print "server_rss_kb="$1}'
    curl -s "localhost:$MPORT/metrics" 2>/dev/null | grep -E \
      "^go_goroutines |^process_resident_memory_bytes |^go_memstats_sys_bytes |^go_memstats_heap_inuse_bytes |^kvstore_vector_total_vectors |^kvstore_vector_delta_len |^kvstore_vector_tombstones |^kvstore_vector_allocator_bytes |^kvstore_vector_data_bytes |^kvstore_vector_segments|^kvstore_vector_flush_delta_total |^kvstore_vector_compaction_total |^kvstore_vector_compaction_rollback_total |^kvstore_oom_events_total |^kvstore_wal_fail_stop_total |^kvstore_ship_errors_total |^kvstore_ship_uploaded_bytes_total |^kvstore_network_active_connections " || true
  } >> "$WORK/drift.log"
}
start_sampler() { ( while true; do sleep "$SAMPLE"; drift "$1"; done ) & SAMPLER_PID=$!; }
stop_sampler()  { [ -n "$SAMPLER_PID" ] && kill "$SAMPLER_PID" 2>/dev/null || true; SAMPLER_PID=""; }

# COMPACT по /dev/tcp (для смоука: гарантирует graph_leveled.bin перед CRC-дриллом;
# в длинном прогоне компакции идут сами по размеру WAL).
send_compact() {
  exec 3<>"/dev/tcp/localhost/$PORT" || return 1
  printf '*1\r\n$7\r\nCOMPACT\r\n' >&3
  read -r -t 5 _ <&3 || true
  exec 3<&- 3>&-
}

# ─── основной цикл ───────────────────────────────────────────────────────────
say "Старт сервера + seed оракула (n=$NKEYS)"
start_server server-1.log
"$WORK/soak-oracle" -mode seed -addr "localhost:$PORT" -n "$NKEYS"
drift "t0 after-seed"

say "Фаза 1: $PHASE_DUR × $CONC клиентов"
start_sampler "phase1"
"$WORK/kvstore-stress" -duration "$PHASE_DUR" -concurrency "$CONC" \
  -addr "localhost:$PORT" -metrics-addr "localhost:$MPORT" 2>&1 | tee "$LOGS/stress-1.log"
stop_sampler

say "Рестарт A ($MODE_A) + oracle verify"
drift "t1 pre-restart-A"
stop_server "$MODE_A"
start_server server-2.log
drift "t1 post-restart-A"
"$WORK/soak-oracle" -mode verify -addr "localhost:$PORT" -n "$NKEYS" || fail "oracle verify A"

say "Фаза 2: $PHASE_DUR × $CONC клиентов"
start_sampler "phase2"
"$WORK/kvstore-stress" -duration "$PHASE_DUR" -concurrency "$CONC" \
  -addr "localhost:$PORT" -metrics-addr "localhost:$MPORT" 2>&1 | tee "$LOGS/stress-2.log"
stop_sampler

say "Рестарт B ($MODE_B) + oracle verify"
drift "t2 pre-restart-B"
# Перед финальным стопом убеждаемся, что вектор-снапшот существует (нужен CRC-дриллу).
if [ ! -f "$WORK/data/graph_leveled.bin" ]; then
  send_compact || true
  for _ in $(seq 1 30); do [ -f "$WORK/data/graph_leveled.bin" ] && break; sleep 1; done
fi
stop_server "$MODE_B"
start_server server-3.log
drift "t2 post-restart-B"
"$WORK/soak-oracle" -mode verify -addr "localhost:$PORT" -n "$NKEYS" || fail "oracle verify B"

# ─── дрилл 1: restore из shipped артефактов ──────────────────────────────────
# Чистый каталог + -ship-restore: скачивает снапшоты и WAL из file://ship и
# поднимается. Полный verify против восстановленного инстанса = RPO-проверка
# continuous shipping (сервер B остановлен graceful → финальный тик дошипил хвост,
# ожидание RPO=0). Порты +1, чтобы не столкнуться с основным.
say "Дрилл 1: restore из shipped WAL в чистый каталог"
stop_server graceful
RESTORE_DIR="$WORK/restore"
rm -rf "$RESTORE_DIR"; mkdir -p "$RESTORE_DIR"
( cd "$RESTORE_DIR" && exec "$WORK/kvstore-server" --port $((PORT+1)) --metrics-port $((MPORT+1)) \
    --partition-attr tenant --pprof \
    --ship-url "file://$WORK/ship" --ship-restore > "$LOGS/server-restore.log" 2>&1 ) &
SERVER_PID=$!
if wait_ready "$SERVER_PID" $((MPORT+1)) "$LOGS/server-restore.log"; then
  "$WORK/soak-oracle" -mode verify -addr "localhost:$((PORT+1))" -n "$NKEYS" \
    || fail "oracle verify после ship-restore"
else
  fail "restore-сервер не поднялся из shipped артефактов"
fi
stop_server graceful

# ─── дрилл 2: битый CRC вектор-снапшота ──────────────────────────────────────
# Флипаем байт в середине graph_leveled.bin. Ожидание: сервер НЕ падает и НЕ
# грузит мусор (LoadBinary всё-или-ничего) — громкий warn + rebuild from WAL.
# KV обязан пережить полностью (snapshot.wal+WAL не тронуты) → verify -kv-only.
# Векторы легитимно неполны: их операции до последней компакции жили только в
# снапшоте. Полное восстановление от bit-rot — дрилл 1 (shipped копия).
say "Дрилл 2: битый CRC graph_leveled.bin → громкий rebuild from WAL"
GRAPH="$WORK/data/graph_leveled.bin"
if [ -f "$GRAPH" ]; then
  size=$(stat -c%s "$GRAPH")
  off=$((size / 2))
  orig=$(dd if="$GRAPH" bs=1 skip="$off" count=1 2>/dev/null | od -An -tu1 | tr -d ' ')
  printf "$(printf '\\%03o' $((orig ^ 255)))" | dd of="$GRAPH" bs=1 seek="$off" conv=notrunc 2>/dev/null
  echo "байт @$off флипнут ($orig → $((orig ^ 255))), размер файла $size"

  if start_server server-crc.log; then
    if grep -q "failed to load vector snapshot" "$LOGS/server-crc.log"; then
      echo "OK: сервер громко отверг битый снапшот и перестроился из WAL"
    else
      fail "CRC-дрилл: в логе нет 'failed to load vector snapshot' (снапшот принят молча?)"
    fi
    "$WORK/soak-oracle" -mode verify -kv-only -addr "localhost:$PORT" -n "$NKEYS" \
      || fail "oracle kv-only verify после битого CRC"
  else
    fail "CRC-дрилл: сервер не поднялся с битым снапшотом (ожидался rebuild from WAL)"
  fi
  stop_server graceful
else
  fail "CRC-дрилл пропущен: $GRAPH не существует (компакция ни разу не прошла?)"
fi

# ─── вердикт ─────────────────────────────────────────────────────────────────
say "Итог"
echo "drift-точек: $(grep -c '^=== ' "$WORK/drift.log" 2>/dev/null || echo 0) (см. $WORK/drift.log)"
if [ ${#FAILURES[@]} -eq 0 ]; then
  printf '\033[1;32mSOAK OK: обе фазы, оба verify, оба дрилла чистые.\033[0m\n'
else
  printf '\033[1;31mSOAK FAIL (%d):\033[0m\n' "${#FAILURES[@]}"
  printf '  - %s\n' "${FAILURES[@]}"
  exit 1
fi
