#!/bin/sh
# backup.sh — снимок каталога данных kvstore в один tar.gz-архив.
#
# Почему копии каталога достаточно: WAL — полный источник правды (replay
# восстанавливает KV + векторы + zset + TTL), снапшоты graph_leveled.bin/
# snapshot.wal — лишь оптимизация старта. Значит архив всего каталога = ПОЛНЫЙ
# бэкап. Бэкап crash-consistent даже на живом сервере: снапшот пишется атомарно
# (tmp+rename) и с CRC (детект bit-rot), а частичная хвостовая запись WAL
# пропускается на replay по CRC — то есть архив эквивалентен состоянию на момент
# «краша», которое путь восстановления и так корректно обрабатывает.
#
# Для минимального окна потери на restore перед бэкапом можно форсировать свежий
# снапшот: `redis-cli -p 6380 COMPACT` (не обязательно для корректности).
#
# Использование: scripts/backup.sh [DATA_DIR] [DEST_DIR]
#   DATA_DIR — каталог данных (по умолчанию ./data)
#   DEST_DIR — куда класть архив (по умолчанию ./backups)
set -eu

DATA_DIR="${1:-data}"
DEST_DIR="${2:-backups}"

if [ ! -d "$DATA_DIR" ]; then
	echo "backup: каталог данных '$DATA_DIR' не найден" >&2
	exit 1
fi

mkdir -p "$DEST_DIR"

# UTC-таймстамп в имени — сортируется лексикографически по времени.
TS="$(date -u +%Y%m%dT%H%M%SZ)"
OUT="$DEST_DIR/kvstore-backup-$TS.tar.gz"

# Архивируем каталог как есть (basename внутри архива), чтобы restore разворачивал
# обратно в тот же относительный путь.
tar -czf "$OUT" -C "$(dirname "$DATA_DIR")" "$(basename "$DATA_DIR")"

SIZE="$(du -h "$OUT" | cut -f1)"
echo "backup: $DATA_DIR → $OUT ($SIZE)"
