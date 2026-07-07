# ─── Stage 1: Build ─────────────────────────────────
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Кэшируем зависимости (меняются редко)
COPY go.mod go.sum ./
RUN go mod download

# Копируем исходный код
COPY kvstore/ kvstore/

# Версия сборки: передаётся через --build-arg VERSION=$(git describe ...).
# Дефолт "dev", если не задан.
ARG VERSION=dev

# Статическая сборка (CGO_ENABLED=0 для alpine); версия вшивается через -X.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /kvstore-server ./kvstore/cmd/kvstore/

# ─── Stage 2: Runtime ───────────────────────────────
FROM alpine:3.20

# ca-certificates — для исходящих TLS (Ollama и пр.). wget (busybox) уже есть в
# alpine и используется в HEALTHCHECK.
RUN apk add --no-cache ca-certificates

WORKDIR /app

# Только бинарник — без Go, без исходников
COPY --from=builder /kvstore-server .

# Непривилегированный пользователь: при пробое процесса в контейнере нет root
# (меньше поверхность эскалации). Каталог данных создаём и передаём во владение
# ДО VOLUME, чтобы named-volume унаследовал права — иначе процесс под uid 10001
# не сможет писать WAL. Для bind-mount владельца задаёт хост (см. docs).
RUN adduser -D -u 10001 -h /app kvstore \
    && mkdir -p /app/data \
    && chown -R kvstore:kvstore /app
USER 10001

# Данные WAL
VOLUME ["/app/data"]

# 6380 — клиентский RESP-порт; 9090 — HTTP метрики/health (/metrics, /health, /ready).
EXPOSE 6380 9090

# Liveness-проба для docker/оркестратора: бьём /health на метрик-порту (дефолт 9090).
# Если запускаете с --metrics-port <иным> или 0 — переопределите/уберите HEALTHCHECK.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:9090/health >/dev/null 2>&1 || exit 1

ENTRYPOINT ["./kvstore-server"]
# 0.0.0.0 внутри контейнера: без него published-порты (-p) не достучатся до
# сервера, который по умолчанию слушает только 127.0.0.1.
CMD ["--bind", "0.0.0.0", "--port", "6380"]
