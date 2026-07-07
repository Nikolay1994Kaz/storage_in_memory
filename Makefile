.PHONY: build test bench vet clean run backup restore

# Версия сборки: git-тег/коммит, вшивается в бинарь через -ldflags.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

# ─── Build ──────────────────────────────────────────
build:
	go build -ldflags="$(LDFLAGS)" -o kvstore-server ./kvstore/cmd/kvstore/

# ─── Run ────────────────────────────────────────────
run: build
	./kvstore-server --port 6380

# ─── Test ───────────────────────────────────────────
test:
	go test ./kvstore/... -count=1 -timeout 120s

# Только unit-тесты (без bench)
test-short:
	go test ./kvstore/... -run "Test" -count=1 -timeout 60s

# ─── Bench ──────────────────────────────────────────
bench:
	go test ./kvstore/internal/store/tcmalloc/... -bench=. -benchmem -count=3
	go test ./kvstore/vector/... -bench=. -benchmem -count=3

# ─── Lint ───────────────────────────────────────────
vet:
	go vet ./kvstore/...

# ─── Backup / Restore ───────────────────────────────
# Каталог данных и место под архивы переопределяются: make backup DATA_DIR=... BACKUP_DIR=...
DATA_DIR   ?= data
BACKUP_DIR ?= backups

# Полный crash-consistent снимок каталога данных в один tar.gz.
backup:
	./scripts/backup.sh $(DATA_DIR) $(BACKUP_DIR)

# Восстановление из архива: make restore BACKUP=backups/kvstore-backup-*.tar.gz
# СЕРВЕР ДОЛЖЕН БЫТЬ ОСТАНОВЛЕН.
restore:
	@test -n "$(BACKUP)" || { echo "укажите BACKUP=<путь к архиву>"; exit 1; }
	./scripts/restore.sh $(BACKUP) $(DATA_DIR)

# ─── Clean ──────────────────────────────────────────
clean:
	rm -f kvstore-server
	rm -f *.prof *.out *.svg *.test *_bin
	rm -rf data/ data_backup/

# ─── Docker ─────────────────────────────────────────
docker-build:
	docker build --build-arg VERSION=$(VERSION) -t kvstore:latest .

# Полный стек: kvstore + Ollama + auto-pull модели.
# --build: всегда собираем образ из текущих исходников, иначе docker поднимет
# закэшированный бинарь и после git pull потеряются новые команды.
up:
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f kvstore
