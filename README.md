# Backend Modular Monolith on Go

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Бэкенд-сервис на Go, спроектированный по архитектуре **модульного монолита (Modular Monolith)**: бизнес-домены изолированы друг от друга (low coupling), а деплоится единый бинарник с нужным набором модулей.

Взаимодействие модулей и динамического шлюза (Gateway) построено на **Protobuf-дескрипторах** и реактивном **Service Discovery поверх NATS JetStream KV**.

> Статус: каркас. Discovery, жизненный цикл, HTTP-сервер шлюза, proxy-мост HTTP↔NATS и SRP-аутентификация identity (Register / InitializeSession / CompleteAuthentication) поверх PostgreSQL работают целиком. Миграции и сиды применяются отдельным разовым прогоном (`cmd/setup`), не при старте реплики.

---

## 🏗 Ключевые архитектурные решения

### 1. Сборка из выбранных модулей
Каждый модуль регистрирует себя в `init()` через `service.Register`. Набор модулей определяется blank-импортами в `cmd/bootstrap/imports.go`, который генерирует `cmd/builder` (интерактивно или флагами `-all` / `-modules`). Файл не хранится в git и подключается только с build-тегом `dynamic`.

### 2. Жизненный цикл (пакет `service`)
* `service.Register(init)` — инициализация модуля; ошибка любого модуля останавливает запуск.
* `service.OnShutdown(hook)` — хуки остановки, выполняются в обратном порядке.
* `service.Provide` / `service.Get[T]()` — общие зависимости. Bootstrap публикует одно на всё приложение соединение `*nats.Conn` (пакет `natsconn`) и закрывает его через `natsconn.Drain` после остановки модулей.

NATS-инфраструктура разнесена по трём независимым пакетам вместо одного монолитного:
* `internal/pkg/natsconn` — подключение к NATS и `Drain` при остановке.
* `internal/pkg/natsdiscovery` — кто есть в системе (см. п.3).
* `internal/pkg/natsrpc` — как вызвать найденный сервис (см. п.4) и мост NATS↔`http.Handler` на стороне модуля.
* `internal/pkg/protoset` — чистая protobuf-утилита (без зависимости от NATS): собирает самодостаточный `FileDescriptorSet` из дескриптора модуля и его транзитивных импортов.

### 3. Децентрализованный Service Discovery (пакет `natsdiscovery`)
* **Разделение данных:** дескрипторы маршрутов и хартбиты инстансов лежат в разных бакетах. TTL есть только у хартбитов, поэтому маршруты не истекают сами по себе.
* **Публикация при старте:** каждая реплика при старте записывает свой дескриптор (`RegisterDescriptors`, байты собраны через `protoset.Build`). Так новая сборка той же версии обновляет маршрут, а шлюз отбрасывает повторы по SHA-256.
* **Самовосстановление:** пока нода жива, keep-alive раз в `TTL/3` продлевает хартбит и восстанавливает маршрут, если тот пропал.
* **«Последний тушит свет»:** при остановке нода удаляет свой хартбит. Если других нод этой версии нет, она удаляет маршрут условным `Delete` по ревизии. Гонку с одновременно стартующей репликой это исключает: та либо видна в бакете нод, либо уже перезаписала маршрут.
* **Чистка мёртвых маршрутов:** «последний тушит свет» предполагает graceful shutdown. Если все реплики версии упали без него (хартбиты просто истекут по TTL), маршрут деregister'ить было некому — `StartStaleRouteReaper` (запускает гейтвей, единственный постоянный потребитель маршрутов) раз в `DefaultStaleRouteReapInterval` удаляет маршруты, для которых в бакете нод не осталось ни одной живой записи, тем же условным `Delete` по ревизии.
* **Версионирование:** токены ключей санитизируются (`1.0.0` → `1_0_0`), поколения кода изолированы при rolling update.

