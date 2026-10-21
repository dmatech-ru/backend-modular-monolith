package identity

type configuration struct {
	NatsURL     string `yaml:"natsUrl" env:"IDENTITY_NATS_URL" env-default:"nats://nats:4222"`
	ServiceName string `yaml:"serviceName" env:"IDENTITY_SERVICE_NAME" required:"true"`
	LogLevel    string `yaml:"logLevel" env:"IDENTITY_LOG_LEVEL" env-default:"info"`
	AppVersion  string `yaml:"appVersion" env:"IDENTITY_APP_VERSION" required:"true"`
	Env         string `yaml:"env" env:"IDENTITY_ENV" env-default:"development"`
}
