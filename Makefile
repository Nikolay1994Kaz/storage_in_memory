.PHONY: build test bench vet clean run

# ─── Build ──────────────────────────────────────────
build:
	go build -o kvstore-server ./kvstore/cmd/kvstore/

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

# ─── Clean ──────────────────────────────────────────
clean:
	rm -f kvstore-server
	rm -f *.prof *.out *.svg *.test *_bin
	rm -rf data/ data_backup/

# ─── Docker ─────────────────────────────────────────
docker-build:
	docker build -t kvstore:latest .

# Полный стек: kvstore + Ollama + auto-pull модели
up:
	docker compose up -d

down:
	docker compose down

logs:
	docker compose logs -f kvstore