### 4. Динамический Gateway
* Подписывается на маршруты через `natsdiscovery.WatchRoutes` (бакет создаётся сам — порядок старта модулей не важен) и держит их в таблице в памяти, дополнительно разбирая дескриптор (`protobuf.FileDescriptorSet`) на полные имена gRPC/Connect-сервисов — так строится индекс `сервис → владеющий модуль`.
* HTTP-сервер (пакет `internal/pkg/httpserver`, переиспользуемый и модулями) с таймаутами, лимитами заголовков/тела/конкурентности, опциональным TLS и плавной остановкой (`shutdownDelay`).
* Эндпоинты: `GET /healthz`, `GET /readyz`, `GET {baseURL}/_routes` (известные маршруты). Остальные пути под `{baseURL}/{package.Service}/{Method}` пересылаются владеющему модулю через NATS request-reply (`natsrpc.RPCSubject`) и ждут ответа не дольше `proxyRequestTimeout` (отдельного от `requestTimeout` статических эндпоинтов); без подписчика на subject'е клиент получает `503`.

---

## 📂 Структура проекта

```text
├── .github/workflows/   # CI: gofmt, vet, staticcheck, тесты с NATS
├── cmd/
│   ├── bootstrap/       # Точка входа рантайма: NATS, загрузка модулей, graceful shutdown
│   ├── builder/         # Выбор модулей и генерация bootstrap/imports.go
│   └── setup/           # Разовый прогон: миграции и сиды модулей перед раскаткой реплик
├── configs/             # YAML-конфигурации модулей
├── internal/
│   ├── modules/
│   │   ├── gateway/     # Реактивный шлюз (HTTP + таблица маршрутов)
│   │   └── identity/    # Модуль аутентификации (SRP + PostgreSQL, регистрируется в discovery)
│   │       ├── migrations/ # SQL-миграции схемы identity (go:embed)
│   │       ├── repository/ # Репозиторий identity поверх PostgreSQL (auth, challenges, users)
│   │       └── setup.go    # Миграции + сиды identity, вызывается из cmd/setup (не из initialization)
│   └── pkg/
│       ├── config/        # Дженерик-загрузчик конфигураций (YAML + env)
│       ├── httpserver/     # Переиспользуемый HTTP-сервер (таймауты, лимиты, graceful shutdown)
│       ├── logger/        # ColorHandler для slog (dev) / JSON (production)
│       ├── minio/         # Подключение к MinIO/S3 и сидирование файловых ассетов (пока не подключён ни к одному модулю)
│       ├── natsconn/      # Подключение к NATS и Drain при остановке
│       ├── natsdiscovery/ # Service discovery поверх NATS JetStream KV
│       ├── natsrpc/       # RPC-транспорт поверх NATS (subject'ы, мост HTTP↔NATS)
│       ├── postgres/      # Подключение к PostgreSQL, миграции и сиды схемы модуля
│       ├── protoset/      # Сборка protobuf.FileDescriptorSet из дескриптора + импортов
│       ├── srp/           # SRP-6a: серверная и клиентская математика (RFC 5054, SHA-256)
│       ├── service/       # Реестр модулей рантайма, хуки остановки, общие зависимости
│       └── setup/         # Реестр одноразовых задач подготовки окружения (миграции/сиды) для cmd/setup
├── rest-examples/       # Примеры HTTP-запросов (humao.rest-client) к gateway/identity
├── seeds/               # SQL-сиды и файловые ассеты модулей (см. seeds/README.md), в бинарь не упаковываются
├── Dockerfile
├── docker-compose.yaml  # NATS с JetStream + PostgreSQL (+ приложение в профиле app)
├── LICENSE              # MIT
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
| `IDENTITY_POSTGRES_URL` | из `configs/identity.yaml` | DSN PostgreSQL для identity, схема `identity` |
| `IDENTITY_SEEDS_DB` | из `configs/identity.yaml` (`seeds.db`) | Каталог SQL-сидов identity для `cmd/setup` |
| `IDENTITY_SEEDS_LEVELS` | из `configs/identity.yaml` (`seeds.levels`) | Уровни сидов по порядку применения, через запятую (например `shared,development`) |
| `IDENTITY_ANTI_ENUM_KEY` | из `configs/identity.yaml` | Hex-секрет (≥32 байта) для `srp.FakeCredentials` — защита `InitializeSession` от перечисления аккаунтов; должен совпадать на всех репликах |

Пример — в `.env.example` (Makefile подхватывает `.env` автоматически).

---

## 🏁 Быстрый старт

Требования: **Go 1.26+**, Docker (для NATS и PostgreSQL) или локальные `nats-server -js` и `postgres`.

```bash
cp .env.example .env
make nats-up      # NATS с JetStream
make pg-up        # PostgreSQL для identity
make setup        # миграции + сиды (один раз и после каждого изменения сидов/миграций)
make run          # при первом запуске включаются все модули
```

```bash
make select       # интерактивный выбор модулей
make dev          # выбрать модули и запустить
make build        # бинарник bin/app
make lint         # vet + staticcheck + gofmt
make test-integration
```

`make setup` (`cmd/setup`) только готовит схему БД — миграции и SQL-сиды (`internal/pkg/postgres`), без NATS и без обслуживания трафика. Обычный старт модуля (`cmd/bootstrap` / `make run`) схему больше не трогает, поэтому перед первым `make run` на чистой базе `make setup` обязателен.

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

    // Дескриптор proto-контракта модуля вместе с транзитивными импортами.
    descriptorBytes, err := protoset.Build(authv1.File_webapi_v1_identity_auth_proto)
    if err != nil {
        return err
    }

    reg, err := natsdiscovery.RegisterDescriptors(ctx, nc, log, cfg.Env, cfg.ServiceName, cfg.AppVersion, descriptorBytes)
    if err != nil {
        return err
    }
    service.OnShutdown(reg.Close) // снять регистрацию при остановке
    return nil
}
```

