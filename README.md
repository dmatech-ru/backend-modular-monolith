# Backend Modular Monolith on Go

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Бэкенд-сервис на Go, спроектированный по архитектуре **модульного монолита (Modular Monolith)**: бизнес-домены изолированы друг от друга (low coupling), а деплоится единый бинарник с нужным набором модулей.

Взаимодействие модулей и динамического шлюза (Gateway) построено на **Protobuf-дескрипторах** и реактивном **Service Discovery поверх NATS JetStream KV**.

> Статус: каркас. Discovery, жизненный цикл, HTTP-сервер шлюза, proxy-мост HTTP↔NATS, SRP-вход identity (InitializeSession / CompleteAuthentication), многошаговая регистрация с подтверждением телефона/email (IdentityRegistrationService) и модуль captcha (proof-of-work, первый пример вызова одного модуля другим напрямую по NATS) поверх PostgreSQL работают целиком. Миграции и сиды применяются отдельным разовым прогоном (`cmd/setup`), не при старте реплики.

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
* **Rate limiter и circuit breaker** (`methodGuard`, `internal/modules/gateway/resilience.go`), выключены по умолчанию (`rateLimitRps`/`circuitBreakerThreshold` = 0). У них разные ключи, и это осознанно — это защита от разных угроз, не одна и та же фича с двумя именами:
  - **Лимитер — token bucket на `(IP клиента, {package.Service}/{Method})`** (`rateLimitRps`/`rateLimitBurst`): превышение — `429` без похода в NATS. Ключ включает IP, чтобы один агрессивный клиент не мог съесть общий бюджет метода и заблокировать остальных (защита от L7-абьюза/DDoS на уровне приложения). IP берётся из `r.RemoteAddr`, если явно не включено `trustProxyHeaders` (доверять `X-Forwarded-For`/`X-Real-IP`) — включать только если гейтвей реально стоит за доверенным реверс-прокси, иначе клиент тривиально обходит лимит, подделывая заголовок. Настоящий volumetric DDoS (флуд на уровне сети) этим не остановить — это не задача приложения, нужна инфраструктура перед сервером (CDN/WAF).
  - **Брейкер — общий на `{package.Service}/{Method}`, без IP** — после `circuitBreakerThreshold` подряд отказов *транспорта* (NATS no-responders/таймаут; бизнес-ошибки сервиса не считаются) открывается на `circuitBreakerCooldown`, дальше сразу `503` без попытки достучаться до NATS; затем одна пробная (half-open) попытка решает, закрыть автомат или продлить охлаждение. Он защищает сам NATS/бэкенд, когда тот уже не отвечает, — если бы он тоже был per-IP, при реальном отказе сервиса каждый новый клиент продолжал бы безнаказанно долбить мёртвый NATS.
  - Оба ключа — из запроса, то есть под контролем клиента: неактивные записи периодически выметаются, иначе атакующий мог бы раздувать карту сотнями несуществующих методов/IP.

### 5. Вызов модуля из модуля (пакеты `pow`, `natsrpc.Client`, модуль `captcha`)
Первый в проекте пример, когда один бизнес-модуль вызывает другой напрямую по NATS, а не через гейтвей — и образец на будущее, как это делать:
* **`internal/pkg/pow`** — proof-of-work (hashcash-подобный) без внешних зависимостей: `SHA-256(challenge‖nonce)` с нужным числом ведущих нулевых бит. Чистая математика, как `srp`; выдачу challenge'ей и их хранение делает модуль `captcha`.
* **Модуль `captcha`** публикует **два** proto-контракта из одного и того же `internal/modules/captcha/proto/`:
  - `webapi/v1/captcha` (пакет `webapi.captcha.v1`) — публичный: только `IssueChallenge`, браузер зовёт его через гейтвей как любой другой webapi-метод.
  - `peer/v1/captcha` (пакет `peer.captcha.v1`) — `IssueChallenge` + `VerifyChallenge`, только для вызова другими модулями по NATS. В discovery (`protoset.Build`) попадает **только** webapi-файл — гейтвей физически не узнаёт о существовании `peer.captcha.v1.CaptchaService` и не сможет его проксировать, даже по ошибке. (Назван `peer`, не `internal`: сегмент пути `internal` — зарезервированное слово в Go, ограничивающее видимость пакета.)
  - Выданные challenge'и живут в NATS JetStream KV с TTL на бакет (`captcha_challenges`) — истечение и one-shot (удаление при `VerifyChallenge`) не нуждаются в отдельной фоновой задаче.
