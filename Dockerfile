# syntax=docker/dockerfile:1.4

# ========= STAGE 1: Build =========
FROM golang:1.24-alpine AS builder

RUN apk add --no-cache \
      gcc \
      musl-dev \
      sqlite-dev

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o payments-service .

# ========= STAGE 2: Runtime =========
FROM alpine:latest

RUN apk add --no-cache \
      sqlite-libs \
      ca-certificates

# Копируем бинарь из билд-стадии
COPY --from=builder /src/payments-service /usr/local/bin/payments-service

# Рабочая папка
WORKDIR /app

# Объявляем /app как точку подвеса тома
VOLUME ["/app"]

EXPOSE 8080

ENTRYPOINT ["payments-service"]