---

## Известные ограничения

* Гейтвей находит владеющий модуль по дескриптору и пересылает запрос через NATS (`{env}.{serviceName}.{version}.rpc.{methodPath}`, см. `natsrpc.RPCSubject`/`natsrpc.ServeHandler`); `identity` подписан через `authhandler.Subscribe` (`internal/modules/identity/handlers/webapi/v1/auth.handler.go`) и реально отвечает.
* **SRP-аутентификация identity** (`internal/pkg/srp`, группа 2048 бит из RFC 5054 Appendix A.3 с SHA-256 вместо SHA-1 — своя схема, совместимость с внешними SRP-клиентами не требовалась): `Register` → `InitializeSession` → `CompleteAuthentication`. Пароль не передаётся на сервер ни разу, в том числе при регистрации — TLS не входит в модель угроз: `Register` принимает уже готовые `salt`/`verifier`, вычисленные клиентом (`srp.GenerateSalt` + `srp.ComputeVerifier`), сервер их только проверяет по форме (`srp.ValidVerifier`) и хранит. `InitializeSession` защищён от перечисления аккаунтов: для несуществующего identifier сервер без обращения к базе отвечает детерминированными фейковыми `salt`/`verifier` (`srp.FakeCredentials`, ключ — `antiEnumerationKey`), неотличимыми по форме и коду ответа от настоящего аккаунта; пройти второй шаг с ними невозможно ни при каком пароле. Challenge'и второго шага (`identity.auth_challenges`) чистятся попутно при каждой новой `InitializeSession` (не нужна отдельная фоновая задача). Осознанное упрощение, о котором нужно знать:
  - Новый пользователь получает статус `pending` (значение по умолчанию) — ничего пока не переводит его в `active`, подтверждения email/телефона нет, хотя колонка `email_verified` под это уже есть.
* Пакет `internal/pkg/minio` адаптирован для работы в этом проекте, но пока не подключён ни к одному модулю: нет ни сервиса MinIO в docker-compose, ни решения о том, что и как через него сидировать (например, аватары из `identity.profiles.avatar_url`). Поля `seeds.assets`/`assetsBucket`/`assetsTimeout` в `configs/identity.yaml` по той же причине не отражены в `configuration` и игнорируются загрузчиком конфига — туда смотрит только `seeds.db`/`seeds.levels`.
* Миграции и сиды больше не применяются при старте модуля: `cmd/setup` (`make setup`) — отдельный разовый прогон перед раскаткой реплик, `cmd/bootstrap` (`initialization` модуля) только подключается к уже готовой схеме. На проде `make setup` нужно гонять как setup-job/init-container, завершающийся успешно до старта реплик приложения; docker-compose пока этот шаг не автоматизирует — `make setup` запускается вручную.
* Тесты `natsdiscovery` (включая новые `TestReapStaleRoutes_*`) и `test-integration` в целом требуют реального NATS с JetStream (`NATS_TEST_URL`) — в среде, где это писалось, живого NATS не было, поэтому они проверены сборкой/`go vet` и логикой, но не прогоном; `make nats-up && make test-integration` перед тем, как полагаться на эту ветку в проде.

---

## Лицензия

[MIT](LICENSE).