* **`internal/pkg/natsrpc.Client`** — `connect.HTTPClient` поверх NATS request-reply (тот же протокол, которым `ServeHandler` принимает запросы на стороне модуля), симметричный клиент к тому, что уже умел `ServeHandler`. Позволяет identity вызывать `peer.captcha.v1.CaptchaService` обычным сгенерированным connect-клиентом, зная только buf-контракт captcha и её имя/версию из своего конфига (`captchaServiceName`/`captchaServiceVersion`) — без прямого Go-импорта `internal/modules/captcha` и без discovery/watch: в отличие от гейтвея (набор проксируемых модулей заранее не известен), у identity зависимость на captcha известна на этапе написания кода.

### 6. Очередь задач поверх NATS JetStream (пакет `mq`, модуль `notifications`), а не отдельный брокер
`notifications` — ещё один чисто `peer`-модуль (`internal/modules/notifications/proto/peer/v1/notifications`, без webapi-файла вообще — гейтвею здесь физически нечего проксировать): единая точка отправки сообщения по каналу из `peer.common.delivery.v1.DeliveryChannel` (email/SMS/Telegram/MAX), `destination` — контакт пользователя (email/телефон), не `chat_id` — для Telegram/MAX модуль сам резолвит чат (см. doc-комментарий `NotificationService`).

Для самой очереди осознанно **не** взят отдельный брокер (RabbitMQ и т.п.): NATS уже обязательная инфраструктура для каждого модуля, а JetStream даёт ровно ту же семантику "очереди задач с retry", только без второго контейнера/клиента/набора отказов на мониторинг:
* **`internal/pkg/mq`** — тонкий коннектор: `EnsureStream` (стрим с `WorkQueuePolicy` — сообщение покидает стрим после подтверждения, это очередь задач, не журнал событий), `Publish`, `StartConsumer` (durable pull-консьюмер, `ConsumerOptions{Durable, MaxDeliver, FilterSubject}`: ошибка `Handler` → `Nak` → JetStream сам повторит доставку, до `MaxDeliver` раз).
* **Namespace и разбивка по каналу:** subject — `{env}.notifications.{channel}` (`email`/`sms`/`telegram`/`max`, см. `channelSubject`/`channelToken` в `subjects.go`), один стрим на окружение с `Subjects: ["{env}.notifications.*"]`, но **отдельный durable-консьюмер на канал** (`FilterSubject` на свой subject). Так недоступный Telegram Bot API не блокирует обработку email, и разные окружения на одном NATS-кластере не пересекаются.
* `NotificationService.Send` принимает не готовый текст, а `template_key` (`peer.common.templates.v1.TemplateKey` — **enum, не строка**: опечатка в имени шаблона — ошибка компиляции, а не ошибка в рантайме) и `variables` для подстановки. Кладёт в очередь *уже отрендеренный* текст и отвечает сразу — это подтверждение *постановки в очередь*, не реальной доставки: провайдер (SMTP/Telegram Bot API) может быть временно недоступен, и это не должно блокировать вызывающий код (например, `IdentityRegistrationService.StartPhoneVerification`) или терять сообщение.
* **`internal/modules/notifications/template`** — рендеринг без БД и без отдельного сервиса: шаблоны — файлы, встроенные в бинарь (`go:embed`), правятся через деплой (не было требования редактировать копирайт без деплоя). `html/template` для email (автоэскейпинг переменных — защита от HTML-инъекции через значение любой из `variables`), `text/template` для SMS/Telegram/MAX. `TemplateKey` — отдельный enum (`internal/pkg/proto/peer/common/v1/templates`, в своей директории, не рядом с `delivery.proto`: go_package у buf выводится из имени proto-пакета, а не из явного alias, так что два файла с разными именами пакета в одной директории не скомпилировались бы). Лежит строго под `peer/` — это внутренняя вокабулярия `peer.notifications.v1.NotificationService`, ни один webapi-контракт её не импортирует (identity обращается к ней из Go-кода как клиент peer-сервиса, собирая `SendRequest`, а не через импорт .proto в .proto). По той же причине `peer/common/v1/delivery/delivery.proto` (`peer.common.delivery.v1.DeliveryChannel`) — тоже строго peer: у `recovery.proto` (webapi) свой собственный, отдельный enum `DeliveryChannel` с теми же значениями, который explicit-функция в Go мапит на peer-овский перед вызовом `NotificationService.Send` (см. `toPeerChannel` в `identity/handlers/webapi/v1/recovery.go`) — webapi/peer остаются строго разделены даже на уровне проверяемых типов, не только сервисов.
* **`internal/modules/notifications/sender`** — один тип на канал за интерфейсом `Sender`, получает уже рендеренные `subject`/`body`:
  - `EmailSender` — реальный SMTP relay (`net/smtp`, без сторонних зависимостей); `destination`/`subject` проверяются на `\r\n` перед сборкой заголовков — иначе было бы возможно SMTP header injection (например, подсунуть лишний `Bcc`).
  - `TelegramSender` — реальный вызов Telegram Bot API (`sendMessage`). `destination` — телефон, не `chat_id`: резолвинг телефон→чат идёт через `ChatResolver`, единственная реализация которого (`unresolvedChatResolver`) сейчас всегда отвечает "не резолвится" — отправка технически готова, но реально сработает только после вебхука, принимающего от Telegram событие "пользователь поделился контактом с ботом" (не реализован в этой итерации).
  - `LogSender` — дефолт для SMS/MAX (нет интеграции) и фоллбэк для email/Telegram, если провайдер не сконфигурирован (`email.host`/`telegram.botToken` пусты в конфиге) — пишет в лог вместо реальной отправки.
