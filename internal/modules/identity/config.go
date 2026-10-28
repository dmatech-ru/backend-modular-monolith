package identity

type configuration struct {
	ServiceName string `yaml:"serviceName" env:"IDENTITY_SERVICE_NAME" required:"true"`
	LogLevel    string `yaml:"logLevel" env:"IDENTITY_LOG_LEVEL" env-default:"info"`
	AppVersion  string `yaml:"appVersion" env:"IDENTITY_APP_VERSION" env-default:"0.0.0"`
	Env         string `yaml:"env" env:"IDENTITY_ENV" env-default:"development"`
	// PostgresURL — DSN вида postgres://user:password@host:5432/db.
	PostgresURL string      `yaml:"postgresUrl" env:"IDENTITY_POSTGRES_URL" required:"true"`
	Seeds       seedsConfig `yaml:"seeds"`
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
