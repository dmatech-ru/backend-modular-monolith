# Backend Modular Monolith on Go

Бэкенд-сервис на Go, спроектированный по архитектуре **модульного монолита (Modular Monolith)**: бизнес-домены изолированы друг от друга (low coupling), а деплоится единый бинарник с нужным набором модулей.

Взаимодействие модулей и динамического шлюза (Gateway) построено на **Protobuf-дескрипторах** и реактивном **Service Discovery поверх NATS JetStream KV**.

> Статус: каркас. Discovery, жизненный цикл и HTTP-сервер шлюза работают; проксирование запросов по дескрипторам и proto-контракты модулей ещё не реализованы.

---

## 🏗 Ключевые архитектурные решения

### 1. Сборка из выбранных модулей
Каждый модуль регистрирует себя в `init()` через `service.Register`. Набор модулей определяется blank-импортами в `cmd/bootstrap/imports.go`, который генерирует `cmd/builder` (интерактивно или флагами `-all` / `-modules`). Файл не хранится в git и подключается только с build-тегом `dynamic`.

### 2. Жизненный цикл (пакет `service`)
* `service.Register(init)` — инициализация модуля; ошибка любого модуля останавливает запуск.
* `service.OnShutdown(hook)` — хуки остановки, выполняются в обратном порядке.
* `service.Provide` / `service.Get[T]()` — общие зависимости. Bootstrap публикует одно на всё приложение соединение `*nats.Conn` и закрывает его через `Drain` после остановки модулей.

### 3. Децентрализованный Service Discovery (пакет `natsrpc`)
* **Разделение данных:** дескрипторы маршрутов и хартбиты инстансов лежат в разных бакетах. TTL есть только у хартбитов, поэтому маршруты не истекают сами по себе.
* **Публикация при старте:** каждая реплика при старте записывает свой дескриптор. Так новая сборка той же версии обновляет маршрут, а шлюз отбрасывает повторы по SHA-256.
* **Самовосстановление:** пока нода жива, keep-alive раз в `TTL/3` продлевает хартбит и восстанавливает маршрут, если тот пропал.
* **«Последний тушит свет»:** при остановке нода удаляет свой хартбит. Если других нод этой версии нет, она удаляет маршрут условным `Delete` по ревизии. Гонку с одновременно стартующей репликой это исключает: та либо видна в бакете нод, либо уже перезаписала маршрут.
* **Версионирование:** токены ключей санитизируются (`1.0.0` → `1_0_0`), поколения кода изолированы при rolling update.

### 4. Динамический Gateway
* Подписывается на маршруты через `natsrpc.WatchRoutes` (бакет создаётся сам — порядок старта модулей не важен) и держит их в таблице в памяти.
* HTTP-сервер с таймаутами, лимитами заголовков/тела/конкурентности, опциональным TLS и плавной остановкой (`shutdownDelay`).
* Эндпоинты: `GET /healthz`, `GET /readyz`, `GET {baseURL}/_routes` (известные маршруты). Остальные пути под `{baseURL}/` пока отвечают `501`.

---

## 📂 Структура проекта

```text
├── .github/workflows/   # CI: gofmt, vet, staticcheck, тесты с NATS
├── cmd/
│   ├── bootstrap/       # Точка входа: NATS, загрузка модулей, graceful shutdown
│   └── builder/         # Выбор модулей и генерация bootstrap/imports.go
├── configs/             # YAML-конфигурации модулей
├── internal/
│   ├── modules/
│   │   ├── gateway/     # Реактивный шлюз (HTTP + таблица маршрутов)
│   │   └── identity/    # Модуль аутентификации (регистрируется в discovery)
│   └── pkg/
│       ├── config/      # Дженерик-загрузчик конфигураций (YAML + env)
│       ├── logger/      # ColorHandler для slog (dev) / JSON (production)
│       ├── natsrpc/     # NewConnection, Drain, RegisterDescriptors, WatchRoutes
│       └── service/     # Реестр модулей, хуки остановки, общие зависимости
├── Dockerfile
├── docker-compose.yaml  # NATS с JetStream (+ приложение в профиле app)
└── Makefile
```

---

## 🛠 Ключи Service Discovery в NATS

| Данные | Бакет | Ключ | TTL |
| :--- | :--- | :--- | :--- |
| **Маршруты** | `service_routes` | `{env}.{service}.{version}` | нет |
| **Ноды** | `service_nodes` | `{env}.{service}.{version}.{hostname}-{nuid}` | 10 секунд |

Все токены санитизируются: любые символы, кроме `A-Z a-z 0-9 _ -`, заменяются на `_`.

---

## ⚙️ Конфигурация

| Переменная | По умолчанию | Назначение |
| :--- | :--- | :--- |
| `NATS_URL` | `nats://nats:4222` | Адрес NATS |
| `LOG_LEVEL` | `info` | Уровень логов bootstrap |
| `SHUTDOWN_TIMEOUT` | `15s` | Общий таймаут плавной остановки |
| `APP_ENV` | — | `production` включает JSON-логи |
| `GATEWAY_CONFIG_PATH`, `IDENTITY_CONFIG_PATH` | — | Пути к YAML модулей; переменные `GATEWAY_*` / `IDENTITY_*` перекрывают значения из файла |

Пример — в `.env.example` (Makefile подхватывает `.env` автоматически).

---

## 🏁 Быстрый старт

Требования: **Go 1.26+**, Docker (для NATS) или локальный `nats-server -js`.

```bash
cp .env.example .env
make nats-up      # NATS с JetStream
make run          # при первом запуске включаются все модули
```

```bash
make select       # интерактивный выбор модулей
make dev          # выбрать модули и запустить
make build        # бинарник bin/app
make lint         # vet + staticcheck + gofmt
make test-integration
```

Весь стек в Docker: `docker compose --profile app up --build`.

---

## 🔄 Жизненный цикл бизнес-модуля (пример)

```go
func init() {
    service.Register(initialization)
}

func initialization(ctx context.Context) error {
    cfg, err := config.Load[configuration]("IDENTITY_CONFIG_PATH")
    if err != nil {
        return err
    }
    log := logger.NewModuleLogger(cfg.ServiceName, cfg.LogLevel)

    nc, err := service.Get[nats.Conn]()
    if err != nil {
        return err
    }

    // Дескриптор, собранный buf из proto-контракта модуля.
    var descriptorBytes []byte

    reg, err := natsrpc.RegisterDescriptors(ctx, nc, log, cfg.Env, cfg.ServiceName, cfg.AppVersion, descriptorBytes)
    if err != nil {
        return err
    }
    service.OnShutdown(reg.Close) // снять регистрацию при остановке
    return nil
}
```

---

## Известные ограничения

* Если нода упала без graceful shutdown, её хартбит истечёт через TTL, но маршрут останется до следующего старта или остановки реплики этой версии.
* Proto-контракты и `buf`-сборка дескрипторов ещё не добавлены: `identity` публикует пустой дескриптор.
