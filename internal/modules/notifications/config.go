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
}
