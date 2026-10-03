#!/usr/bin/env bash
# ab_server.sh — A/B двух бинарей kvstore-server через redis-benchmark.
#
#   scripts/ab_server.sh load    BIN_A BIN_B [ROUNDS]  # SET/GET/PING: 50, 500 клиентов, конвейер ×16
#   scripts/ab_server.sh gc1m    BIN_A BIN_B [ROUNDS]  # ~1M живых ключей: p99 под нагрузкой + GC (gctrace)
#   scripts/ab_server.sh idle10k BIN_A BIN_B           # 10 000 молчащих соединений: RSS и потоки
#
# Прогоны A и B чередуются (A,B,A,B,…), чтобы дрейф машины — нагрев, питание
# ноутбука, фон — ложился на обе стороны, а не на одну. Сравнивать медианы и
# смотреть на разброс: на ноутбуке он ±10–30 %, вердикт на зазоре такого
# порядка не выносить. Каждый сервер — свежий каталог данных, порт 6395,
# метрики выключены. Результат: CSV redis-benchmark с меткой в первой колонке.
#
# Нужны redis-benchmark ≥ 7 (--csv с перцентилями) и python3. Тяжёлые прогоны
# запускать с потолком памяти, чтобы сбой не утянул остальную сессию:
#   systemd-run --user --scope -p MemoryMax=4G scripts/ab_server.sh load ./a ./b
set -euo pipefail
export LC_ALL=C # awk/printf с точкой как разделителем дробей

MODE=${1:?режим: load | gc1m | idle10k}
BIN_A=${2:?бинарь A}
BIN_B=${3:?бинарь B}
ROUNDS=${4:-3}
PORT=6395
OUT=${OUT:-ab_$MODE.csv}
WORK=$(mktemp -d)
SERVER_PID=""
trap 'stop_server; rm -rf "$WORK"' EXIT

start_server() { # бинарь, доп. окружение (может быть пустым)
  rm -rf "$WORK/data"; mkdir -p "$WORK/data"
  env $2 "$1" -port $PORT -bind 127.0.0.1 -metrics-port 0 -data-dir "$WORK/data" \
    -log-level error >"$WORK/server.log" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 100); do
    redis-cli -p $PORT PING >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  echo "сервер $1 не поднялся" >&2; exit 1
}

stop_server() {
  [ -n "$SERVER_PID" ] || return 0
  kill -TERM "$SERVER_PID" 2>/dev/null || true
  wait "$SERVER_PID" 2>/dev/null || true
  SERVER_PID=""
}

proc() { awk '/^VmRSS|^Threads/ {printf "%s%s ", $1, $2}' "/proc/$SERVER_PID/status"; }

bench() { # метка, сценарий, аргументы redis-benchmark…
  local label=$1 scen=$2; shift 2
  redis-benchmark -p $PORT --csv "$@" 2>/dev/null | grep -v '^"test"' |
    sed "s/^/$label,$scen,/" >>"$OUT"
}

run_load() { # метка, бинарь
  start_server "$2" ""
  bench "$1" c50    -t set,get -n 600000  -r 100000 -d 20 -c 50  --threads 4
  bench "$1" c500   -t set,get -n 600000  -r 100000 -d 20 -c 500 --threads 4
  echo "$1 c500: $(proc)" >>"$OUT.proc"
  bench "$1" c50P16 -t set,get -n 3000000 -r 100000 -d 20 -c 50 -P 16 --threads 4
  bench "$1" ping   -t ping_mbulk -n 1000000 -c 50 --threads 4
  stop_server
}

run_gc1m() { # метка, бинарь
  start_server "$2" "GODEBUG=gctrace=1"
  # ~950k уникальных ключей по 64 байта (3M случайных SET в диапазоне 1M)
  redis-benchmark -p $PORT -t set -n 3000000 -r 1000000 -d 64 -c 50 -P 16 --threads 4 -q >/dev/null 2>&1
  local keys n0 n1
  keys=$(redis-cli -p $PORT DBSIZE)
  sleep 2
  n0=$(grep -c '^gc ' "$WORK/server.log" || true)
  bench "$1" load -t set,get -n 1500000 -r 1000000 -d 64 -c 50 --threads 4
  n1=$(grep -c '^gc ' "$WORK/server.log" || true)
  # gctrace: «gc N @t s P%: STW1+mark+STW2 ms clock, …» — суммы за окно нагрузки
  grep '^gc ' "$WORK/server.log" | sed -n "$((n0 + 1)),${n1}p" | awk -v l="$1" -v k="$keys" -v rss="$(proc)" '
    { split($5, a, "+"); mark += a[2]; stw = a[1] + a[3]; if (stw > m) m = stw; n++ }
    END { printf "%s: ключей %s, %s циклов GC %d, разметка %.0f мс, STW max %.2f мс\n", l, k, rss, n, mark, m }' \
    >>"$OUT.proc"
  stop_server
}

run_idle10k() { # метка, бинарь
  start_server "$2" ""
  local before
  before=$(proc)
  python3 - "$PORT" "$1" "$SERVER_PID" "$before" >>"$OUT.proc" <<'EOF'
import socket, sys
port, label, pid, before = int(sys.argv[1]), sys.argv[2], sys.argv[3], sys.argv[4]
conns = []
for _ in range(10000):
    s = socket.create_connection(("127.0.0.1", port))
    conns.append(s)
ok = 0
for s in conns:                       # каждое соединение отвечает: сервер их обслуживает
    s.sendall(b"PING\r\n")
for s in conns:
    s.settimeout(5)
    ok += s.recv(16) == b"+PONG\r\n"
fields = {}
for line in open(f"/proc/{pid}/status"):
    k, _, v = line.partition(":")
    if k in ("VmRSS", "Threads"):
        fields[k] = v.split()[0]
print(f"{label}: до {before}| 10k соединений, PONG {ok}/10000: VmRSS {int(fields['VmRSS'])//1024} МБ, Threads {fields['Threads']}")
for s in conns:
    s.close()
EOF
  stop_server
}

: >"$OUT"; : >"$OUT.proc"
case $MODE in
load)    for _ in $(seq "$ROUNDS"); do run_load A "$BIN_A"; run_load B "$BIN_B"; done ;;
gc1m)    for _ in $(seq "$ROUNDS"); do run_gc1m A "$BIN_A"; run_gc1m B "$BIN_B"; done ;;
idle10k) run_idle10k A "$BIN_A"; run_idle10k B "$BIN_B" ;;
*)       echo "неизвестный режим $MODE" >&2; exit 2 ;;
esac

# Медианы rps и p99 по (сценарий, тест, сторона).
python3 - "$OUT" <<'EOF'
import csv, statistics as st, sys, collections
d = collections.defaultdict(list)
for r in csv.reader(open(sys.argv[1])):
    if len(r) < 10:
        continue
    d[(r[1], r[2], r[0])].append((float(r[3]), float(r[8])))
for scen, test in sorted({(k[0], k[1]) for k in d}):
    a, b = d.get((scen, test, "A")), d.get((scen, test, "B"))
    if not a or not b:
        continue
    ra, rb = st.median(x[0] for x in a), st.median(x[0] for x in b)
    print(f"{scen:7} {test:11} rps A {ra/1e3:7.0f}k  B {rb/1e3:7.0f}k ({100*(rb-ra)/ra:+5.1f}%)  "
          f"p99 A {st.median(x[1] for x in a):6.2f} ms  B {st.median(x[1] for x in b):6.2f} ms")
EOF
cat "$OUT.proc"
