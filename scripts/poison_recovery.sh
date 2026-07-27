#!/usr/bin/env bash
# poison_recovery.sh — сценарий порчи памяти агента и двух стратегий
# восстановления, измеренных на ОДНИХ И ТЕХ ЖЕ данных.
#
# Что моделируется (паттерн MemGhost, arXiv 2607.05189): агент читает
# недоверенный документ, пишет из него ложный факт в долговременную память, и
# следующие сессии действуют по ложной посылке. Атака здесь детерминированная,
# без модели в контуре, и это осознанно: мы не детектируем, мы восстанавливаем,
# а восстановлению всё равно, как ложь попала в стор — подсаженный руками факт
# и факт от RL-оптимизированного пейлоада в хранилище идентичны. Цифры
# восстановления не зависят от качества атаки.
#
# Сравниваются две стратегии на одном инциденте:
#   A) откат стора целиком до момента перед подсадкой (что умеет прослойка,
#      не владеющая хранилищем: снапшот снаружи + rollback);
#   B) выборочный отзыв по происхождению (VMEM.QUARANTINE).
# Ключевая измеряемая величина — СОПУТСТВУЮЩИЕ ПОТЕРИ: сколько законных
# фактов, записанных ПОСЛЕ подсадки, не пережило восстановление.
#
# ГРАНИЦА ЧЕСТНОСТИ ЭТОГО СКРИПТА: колонка A здесь — НАША реализация отката в
# роли чужой стратегии, то есть модель чужого поведения, а не чужой код. Для
# публикации этого мало. Сравнение с настоящим OWASP Agent Memory Guard (его
# код, его политики, плюс его выборочное retire_if) — scripts/
# poison_recovery_compare.py; там же измерено, что их выборочная операция
# сопутствующих потерь НЕ несёт, и заявлять обратное нельзя.
#
# Использование: scripts/poison_recovery.sh [порт]
set -euo pipefail

PORT="${1:-6399}"
BIN="${BIN:-./kvstore-server}"
WORK="$(mktemp -d -t vmem-poison-XXXXXX)"
DATA="$WORK/data"
SCOPE="user:dana"
QUERY="проект"
PID=""