* `identity` зовёт `NotificationService.Send` через `notifications_client.go` тем же `natsrpc.Client`, которым уже зовёт captcha — `otpPhoneSender`/`otpEmailSender` реализуют интерфейсы `PhoneSender`/`EmailSender`, объявленные в хендлере, передавая `template_key = TEMPLATE_KEY_OTP_CODE` и переменную `code`.

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
│   │   ├── captcha/     # Proof-of-work: webapi (IssueChallenge) + peer (+VerifyChallenge) контракты, KV-хранилище challenge'ей
│   │   ├── gateway/     # Реактивный шлюз (HTTP + таблица маршрутов)
│   │   ├── identity/    # Модуль аутентификации (SRP + PostgreSQL, регистрируется в discovery)
│   │   │   ├── migrations/ # SQL-миграции схемы identity (go:embed)
│   │   │   ├── repository/ # Репозиторий identity поверх PostgreSQL (auth, challenges, users, регистрация, восстановление)
│   │   │   └── setup.go    # Миграции + сиды identity, вызывается из cmd/setup (не из initialization)
│   │   └── notifications/ # Peer-только: очередь отправки (email/SMS/Telegram/MAX) поверх NATS JetStream
│   │       ├── sender/     # Sender на канал: EmailSender (SMTP), TelegramSender (Bot API), LogSender (dev-заглушка)
│   │       └── template/   # Рендеринг шаблонов (go:embed, html/template + text/template), локализованных (templates/<key>/<locale>/), общая email-вёрстка в email_layout.tmpl ("extends" через Template.Clone), без БД
│   └── pkg/
│       ├── config/        # Дженерик-загрузчик конфигураций (YAML + env)
│       ├── httpserver/     # Переиспользуемый HTTP-сервер (таймауты, лимиты, graceful shutdown)
│       ├── locale/        # Перенос локали запроса через connect-интерцепторы (X-Locale/Accept-Language → context.Context → заголовок исходящего вызова)
│       ├── logger/        # ColorHandler для slog (dev) / JSON (production)
│       ├── minio/         # Подключение к MinIO/S3 и сидирование файловых ассетов (пока не подключён ни к одному модулю)
│       ├── mq/            # Durable work-queue поверх NATS JetStream (стримы + pull-консьюмеры с retry)
│       ├── natsconn/      # Подключение к NATS и Drain при остановке
│       ├── natsdiscovery/ # Service discovery поверх NATS JetStream KV
│       ├── natsrpc/       # RPC-транспорт поверх NATS: сервер (ServeHandler) и клиент (Client) для вызовов между модулями
│       ├── postgres/      # Подключение к PostgreSQL, миграции и сиды схемы модуля
│       ├── pow/           # Proof-of-work challenge/verify (hashcash-подобный), без внешних зависимостей
│       ├── protoset/      # Сборка protobuf.FileDescriptorSet из дескриптора + импортов
│       ├── srp/           # SRP-6a: серверная и клиентская математика (RFC 5054, SHA-256)
│       ├── service/       # Реестр модулей рантайма, хуки остановки, общие зависимости
│       ├── setup/         # Реестр одноразовых задач подготовки окружения (миграции/сиды) для cmd/setup
│       └── validation/    # connect-интерцептор над protovalidate: buf.validate.* правила → единый формат ошибок с ключом (webapi/v1/common/errors.proto)
├── rest-examples/       # Примеры HTTP-запросов (humao.rest-client) к gateway/identity/captcha
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
| `GATEWAY_CONFIG_PATH`, `IDENTITY_CONFIG_PATH`, `CAPTCHA_CONFIG_PATH`, `NOTIFICATIONS_CONFIG_PATH` | — | Пути к YAML модулей; переменные `GATEWAY_*` / `IDENTITY_*` / `CAPTCHA_*` / `NOTIFICATIONS_*` перекрывают значения из файла |
| `IDENTITY_POSTGRES_URL` | из `configs/identity.yaml` | DSN PostgreSQL для identity, схема `identity` |
| `IDENTITY_SEEDS_DB` | из `configs/identity.yaml` (`seeds.db`) | Каталог SQL-сидов identity для `cmd/setup` |
| `IDENTITY_SEEDS_LEVELS` | из `configs/identity.yaml` (`seeds.levels`) | Уровни сидов по порядку применения, через запятую (например `shared,development`) |
| `IDENTITY_ANTI_ENUM_KEY` | из `configs/identity.yaml` | Hex-секрет (≥32 байта) для `srp.FakeCredentials` — защита `InitializeSession` от перечисления аккаунтов; должен совпадать на всех репликах |
| `IDENTITY_CAPTCHA_SERVICE_NAME`, `IDENTITY_CAPTCHA_SERVICE_VERSION` | `captcha-service`, `0.0.0` | Имя/версия captcha-модуля для вызова `peer.captcha.v1.CaptchaService` по NATS (должны совпадать с `serviceName`/`appVersion` в `configs/captcha.yaml`) |
| `IDENTITY_NOTIFICATIONS_SERVICE_NAME`, `IDENTITY_NOTIFICATIONS_SERVICE_VERSION` | `notifications-service`, `0.0.0` | То же самое для `peer.notifications.v1.NotificationService` (должны совпадать с `configs/notifications.yaml`) |
| `IDENTITY_PASSWORD_RESET_FREEZE_HOURS` | `24` | Сколько часов после `IdentityRecoveryService.ResetPassword` отдаётся в ответе как `frozen_until` (зафиксированный факт, enforcement не реализован — см. "Известные ограничения") |
| `CAPTCHA_DEFAULT_DIFFICULTY` | из `configs/captcha.yaml` | Сложность PoW по умолчанию (число ведущих нулевых бит) для `webapi.captcha.v1.CaptchaService.IssueChallenge` |
| `CAPTCHA_CHALLENGE_TTL` | из `configs/captcha.yaml` | Срок жизни выданного challenge'а |
| `NOTIFICATIONS_STREAM_NAME` | из `configs/notifications.yaml` | Имя JetStream work-queue стрима очереди отправки |
| `NOTIFICATIONS_MAX_DELIVER` | из `configs/notifications.yaml` | Сколько раз повторить попытку доставки одного сообщения, прежде чем JetStream перестанет пытаться |
| `NOTIFICATIONS_EMAIL_SMTP_HOST`, `_PORT`, `_USERNAME`, `_PASSWORD`, `NOTIFICATIONS_EMAIL_FROM` | — | Настройки SMTP relay; пустой `HOST` — email идёт в `LogSender`, а не отправляется реально |
| `NOTIFICATIONS_TELEGRAM_BOT_TOKEN` | — | Токен Telegram Bot API; пусто — Telegram идёт в `LogSender` |

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
    descriptorBytes, err := protoset.Build(authv1.File_webapi_v1_identity_auth_auth_proto)
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

