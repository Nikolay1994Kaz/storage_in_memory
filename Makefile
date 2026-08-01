.PHONY: build test test-race test-full bench vet clean run backup restore

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
#
# 🚨ОБЕ ЦЕЛИ ЗДЕСЬ РАНЬШЕ НЕ МОГЛИ ПРОЙТИ В ПРИНЦИПЕ, и это не преувеличение.
# Ни в одной не было флага -short, которым в репозитории намеренно загейтованы
# тяжёлые вещи: валидации на SIFT/dbpedia/GIST и стресс на 500k векторов. При
# этом таймауты стояли 120с и 60с, а один только TestLeveledStore_500k идёт
# больше десяти минут. То есть `make test` гарантированно упирался в таймаут,
# а `make test-short` был вдобавок назван неверно: -run "Test" не отсекает
# бенчмарки (те и так не запускаются без -bench) и никак не заменяет -short.
#
# Следствие было хуже самой поломки: локального способа прогнать тесты не
# существовало, единственным живым гейтом оставалась строка в CI, и состояние
# тяжёлых тестов не знал никто.

# ГЕЙТ. Ровно то, что гоняет CI (.github/workflows/release.yml). Должен быть
# зелёным всегда; ломается — чинить до коммита.
test:
	go test -short ./kvstore/... -count=1 -timeout 10m

# Детектор гонок по подсистемам, как в ci.yml. Кратно медленнее гейта.
test-race:
	go test -short -race ./kvstore/internal/... -count=1 -timeout 15m

# ⚠НЕ ГЕЙТ. Полный набор БЕЗ -short: валидации качества поиска и стресс-тесты.
# Идёт десятки минут, CI его не гоняет НИКОГДА и запускать в фоне под другой
# работой бессмысленно — внутри есть АБСОЛЮТНЫЕ пороги пропускной способности
# (например «≥1000 вект/с» в TestLeveledStore_500k, который берёт все 12 ядер).
# Такой порог падает от посторонней нагрузки на машине, а не от дефекта:
# падение здесь читать как результат ИЗМЕРЕНИЯ, а не как «сломали код», и
# перепроверять на свободном железе.
test-full:
	go test ./kvstore/... -count=1 -timeout 90m

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

# Стек по умолчанию: kvstore + метрики (Grafana/VictoriaMetrics), БЕЗ Ollama.
# --build: всегда собираем образ из текущих исходников, иначе docker поднимет
# закэшированный бинарь и после git pull потеряются новые команды.
up:
	docker compose up -d --build

# То же + опциональный RAG-слой (Ollama, профиль ai): AI.EMBED/INGEST/SEARCH/ASK.
# Первый запуск качает модели (~0.3GB embeddings + ~7GB chat для AI.ASK;
# только embeddings: OLLAMA_SKIP_CHAT_MODEL=1 make up-ai).
up-ai:
	docker compose --profile ai up -d --build

# --profile ai, чтобы down гасил и Ollama-контейнеры, если они были подняты
# (без активного профиля compose их не видит); лишний профиль безвреден.
down:
	docker compose --profile ai down

logs:
	docker compose logs -f kvstore