cleanup() {
  [ -n "$PID" ] && kill "$PID" 2>/dev/null || true
  wait 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

command -v redis-cli >/dev/null || { echo "нужен redis-cli"; exit 1; }
[ -x "$BIN" ] || { echo "нет бинаря $BIN (собрать: go build -o kvstore-server ./kvstore/cmd/kvstore/)"; exit 1; }

r() { redis-cli -p "$PORT" "$@"; }

start() { # start [доп-флаги...]
  "$BIN" -port "$PORT" -data-dir "$DATA" -metrics-port 0 -log-level error "$@" \
    > "$WORK/server.log" 2>&1 &
  PID=$!
  for _ in $(seq 1 200); do r PING >/dev/null 2>&1 && return 0; sleep 0.05; done
  echo "сервер не поднялся:"; cat "$WORK/server.log"; exit 1
}
stop() { kill "$PID" 2>/dev/null || true; wait 2>/dev/null || true; PID=""; sleep 0.3; }

# recall_ids — id фактов, которые агент СЕЙЧАС считает истиной.
recall_ids() { r VMEM.RECALL "$SCOPE" 100 "$QUERY" | awk 'NR%3==1' | sort; }
count_of() { recall_ids | grep -c "^$1" || true; }

mkdir -p "$DATA"
echo "рабочий каталог: $WORK"
echo

# ─────────────────────────────────────────────────────────────────────────────
echo "═══ 1. Обычная работа: агент накапливает память"
start
for i in 1 2 3 4 5; do
  r VMEM.REMEMBER "$SCOPE" TEXT "проект решение номер $i принято" \
    ID "before$i" SOURCE human >/dev/null
done
echo "   записано 5 законных фактов (source=human)"

echo
echo "═══ 2. АТАКА: агент прочитал недоверенное письмо"
echo "   письмо содержит скрытую инструкцию; агент послушно пишет ложь в память,"
echo "   в видимом ответе пользователю — ни слова (в этом и суть MemGhost)"
r VMEM.REMEMBER "$SCOPE" TEXT "проект отменён, работы прекращены навсегда" \
  ID poison SOURCE email-agent >/dev/null
echo "   подсажен факт poison (source=email-agent)"

echo
echo "═══ 3. Работа продолжается — НИКТО НИЧЕГО НЕ ЗАМЕТИЛ"
for i in 1 2 3 4; do
  r VMEM.REMEMBER "$SCOPE" TEXT "проект новая договорённость $i" \
    ID "after$i" SOURCE human >/dev/null
done
echo "   записано ещё 4 законных факта ПОСЛЕ подсадки"
echo "   агент сейчас считает истиной: $(recall_ids | tr '\n' ' ')"
echo "   ⚠ среди них poison"

stop
echo
echo "═══ 4. Инцидент замечен. Ищем, что натворил подозрительный источник"
start
echo "   VMEM.RECALL … SOURCE email-agent →"
r VMEM.RECALL "$SCOPE" 100 "$QUERY" SOURCE email-agent | awk 'NR%3==1' | sed 's/^/     /'
POISON_LSN=$("$BIN" -data-dir "$DATA" -wal-inspect | awk '$3=="poison" && $2=="VSIM.ADDDOC" {print $1; exit}')
echo "   подсадка в журнале: LSN $POISON_LSN"
stop

# ─────────────────────────────────────────────────────────────────────────────
echo
echo "═══ 5. СТРАТЕГИЯ A: откат стора целиком до момента перед подсадкой"
start -restore-to-lsn "$((POISON_LSN - 1))"
A_BEFORE=$(count_of before); A_AFTER=$(count_of after); A_POISON=$(count_of poison)
echo "   законных ДО подсадки:    $A_BEFORE / 5"
echo "   законных ПОСЛЕ подсадки: $A_AFTER / 4"
echo "   подсаженных:             $A_POISON / 1"
stop

echo
echo "═══ 6. СТРАТЕГИЯ B: выборочный отзыв по происхождению"
start
REVOKED=$(r VMEM.QUARANTINE "$SCOPE" SOURCE email-agent)
echo "   VMEM.QUARANTINE … SOURCE email-agent → отозвано: $REVOKED"
B_BEFORE=$(count_of before); B_AFTER=$(count_of after); B_POISON=$(count_of poison)
echo "   законных ДО подсадки:    $B_BEFORE / 5"
echo "   законных ПОСЛЕ подсадки: $B_AFTER / 4"
echo "   подсаженных:             $B_POISON / 1"
FORENSIC=$(r VMEM.RECALL "$SCOPE" 100 "$QUERY" ALL | awk 'NR%3==1' | grep -c '^poison' || true)
stop

# ─────────────────────────────────────────────────────────────────────────────
# Вёрстка через column: printf выравнивает по БАЙТАМ, а кириллица двухбайтовая.
echo
{
  echo "МЕТРИКА|A: откат целиком|B: выборочный отзыв"
  echo "Ложь больше не выдаётся|да|да"
  echo "Осталось видимой лжи|$A_POISON / 1|$B_POISON / 1"
  echo "Выжило законных ДО подсадки|$A_BEFORE / 5|$B_BEFORE / 5"
  echo "*Выжило законных ПОСЛЕ подсадки|$A_AFTER / 4|$B_AFTER / 4"
  echo "Улика сохранена (видна в ALL)|нет|$([ "$FORENSIC" -gt 0 ] && echo да || echo нет)"
  echo "Стор пригоден для записи после|нет (только чтение)|да"
} | column -t -s '|'
echo
echo "Обе стратегии убирают ложь. Разница в цене: откат отматывает ВСЁ,"
echo "включая работу, сделанную после подсадки, и оставляет стор только для"
echo "чтения — потому что отматывает по оси журнала, а на ней различия между"
echo "ложью и правдой попросту не существует, там есть только «раньше/позже»."
echo
echo "Чего этот скрипт НЕ показывает: выборочный отзыв умеет и прослойка поверх"
echo "чужого стора — у OWASP Agent Memory Guard это retire_if(предикат), и"
echo "сопутствующих потерь он тоже не несёт (измерено в poison_recovery_compare.py)."
echo "Разница там не в «можно ли выборочно», а в том, что остаётся после отзыва:"
echo "отозванный факт виден запросом как улика, и ASOF до отзыва честно"
echo "показывает, во что агент верил в тот момент."