## 📈 Если потребуется больший масштаб

Текущая схема публикует `FileDescriptorSet` в NATS KV **на ключ модуля**
(`{env}.{service}.{version}`, см. `protoset.Build` и п.3) — размер зависит
от proto-сёрфейса конкретного модуля, а не от суммарного числа RPC во всей
системе: 1000 методов, размазанные по полусотне модулей, — это десятки-сотни
КБ на ключ, не один гигантский блоб. Для умеренного числа модулей умеренного
размера (типичный модульный монолит) это нормально работает в лимитах NATS
JetStream KV по умолчанию.

Если один модуль разрастётся до огромного proto-сёрфейса (или дескрипторы
понадобятся внешним потребителям, не только этому гейтвею), правильный
следующий шаг — не заливать бинарники дескрипторов в MinIO/S3 самодельным
кодом (версионирование, инвалидация кэша, авторизация к бакету — всё ручное
и чужое для домена), а подключить настоящий **Buf Schema Registry** (свой
инстанс или buf.build): он уже умеет версионирование, compatibility-checks и
резолвинг зависимостей между командами. NATS KV при этом продолжает хранить
только лёгкий список маршрутов (`сервис.Method → модуль`), а не сами
дескрипторы.

---

## Известные ограничения

* Гейтвей находит владеющий модуль по дескриптору и пересылает запрос через NATS (`{env}.{serviceName}.{version}.rpc.{methodPath}`, см. `natsrpc.RPCSubject`/`natsrpc.ServeHandler`); `identity` (`IdentityAuthService` + `IdentityRegistrationService` + `IdentityRecoveryService`, один общий mux и одна подписка, см. `authhandler.Subscribe`) и `captcha` подписаны и реально отвечают.
* **SRP-вход identity** (`internal/pkg/srp`, группа 2048 бит из RFC 5054 Appendix A.3 с SHA-256 вместо SHA-1 — своя схема, совместимость с внешними SRP-клиентами не требовалась): `InitializeSession` → `CompleteAuthentication`. Пароль не передаётся на сервер ни разу — сервер хранит только `salt`/`verifier`, вычисленные клиентом. `InitializeSession` защищён от перечисления аккаунтов: для несуществующего identifier сервер без обращения к базе отвечает детерминированными фейковыми `salt`/`verifier` (`srp.FakeCredentials`, ключ — `antiEnumerationKey`), неотличимыми по форме и коду ответа от настоящего аккаунта; пройти второй шаг с ними невозможно ни при каком пароле. Challenge'и второго шага (`identity.auth_challenges`) чистятся попутно при каждой новой `InitializeSession` (не нужна отдельная фоновая задача).
* **Регистрация** (`IdentityRegistrationService`, `registration.proto`) — многошаговая: `CheckIdentifier` → `StartPhoneVerification` (телефон + согласие на обработку персональных данных, 152-ФЗ) → `ConfirmPhoneVerification` → `StartEmailVerification` → `ConfirmEmailVerification` → `CompleteRegistration` (пароль — как и раньше, `salt`/`verifier` с клиента). Коды подтверждения хранятся только хешем (SHA-256), лимит попыток на код — 5 (`RESOURCE_EXHAUSTED` дальше), сессия регистрации — в `identity.registration_sessions` с общим TTL 30 минут. Осознанные упрощения:
  - `CheckIdentifier`, в отличие от `InitializeSession`, **намеренно** раскрывает, существует ли аккаунт — это нужно, чтобы решить "вести на вход или на регистрацию". Ограничено рейт-лимитом гейтвея (`methodGuard`) и PoW-защитой (см. ниже), но это осознанный трейд-офф UX/приватность, а не недосмотр.
  - PoW (`captcha_client.go`, модуль `captcha`) требуется **всегда** на `CheckIdentifier`/`StartPhoneVerification`/`StartEmailVerification`, а не адаптивно после N неудачных попыток, как задумано в `registration.proto` — отслеживание попыток по IP/identifier отдельным хранилищем не реализовано в этой итерации. Для одного легитимного пользователя разница не заметна (решение — доли секунды CPU), но от задуманного "невидимо, пока нет абьюза" текущая реализация отступает.
  - Отправка кода подтверждения идёт через `peer.notifications.v1.NotificationService.Send` (`notifications_client.go`) с `template_key = TEMPLATE_KEY_OTP_CODE` — identity не формирует текст сообщения сама, только передаёт переменную `code` (см. `notifications` ниже про реальность самой отправки по каждому каналу).
  - Новый пользователь получает статус `pending` (значение по умолчанию) — даже после подтверждения телефона и email ничего не переводит его в `active`.
  - `StartPhoneVerification`/`StartEmailVerification` с тем же номером/почтой каждый раз создают новую сессию регистрации, а не переиспользуют существующую (так проще, и это безопасно — уникальность всё равно гарантируется на `CompleteRegistration`), хотя комментарий в proto изначально предполагал переотправку в рамках одной сессии.
