package identity

import "time"

type configuration struct {
	NatsURL     string `yaml:"natsUrl" env:"IDENTITY_NATS_URL" env-default:"nats://nats:4222"`
	ServiceName string `yaml:"serviceName" env:"IDENTITY_SERVICE_NAME" required:"true"`
	LogLevel    string `yaml:"logLevel" env:"IDENTITY_LOG_LEVEL" env-default:"info"`
	AppVersion  string `yaml:"appVersion" env:"IDENTITY_APP_VERSION" required:"true"`

	DiscoveryBucket string        `yaml:"discovery_bucket" env:"IDENTITY_DISCOVERY_BUCKET" env-default:"service_discovery"`
	DiscoveryTTL    time.Duration `yaml:"discovery_ttl" env:"IDENTITY_DISCOVERY_TTL" env-default:"10s"`
}
