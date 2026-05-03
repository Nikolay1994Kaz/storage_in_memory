#!/usr/bin/env bash
#
# KVStore Benchmark Script
# Цель: воспроизвести 3.5M+ RPS на вашем bare-metal железе.
#
# Использование:
#   chmod +x bench.sh
#   ./bench.sh
#
# Скрипт сам соберёт сервер, запустит его, прогонит все тесты
# и покажет итоговую таблицу результатов.
#

set -euo pipefail

PORT=6399
BINARY="./kvstore_bench"
LOGFILE="bench_server.log"
RESULTS_FILE="bench_results.txt"

# ── Цвета ──
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

cleanup() {
    if [[ -n "${SERVER_PID:-}" ]]; then
        kill "$SERVER_PID" 2>/dev/null || true
        wait "$SERVER_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

echo -e "${CYAN}╔══════════════════════════════════════════════════════════════╗${NC}"
echo -e "${CYAN}║${BOLD}         KVStore Performance Benchmark Suite                  ${NC}${CYAN}║${NC}"
echo -e "${CYAN}║${NC}         $(date '+%Y-%m-%d %H:%M:%S')                               ${CYAN}║${NC}"
echo -e "${CYAN}╚══════════════════════════════════════════════════════════════╝${NC}"
echo ""

# ── 0. Системная информация ──
echo -e "${YELLOW}▸ Системная информация:${NC}"
echo "  CPU:    $(grep 'model name' /proc/cpuinfo | head -1 | cut -d: -f2 | xargs)"
echo "  Cores:  $(nproc)"
echo "  RAM:    $(free -h | awk '/Mem:/{print $2}')"
echo "  Kernel: $(uname -r)"
echo "  Go:     $(go version 2>/dev/null | awk '{print $3}')"
echo ""

# ── 1. Сборка ──
echo -e "${YELLOW}▸ Собираю сервер...${NC}"
go build -o "$BINARY" cmd/kvstore/main.go
echo -e "  ${GREEN}✓ Сборка завершена${NC}"
echo ""

# ── 2. Убить старые процессы ──
pkill -f "$BINARY" 2>/dev/null || true
sleep 0.5

# ── 3. Запуск сервера ──
echo -e "${YELLOW}▸ Запускаю сервер на порту ${PORT}...${NC}"
"$BINARY" -port "$PORT" > "$LOGFILE" 2>&1 &
SERVER_PID=$!

# Ждём готовности
for i in $(seq 1 30); do
    if redis-cli -p "$PORT" PING 2>/dev/null | grep -q PONG; then
        break
    fi
    sleep 0.5
done

if ! redis-cli -p "$PORT" PING 2>/dev/null | grep -q PONG; then
    echo -e "  ${RED}✗ Сервер не запустился! Лог:${NC}"
    cat "$LOGFILE"
    exit 1
fi

WORKERS=$(grep "workers" "$LOGFILE" | grep -oP '\d+ workers' || echo "?")
echo -e "  ${GREEN}✓ Сервер готов (PID=$SERVER_PID, $WORKERS)${NC}"
echo ""

# ── 4. Прогрев (заполняем кэш процессора) ──
echo -e "${YELLOW}▸ Прогрев (warmup)...${NC}"
redis-benchmark -p "$PORT" -t set -n 500000 -P 50 -c 50 -q > /dev/null 2>&1
echo -e "  ${GREEN}✓ Прогрев завершён (500K ключей в store)${NC}"
echo ""

# ── 5. Бенчмарки ──
> "$RESULTS_FILE"  # очищаем файл результатов

run_bench() {
    local label="$1"
    local pipeline="$2"
    local conns="$3"
    local count="$4"
    local tests="${5:-set,get}"

    echo -e "${CYAN}━━━ ${BOLD}${label}${NC}${CYAN} ━━━${NC}"
    echo -e "  Pipeline=$pipeline, Connections=$conns, Operations=$count"

    local output
    if [[ "$pipeline" -eq 1 ]]; then
        output=$(redis-benchmark -p "$PORT" -t "$tests" -n "$count" -c "$conns" -q 2>&1)
    else
        output=$(redis-benchmark -p "$PORT" -t "$tests" -n "$count" -c "$conns" -P "$pipeline" -q 2>&1)
    fi

    # Парсим результаты
    # Формат redis-benchmark -q: "SET: 1818181.88 requests per second, p50=2.175 msec"
    local set_rps get_rps
    set_rps=$(echo "$output" | grep "SET:" | grep -oP '[0-9]+\.[0-9]+(?= requests per second)' | tail -1 || echo "N/A")
    get_rps=$(echo "$output" | grep "GET:" | grep -oP '[0-9]+\.[0-9]+(?= requests per second)' | tail -1 || echo "N/A")
    [[ -z "$set_rps" ]] && set_rps="N/A"
    [[ -z "$get_rps" ]] && get_rps="N/A"

    # Форматируем с human-readable суффиксом
    if [[ "$set_rps" != "N/A" ]]; then
        local set_h=$(echo "$set_rps" | awk '{if($1>=1000000) printf "%.2fM",$1/1000000; else printf "%.1fK",$1/1000}')
        echo -e "  SET: ${GREEN}${set_rps} RPS${NC} (${BOLD}${set_h}${NC})"
    fi
    if [[ "$get_rps" != "N/A" ]]; then
        local get_h=$(echo "$get_rps" | awk '{if($1>=1000000) printf "%.2fM",$1/1000000; else printf "%.1fK",$1/1000}')
        echo -e "  GET: ${GREEN}${get_rps} RPS${NC} (${BOLD}${get_h}${NC})"
    fi

    echo "$label | P=$pipeline | c=$conns | SET=$set_rps | GET=$get_rps" >> "$RESULTS_FILE"
    echo ""
}

# ── Тесты ──

echo -e "${BOLD}${YELLOW}═══════════════════════════════════════════════════${NC}"
echo -e "${BOLD}${YELLOW}  ФАЗА 1: Базовый тест (без пайплайна)${NC}"
echo -e "${BOLD}${YELLOW}═══════════════════════════════════════════════════${NC}"
echo ""
run_bench "No Pipeline (baseline)" 1 50 200000

echo -e "${BOLD}${YELLOW}═══════════════════════════════════════════════════${NC}"
echo -e "${BOLD}${YELLOW}  ФАЗА 2: Масштабирование пайплайна${NC}"
echo -e "${BOLD}${YELLOW}═══════════════════════════════════════════════════${NC}"
echo ""
run_bench "Pipeline P=16"   16  50 2000000
run_bench "Pipeline P=32"   32  50 5000000
run_bench "Pipeline P=50"   50  50 5000000
run_bench "Pipeline P=100" 100  50 10000000
run_bench "Pipeline P=200" 200  50 10000000
run_bench "Pipeline P=500" 500  50 10000000

echo -e "${BOLD}${YELLOW}═══════════════════════════════════════════════════${NC}"
echo -e "${BOLD}${YELLOW}  ФАЗА 3: Масштабирование по соединениям${NC}"
echo -e "${BOLD}${YELLOW}═══════════════════════════════════════════════════${NC}"
echo ""
run_bench "P=100, 100 conns"  100 100 10000000
run_bench "P=100, 200 conns"  100 200 10000000
run_bench "P=100, 500 conns"  100 500 10000000

echo -e "${BOLD}${YELLOW}═══════════════════════════════════════════════════${NC}"
echo -e "${BOLD}${YELLOW}  ФАЗА 4: Стресс-тест (максимальная нагрузка)${NC}"
echo -e "${BOLD}${YELLOW}═══════════════════════════════════════════════════${NC}"
echo ""
run_bench "STRESS: P=200, 200 conns" 200 200 20000000
run_bench "STRESS: P=500, 500 conns" 500 500 20000000

# ── 6. Итоговая таблица ──
echo ""
echo -e "${CYAN}╔══════════════════════════════════════════════════════════════╗${NC}"
echo -e "${CYAN}║${BOLD}                   ИТОГОВАЯ ТАБЛИЦА                           ${NC}${CYAN}║${NC}"
echo -e "${CYAN}╚══════════════════════════════════════════════════════════════╝${NC}"
echo ""
printf "%-30s | %8s | %5s | %15s | %15s\n" "Test" "Pipeline" "Conns" "SET RPS" "GET RPS"
echo "-------------------------------+----------+-------+-----------------+-----------------"

while IFS='|' read -r label pipeline conns set_val get_val; do
    label=$(echo "$label" | xargs)
    pipeline=$(echo "$pipeline" | xargs)
    conns=$(echo "$conns" | xargs | sed 's/c=//')
    set_val=$(echo "$set_val" | xargs | sed 's/SET=//')
    get_val=$(echo "$get_val" | xargs | sed 's/GET=//')

    # Красим в зелёный если > 1M
    set_display="$set_val"
    get_display="$get_val"

    printf "%-30s | %8s | %5s | %15s | %15s\n" "$label" "$pipeline" "$conns" "$set_display" "$get_display"
done < "$RESULTS_FILE"

echo ""
echo -e "${GREEN}▸ Результаты сохранены в: ${RESULTS_FILE}${NC}"
echo -e "${GREEN}▸ Лог сервера: ${LOGFILE}${NC}"
echo ""

# ── 7. DBSIZE (сколько ключей в store) ──
DBSIZE=$(redis-cli -p "$PORT" DBSIZE 2>/dev/null | grep -oP '\d+' || echo "?")
echo -e "${YELLOW}▸ Ключей в store после тестов: ${DBSIZE}${NC}"
echo ""
echo -e "${CYAN}Done! 🚀${NC}"
