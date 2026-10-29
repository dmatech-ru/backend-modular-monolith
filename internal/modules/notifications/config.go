package notifications

import "dmatech.ru/internal/modules/notifications/sender"

type configuration struct {
	ServiceName string `yaml:"serviceName" env:"NOTIFICATIONS_SERVICE_NAME" env-required:"true"`
	LogLevel    string `yaml:"logLevel" env:"NOTIFICATIONS_LOG_LEVEL" env-default:"info"`
	AppVersion  string `yaml:"appVersion" env:"NOTIFICATIONS_APP_VERSION" env-default:"0.0.0"`
	Env         string `yaml:"env" env:"NOTIFICATIONS_ENV" env-default:"development"`

	// StreamName — имя JetStream work-queue стрима (см. internal/pkg/mq).
	// Subject'ы, которые стрим принимает, строятся из Env и канала (см.
	// subjects.go) — не настраиваются отдельно.
	StreamName string `yaml:"streamName" env:"NOTIFICATIONS_STREAM_NAME" env-default:"notifications_outbound"`
	// MaxDeliver — сколько раз повторить попытку доставки одного сообщения
	// (неудача = Sender.Send вернул ошибку) на каждый канал, прежде чем
	// JetStream перестанет пытаться.
	MaxDeliver int `yaml:"maxDeliver" env:"NOTIFICATIONS_MAX_DELIVER" env-default:"5"`

	// Email/Telegram — настройки реальных провайдеров. Пустой Host/BotToken
	// — соответствующий канал использует logSender (dev-заглушка) вместо
	// реальной отправки, см. notifications.go.
	Email    sender.EmailConfig    `yaml:"email"`
	Telegram sender.TelegramConfig `yaml:"telegram"`

	// TelegramWebhook — HTTP-вебхук, резолвящий телефон → chat_id (см.
	// telegram_webhook.go/telegram_chats.go): пользователь делится
	// контактом с ботом, вебхук запоминает привязку. Пустой Addr —
	// вебхук не поднимается, Telegram-доставка остаётся
	// нерезолвящейся (sender.unresolvedChatResolver), как раньше.
	TelegramWebhook TelegramWebhookConfig `yaml:"telegramWebhook"`
}

type TelegramWebhookConfig struct {
	// Addr — на чём слушает сам процесс (например, ":8081") — может
	// отличаться от PublicURL, если модуль стоит за reverse proxy/NAT.
	Addr string `yaml:"addr" env:"NOTIFICATIONS_TELEGRAM_WEBHOOK_ADDR"`
	// Path — путь, на который Telegram будет слать обновления.
	Path string `yaml:"path" env:"NOTIFICATIONS_TELEGRAM_WEBHOOK_PATH" env-default:"/telegram/webhook"`
	// PublicURL — полный публичный адрес вебхука, включая Path (например,
	// "https://api.example.com/telegram/webhook" — то, что снаружи видит
	// Telegram, не Addr, на котором слушает сам процесс за NAT/reverse
	// proxy). Используется модулем, чтобы самостоятельно зарегистрировать
	// вебхук в Telegram Bot API при старте (setWebhook, см.
	// registerTelegramWebhook) — отдельный разовый ручной вызов setWebhook
	// не нужен. Пусто — вебхук поднят и принимает запросы, но Telegram о
	// нём не уведомлён (нужно либо задать PublicURL, либо вызвать
	// setWebhook вручную).
	PublicURL string `yaml:"publicUrl" env:"NOTIFICATIONS_TELEGRAM_WEBHOOK_PUBLIC_URL"`
	// SecretToken — Telegram Bot API сам прикладывает его в заголовке
	// X-Telegram-Bot-Api-Secret-Token на каждый вызов вебхука, если он был
	// задан в setWebhook (см. registerTelegramWebhook) — обязателен, если
	// Addr задан: без него вебхук был бы открыт для любого, кто узнает
	// его URL (а это публичный HTTP-эндпоинт, не внутренний NATS).
	SecretToken string `yaml:"secretToken" env:"NOTIFICATIONS_TELEGRAM_WEBHOOK_SECRET"`
}
