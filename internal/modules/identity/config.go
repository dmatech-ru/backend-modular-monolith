package identity

type configuration struct {
	ServiceName string `yaml:"serviceName" env:"IDENTITY_SERVICE_NAME" env-required:"true"`
	LogLevel    string `yaml:"logLevel" env:"IDENTITY_LOG_LEVEL" env-default:"info"`
	AppVersion  string `yaml:"appVersion" env:"IDENTITY_APP_VERSION" env-default:"0.0.0"`
	Env         string `yaml:"env" env:"IDENTITY_ENV" env-default:"development"`
	// PostgresURL — DSN вида postgres://user:password@host:5432/db.
	PostgresURL string      `yaml:"postgresUrl" env:"IDENTITY_POSTGRES_URL" env-required:"true"`
	Seeds       seedsConfig `yaml:"seeds"`
	// AntiEnumerationKey — секрет srp.FakeCredentials (hex, минимум 32 байта
	// = 64 hex-символа), общий для всех реплик. См. проверку длины в
	// initialization. Для прод-окружения сгенерировать свой и хранить как
	// секрет, а не в configs/identity.yaml.
	AntiEnumerationKey string `yaml:"antiEnumerationKey" env:"IDENTITY_ANTI_ENUM_KEY" env-required:"true"`

	// CaptchaServiceName/CaptchaServiceVersion — имя и версия captcha-модуля
	// (см. internal/modules/captcha) для вызова peer.captcha.v1.CaptchaService
	// по NATS (natsrpc.Client) — известная на этапе написания кода
	// зависимость, не через discovery (см. captcha_client.go).
	CaptchaServiceName    string `yaml:"captchaServiceName" env:"IDENTITY_CAPTCHA_SERVICE_NAME" env-default:"captcha-service"`
	CaptchaServiceVersion string `yaml:"captchaServiceVersion" env:"IDENTITY_CAPTCHA_SERVICE_VERSION" env-default:"0.0.0"`

	// NotificationsServiceName/NotificationsServiceVersion — то же самое
	// для peer.notifications.v1.NotificationService (см. notifications_client.go).
	NotificationsServiceName    string `yaml:"notificationsServiceName" env:"IDENTITY_NOTIFICATIONS_SERVICE_NAME" env-default:"notifications-service"`
	NotificationsServiceVersion string `yaml:"notificationsServiceVersion" env:"IDENTITY_NOTIFICATIONS_SERVICE_VERSION" env-default:"0.0.0"`

	// PasswordResetFreezeHours — см. ResetPasswordResponse.frozen_until в
	// recovery.proto: сколько часов после смены пароля через восстановление
	// доступа окно считается "заморозкой" (сегодня это только зафиксированный
	// факт в ответе, enforcement нигде не реализован — см. README
	// "Известные ограничения").
	PasswordResetFreezeHours int `yaml:"passwordResetFreezeHours" env:"IDENTITY_PASSWORD_RESET_FREEZE_HOURS" env-default:"24"`

	// AuthJWTSecret — hex-секрет (≥32 байта), которым identity подписывает
	// access-токены (internal/pkg/authctx.Issuer) — см. doc-комментарий
	// AuthJWTSecret в gateway/config.go про то, почему имя переменной без
	// обычного префикса IDENTITY_: значение должно быть побайтово
	// одинаковым в конфиге identity и в конфиге gateway.
	AuthJWTSecret string `yaml:"authJwtSecret" env:"AUTH_JWT_SECRET" env-required:"true"`
}

// seedsConfig — настройки SQL-сидов схемы identity, применяются только
// командой cmd/setup (см. setup.go), не при обычном старте модуля.
// Сиды файловых ассетов через MinIO пока не подключены — поля assets/
// assetsBucket/assetsTimeout из configs/identity.yaml осознанно не
// отражены здесь и игнорируются загрузчиком конфига.
type seedsConfig struct {
	DB     string   `yaml:"db" env:"IDENTITY_SEEDS_DB"`
	Levels []string `yaml:"levels" env:"IDENTITY_SEEDS_LEVELS" env-separator:","`
}
