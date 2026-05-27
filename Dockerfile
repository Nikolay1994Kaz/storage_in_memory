# ─── Stage 1: Build ─────────────────────────────────
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Кэшируем зависимости (меняются редко)
COPY go.mod go.sum ./
RUN go mod download

# Копируем исходный код
COPY kvstore/ kvstore/

# Статическая сборка (CGO_ENABLED=0 для alpine)
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /kvstore-server ./kvstore/cmd/kvstore/

# ─── Stage 2: Runtime ───────────────────────────────
FROM alpine:3.20

RUN apk add --no-cache ca-certificates

WORKDIR /app

# Только бинарник — без Go, без исходников
COPY --from=builder /kvstore-server .

# Данные WAL
VOLUME ["/app/data"]

EXPOSE 6380

ENTRYPOINT ["./kvstore-server"]
CMD ["--port", "6380"]
