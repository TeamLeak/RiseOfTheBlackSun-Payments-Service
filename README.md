# RiseOfTheBlackSun-Payments-Service

Микросервис на Go для работы с эквайрингом Т-Банка.

## Требования

- Go 1.20+
- Docker (для контейнеризации)
- Доступ к платёжному шлюзу Т-Банка (ключи/API endpoint)

## Установка и запуск

```bash
# Клонировать репозиторий
git clone https://github.com/TeamLeak/RiseOfTheBlackSun-Payments-Service.git
cd RiseOfTheBlackSun-Payments-Service

# Сборка Docker-образа
docker build -t payments-service .

# Запуск контейнера
docker run -d --name payments-svc \
  -p 39756:8080 \
  -v "$(pwd)":/app \
  -e CONFIG_PATH="/app/config.yaml" \
  payments-service
```

Сервис будет доступен по адресу `http://localhost:39756`.
