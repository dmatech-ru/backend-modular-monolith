package identity

type configuration struct {
	ServiceName string `yaml:"serviceName" env:"IDENTITY_SERVICE_NAME" required:"true"`
	LogLevel    string `yaml:"logLevel" env:"IDENTITY_LOG_LEVEL" env-default:"info"`
	AppVersion  string `yaml:"appVersion" env:"IDENTITY_APP_VERSION" env-default:"0.0.0"`
	Env         string `yaml:"env" env:"IDENTITY_ENV" env-default:"development"`
}
