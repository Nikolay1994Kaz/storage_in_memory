#!/bin/sh
# restore.sh — восстановление каталога данных kvstore из tar.gz-архива.
#
# ВАЖНО: сервер должен быть ОСТАНОВЛЕН на момент restore — иначе живой процесс
# перепишет файлы поверх восстановленных. Существующий каталог данных не удаляется,
# а отодвигается в <DATA_DIR>.bak.<ts> (откат вручную при ошибке).
#
# Использование: scripts/restore.sh ARCHIVE [DATA_DIR]
#   ARCHIVE  — путь к kvstore-backup-*.tar.gz
#   DATA_DIR — куда разворачивать (по умолчанию ./data)
set -eu

ARCHIVE="${1:-}"
DATA_DIR="${2:-data}"

if [ -z "$ARCHIVE" ] || [ ! -f "$ARCHIVE" ]; then
	echo "restore: архив '$ARCHIVE' не найден" >&2
	echo "usage: scripts/restore.sh ARCHIVE [DATA_DIR]" >&2
	exit 1
fi

# Отодвигаем текущий каталог данных, если он есть (не теряем его молча).
if [ -e "$DATA_DIR" ]; then
	BAK="${DATA_DIR}.bak.$(date -u +%Y%m%dT%H%M%SZ)"
	mv "$DATA_DIR" "$BAK"
	echo "restore: прежний '$DATA_DIR' отодвинут в '$BAK'"
fi

# Архив содержит basename(DATA_DIR) как корень → разворачиваем в его dirname.
DEST_PARENT="$(dirname "$DATA_DIR")"
mkdir -p "$DEST_PARENT"
tar -xzf "$ARCHIVE" -C "$DEST_PARENT"

echo "restore: $ARCHIVE → $DATA_DIR (запускайте сервер поверх этого каталога)"
