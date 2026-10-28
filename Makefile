-include .env
export

export GOPRIVATE=dmatech.ru/*

IMPORTS := cmd/bootstrap/imports.go
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.2

.PHONY: help select modules-all run dev build test test-integration vet lint fmt tidy nats-up nats-down

help: ## Список команд
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-18s %s\n", $$1, $$2}'

# При первом запуске в сборку попадают все модули.
$(IMPORTS):
	go run ./cmd/builder -all

select: ## Интерактивно выбрать модули для сборки
	go run ./cmd/builder

modules-all: ## Включить в сборку все модули
	go run ./cmd/builder -all

run: $(IMPORTS) ## Запустить приложение с выбранными модулями
	@trap 'exit 0' INT; go run -tags dynamic ./cmd/bootstrap

dev: select run ## Выбрать модули и запустить

build: $(IMPORTS) ## Собрать бинарник в bin/app
	go build -tags dynamic -o bin/app ./cmd/bootstrap

test: ## Тесты (интеграционные пропускаются без NATS_TEST_URL)
	go test -race ./...

test-integration: ## Все тесты против NATS из docker-compose (make nats-up)
	NATS_TEST_URL=nats://localhost:4222 go test -race -count=1 ./...

vet: $(IMPORTS) ## go vet для обоих вариантов сборки
	go vet ./...
	go vet -tags dynamic ./...

lint: vet ## go vet + staticcheck + проверка gofmt
	go run $(STATICCHECK) ./...
	go run $(STATICCHECK) -tags dynamic ./...
	@test -z "$$(gofmt -l cmd internal)" || (gofmt -l cmd internal; exit 1)

fmt: ## Отформатировать код
	gofmt -w cmd internal

tidy: ## Обновить go.mod и vendor/
	go mod tidy
	go mod vendor

nats-up: ## Поднять NATS с JetStream
	docker compose up -d nats

nats-down: ## Остановить NATS
	docker compose down