* **Восстановление доступа** (`IdentityRecoveryService`, `recovery.proto`) реализовано полностью: `InitiateRecovery` (identifier → доступные каналы, под PoW, та же "утечка по делу" и те же ограничители, что у `CheckIdentifier`) → `SendRecoveryCode` (канал из перечисленных `InitiateRecovery`, иначе `INVALID_ARGUMENT`) → `ConfirmRecoveryCode` → `ResetPassword` (пароль — `salt`/`verifier` с клиента, как и везде в этом контракте). Сессия — `identity.recovery_sessions`, снимок `email`/`phone` аккаунта на момент `InitiateRecovery` (не переопрашивается повторно), код — хешем (SHA-256), лимит попыток — 5, TTL сессии — 30 минут, TTL кода — 10 минут. `ResetPassword` фиксирует `identity.auth.password_changed_at` и возвращает `frozen_until` (`passwordResetFreezeHours`, по умолчанию 24ч) — только зафиксированный факт, enforcement где-либо в проекте не реализован (сессий/токенов пока не существует вовсе).
  - Доступные каналы считаются от того, что реально есть на аккаунте: `EMAIL`/`SMS` — если есть email/телефон, `TELEGRAM` — на тех же условиях, что `SMS` (доставка через бота, без привязки заранее, см. doc-комментарий сервиса), но без `masked_destination` (адрес не хранится заранее). `MAX` никогда не предлагается — в схеме пока нет даже колонки, по которой можно было бы решить, привязан ли аккаунт к нему (см. `DELIVERY_CHANNEL_MAX` в `peer/common/v1/delivery/delivery.proto`). Канал в `recovery.proto` — свой webapi-локальный `DeliveryChannel`, не `peer.common.delivery.v1.DeliveryChannel` (см. `internal/modules/notifications/template` ниже про то, почему эти два enum'а разделены).
  - Отправка — тот же путь, что у регистрации (`peer.notifications.v1.NotificationService.Send`), но отдельным `template_key = TEMPLATE_KEY_PASSWORD_RESET_CODE` (`recoveryCodeSender` в `notifications_client.go`) и каналом по выбору пользователя, а не фиксированным заранее. Отправка в `TELEGRAM` сегодня гарантированно упадёт `CodeInternal` — `identity` здесь ничего не стабит сама, просто зовёт `NotificationService` и транслирует его ошибку: единственная заглушка на этом пути (`unresolvedChatResolver`) живёт в `notifications`, см. ниже.
* **`notifications`** — `identity` теперь реально зовёт `NotificationService.Send` (не свою dev-заглушку, как раньше), с `template_key` + `variables`, а не готовым текстом — текст собирает сам `notifications` (`internal/modules/notifications/template`, `embed.FS`, `html/template` для email с автоэскейпингом переменных, `text/template` для SMS/Telegram/MAX). `EmailSender` (SMTP relay) и `TelegramSender` (Bot API `sendMessage`) реально реализованы и покрыты тестами, но:
  - у `TelegramSender` резолвинг телефон→chat_id (`ChatResolver`) не реализован вообще — единственная реализация всегда отвечает "не резолвится" (`unresolvedChatResolver`). Нужен вебхук, принимающий от Telegram событие "пользователь поделился контактом с ботом" (см. doc-комментарий `NotificationService` в `notifications.proto`), и хранилище телефон→chat_id — ни того, ни другого в этой итерации нет, поэтому реальная отправка в Telegram пока не сработает ни при каких настройках, хоть шаблон для неё и заведён.
  - SMS/MAX — всегда `LogSender` для MAX (интеграции нет вовсе); SMS — тоже `LogSender`, пока нет настроенного СМС-гейтвея (в этой итерации его нет вообще, в отличие от email/Telegram).
  - Шаблоны — файлы, встроенные в бинарь (`go:embed`), не в БД: нет требования редактировать копирайт без деплоя. Сейчас их два — `otp_code` (регистрация) и `password_reset_code` (восстановление доступа), оба для email/SMS/Telegram, каждый в двух локалях (`ru`/`en`, файлы `templates/<key>/<locale>/<файл>`) — новый шаблон = новое значение `peer.common.templates.v1.TemplateKey` + файлы в `internal/modules/notifications/template/templates/`; новая локаль = новая поддиректория + добавление в `locale.Supported`.
  - Вёрстка email — не копия в каждом `email.body.tmpl`: `email.body.tmpl` объявляет только блок `{{define "content"}}...{{end}}` (переведённый текст письма), общая обёртка (`<!DOCTYPE html>`, `<html lang="...">` — локаль передаётся не только для выбора файла, но и в сам шаблон полем `Locale`) — одна на все key×locale, в `internal/modules/notifications/template/email_layout.tmpl`. Механика — обычное для `html/template` "extends"-наследование через `Template.Clone()`: при старте модуля общая вёрстка клонируется под каждый `email.body.tmpl`, и в клон парсится его `content`; `Render` исполняет именованный шаблон `"email_layout"` клона, который сам зовёт `{{template "content" .}}`.
* **Локализация** (`internal/pkg/locale`) — два connect-интерцептора переносят локаль запроса через границы NATS/connect-вызовов: `ServerInterceptor` на входе в модуль читает `X-Locale` (если его нет — стандартный `Accept-Language` с учётом q-весов), кладёт в `context.Context` и эхом возвращает фактически применённую локаль в заголовке ответа (если запрошенная не из `Supported`, клиент иначе не узнал бы, что сервер молча взял `Default`); `ClientInterceptor` на исходящем вызове другого модуля переносит её обратно в заголовок (без этого шага локаль обрывалась бы на первом же межмодульном вызове — сам NATS-транспорт переносит заголовки байт-в-байт, но не копирует `context.Context` в них). Подключены на входе всех webapi-сервисов `identity` и peer-сервиса `notifications`, и на исходящем вызове `identity → notifications`. Локаль **не** используется для перевода текста ошибок API (см. ниже) — только для выбора варианта шаблона `notifications` (это единственное место, где получатель — не клиентское приложение, а живой человек, которому нужен готовый текст на его языке). `captcha` эту пару интерцепторов не подключает — у него нет ничего, что зависело бы от локали (ни шаблонов, ни переводимых бизнес-ошибок: `IssueChallenge` либо успешен, либо `CodeInternal`, `VerifyChallenge` возвращает `valid=false` как обычный ответ, не ошибку). Локаль живёт только в рамках запроса — если когда-нибудь понадобится уведомление, инициированное не входящим запросом (крон, вебхук), взять для него локаль будет не из чего: придётся хранить предпочтение языка на аккаунте отдельно. Строить это сейчас, без реального кейса, намеренно не стали.
* **Единый формат ошибок и статическая валидация** (`internal/pkg/validation`, buf.validate/protovalidate) — правила `(buf.validate.field).cel { id, message, expression }` объявлены прямо в `.proto`: все статические проверки формы во всех трёх webapi-сервисах `identity` (`auth.proto`/`registration.proto`/`recovery.proto`) и в `peer.captcha.v1.CaptchaService.VerifyChallengeRequest` (`captcha.proto`); `validation.Interceptor` подключён на входе всех трёх webapi-сервисов `identity` и на входе обоих контрактов `captcha` (webapi и peer) — проверяет форму запроса (непустые обязательные поля, `personal_data_consent == true` и т.п.) ДО вызова метода хендлера и переводит нарушения в `common.ErrorPayload`/`common.FieldError` с `key` = `id` правила (например `identity.identifier.required`) — тот самый стабильный, непереведённый идентификатор ошибки, по которому клиент сам подбирает локализованную строку; `message` — фиксированный текст на одном языке для логов/отладки, backend его не переводит и клиент не должен его показывать. Один и тот же ключ переиспользуется для одного и того же смысла поля во всех RPC, где оно встречается (например, `identity.identifier.required` — у `InitializeSession`, `CheckIdentifier` и `InitiateRecovery`), чтобы у клиента был один перевод, не по одному на каждый метод. Все ручные ошибки `identity` (сессия не найдена/просрочена, слишком много попыток, неверный код, телефон/email уже зарегистрирован и т.п. — `keyedError`/`fieldError` в хендлерах) тоже несут `key` в этом же формате — мигрированы полностью, не только проверки формы. Осознанные границы:
  - `buf.validate` — только статическая форма запроса; бизнес-правила (существование сессии, доступность канала, попытки кода) остаются ручными `keyedError`/`fieldError` в хендлерах — это состояние системы, а не форма запроса, `buf.validate` для этого не подходит;
  - `verifier` (SRP) проверяется в Go (`srp.ValidVerifier`), не правилом `buf.validate` — граница размера зависит от SRP-группы (`internal/pkg/srp`), дублировать её магическим числом в `.proto` не стоило;
  - `connect.NewError(CodeInternal, err)` (неожиданные сбои — БД недоступна и т.п.) `key` не несёт и не должен: это не состояние, которое клиент переводит и показывает пользователю, а поломка, которую не из чего локализовать.
* Пакет `internal/pkg/minio` адаптирован для работы в этом проекте, но пока не подключён ни к одному модулю: нет ни сервиса MinIO в docker-compose, ни решения о том, что и как через него сидировать (например, аватары из `identity.profiles.avatar_url`). Поля `seeds.assets`/`assetsBucket`/`assetsTimeout` в `configs/identity.yaml` по той же причине не отражены в `configuration` и игнорируются загрузчиком конфига — туда смотрит только `seeds.db`/`seeds.levels`.
* Миграции и сиды больше не применяются при старте модуля: `cmd/setup` (`make setup`) — отдельный разовый прогон перед раскаткой реплик, `cmd/bootstrap` (`initialization` модуля) только подключается к уже готовой схеме. На проде `make setup` нужно гонять как setup-job/init-container, завершающийся успешно до старта реплик приложения; docker-compose пока этот шаг не автоматизирует — `make setup` запускается вручную.
* Тесты `natsdiscovery`, `natsrpc.Client` и `captcha`'s KV-хранилище (`TestKVStore_*`) и `test-integration` в целом требуют реального NATS с JetStream (`NATS_TEST_URL`) — в среде, где это писалось, живого NATS не было, поэтому они проверены сборкой/`go vet` и логикой, но не прогоном; `make nats-up && make test-integration` перед тем, как полагаться на эту ветку в проде.

---

## Лицензия

[MIT](LICENSE).
