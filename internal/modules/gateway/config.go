package gateway

import "time"

type HttpConfig struct {
	Addr                  string        `yaml:"addr" env:"GATEWAY_HTTP_ADDR" env-default:":1987"`
	BaseUrl               string        `yaml:"baseURL" env:"GATEWAY_HTTP_BASE_URL" env-default:"/api"`
	Debug                 bool          `yaml:"debug" env:"GATEWAY_HTTP_DEBUG"`
	TLSCert               string        `yaml:"tlsCertFile" env:"GATEWAY_HTTP_TLS_CERT_FILE"`
	TLSKey                string        `yaml:"tlsKeyFile" env:"GATEWAY_HTTP_TLS_KEY_FILE"`
	ReadHeaderTimeout     time.Duration `yaml:"readHeaderTimeout" env:"GATEWAY_HTTP_READ_HEADER_TIMEOUT" env-default:"5s"`
	ReadTimeout           time.Duration `yaml:"readTimeout" env:"GATEWAY_HTTP_READ_TIMEOUT" env-default:"15s"`
	IdleTimeout           time.Duration `yaml:"idleTimeout" env:"GATEWAY_HTTP_IDLE_TIMEOUT" env-default:"60s"`
	RequestTimeout        time.Duration `yaml:"requestTimeout" env:"GATEWAY_HTTP_REQUEST_TIMEOUT" env-default:"5s"`
	AuthRequestTimeout    time.Duration `yaml:"authRequestTimeout" env:"GATEWAY_HTTP_AUTH_REQUEST_TIMEOUT" env-default:"10s"`
	MaxHeaderBytes        int           `yaml:"maxHeaderBytes" env:"GATEWAY_HTTP_MAX_HEADER_BYTES" env-default:"32768"`
	MaxBodyBytes          int64         `yaml:"maxBodyBytes" env:"GATEWAY_HTTP_MAX_BODY_BYTES" env-default:"1048576"`
	MaxConcurrentRequests int           `yaml:"maxConcurrentRequests" env:"GATEWAY_HTTP_MAX_CONCURRENT_REQUESTS" env-default:"1000"`
	// ShutdownDelay — пауза между снятием готовности и остановкой приёма соединений,
	// чтобы балансировщик успел убрать инстанс из ротации.
	ShutdownDelay time.Duration `yaml:"shutdownDelay" env:"GATEWAY_HTTP_SHUTDOWN_DELAY" env-default:"0s"`
}

type configuration struct {
	NatsURL     string     `yaml:"natsUrl" env:"GATEWAY_NATS_URL" env-default:"nats://nats:4222"`
	ServiceName string     `yaml:"serviceName" env:"GATEWAY_SERVICE_NAME" required:"true"`
	Http        HttpConfig `yaml:"http"`
	LogLevel    string     `yaml:"logLevel" env:"GATEWAY_LOG_LEVEL" env-default:"info"`
	Services    []string   `yaml:"services"`
}
