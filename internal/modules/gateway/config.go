package gateway

import "time"

type HttpConfig struct {
	Addr              string        `yaml:"addr" env:"GATEWAY_HTTP_ADDR" env-default:":1987"`
	BaseUrl           string        `yaml:"baseURL" env:"GATEWAY_HTTP_BASE_URL" env-default:"/api"`
	Debug             bool          `yaml:"debug" env:"GATEWAY_HTTP_DEBUG"` // логировать каждый запрос
	TLSCert           string        `yaml:"tlsCertFile" env:"GATEWAY_HTTP_TLS_CERT_FILE"`
	TLSKey            string        `yaml:"tlsKeyFile" env:"GATEWAY_HTTP_TLS_KEY_FILE"`
	ReadHeaderTimeout time.Duration `yaml:"readHeaderTimeout" env:"GATEWAY_HTTP_READ_HEADER_TIMEOUT" env-default:"5s"`
	ReadTimeout       time.Duration `yaml:"readTimeout" env:"GATEWAY_HTTP_READ_TIMEOUT" env-default:"15s"`
	IdleTimeout       time.Duration `yaml:"idleTimeout" env:"GATEWAY_HTTP_IDLE_TIMEOUT" env-default:"60s"`
	// RequestTimeout — таймаут для "статических" эндпоинтов (healthz/readyz/
	// _routes), которые отвечают локально и быстро.
	RequestTimeout time.Duration `yaml:"requestTimeout" env:"GATEWAY_HTTP_REQUEST_TIMEOUT" env-default:"5s"`
	// ProxyRequestTimeout — отдельный, обычно больший таймаут ожидания ответа
	// модуля через NATS при проксировании запроса (см. handleProxy). Не связан
	// с RequestTimeout: латентность модуля не предсказать по латентности
	// health-чеков, поэтому таймаут настраивается отдельно.
	ProxyRequestTimeout   time.Duration `yaml:"proxyRequestTimeout" env:"GATEWAY_HTTP_PROXY_REQUEST_TIMEOUT" env-default:"10s"`
	MaxHeaderBytes        int           `yaml:"maxHeaderBytes" env:"GATEWAY_HTTP_MAX_HEADER_BYTES" env-default:"32768"`
	MaxBodyBytes          int64         `yaml:"maxBodyBytes" env:"GATEWAY_HTTP_MAX_BODY_BYTES" env-default:"1048576"`
	MaxConcurrentRequests int           `yaml:"maxConcurrentRequests" env:"GATEWAY_HTTP_MAX_CONCURRENT_REQUESTS" env-default:"1000"`
	// ShutdownDelay — пауза между снятием готовности и остановкой приёма соединений,
	// чтобы балансировщик успел убрать инстанс из ротации.
	ShutdownDelay time.Duration `yaml:"shutdownDelay" env:"GATEWAY_HTTP_SHUTDOWN_DELAY" env-default:"0s"`

	// TrustProxyHeaders — доверять X-Forwarded-For/X-Real-IP при определении
	// IP клиента для лимитера (см. RateLimitRPS). Включать только если
	// гейтвей реально стоит за доверенным реверс-прокси/балансировщиком,
	// который сам подставляет эти заголовки и не пропускает их от клиента
	// как есть — иначе любой клиент подделает заголовок и обойдёт лимит
	// по IP просто подставляя новый адрес на каждый запрос. По умолчанию
	// выключено: берётся реальный адрес TCP-соединения (r.RemoteAddr).
	TrustProxyHeaders bool `yaml:"trustProxyHeaders" env:"GATEWAY_HTTP_TRUST_PROXY_HEADERS" env-default:"false"`

	// RateLimitRPS — лимит запросов в секунду на каждый "/{package.Service}/{Method}"
	// на каждый IP клиента независимо (не суммарно по шлюзу и не на всех
	// клиентов сразу — иначе один агрессивный клиент съедал бы общий бюджет
	// метода и блокировал остальных). 0 (по умолчанию) — лимитер выключен.
	RateLimitRPS float64 `yaml:"rateLimitRps" env:"GATEWAY_HTTP_RATE_LIMIT_RPS" env-default:"0"`
	// RateLimitBurst — размер всплеска token bucket сверх RateLimitRPS.
	// Игнорируется, если RateLimitRPS <= 0.
	RateLimitBurst int `yaml:"rateLimitBurst" env:"GATEWAY_HTTP_RATE_LIMIT_BURST" env-default:"0"`

	// CircuitBreakerThreshold — после скольких подряд отказов транспорта
	// (NATS no responders/таймаут) на конкретный "/{package.Service}/{Method}"
	// открывается автомат: дальнейшие запросы получают 503 сразу, без похода
	// в NATS, до истечения CircuitBreakerCooldown. В отличие от RateLimitRPS
	// — общий для всех клиентов, не по IP: это защита самого NATS/бэкенда от
	// лавины, а не персональный лимит одному клиенту. 0 (по умолчанию) — брейкер выключен.
	CircuitBreakerThreshold int `yaml:"circuitBreakerThreshold" env:"GATEWAY_HTTP_CIRCUIT_BREAKER_THRESHOLD" env-default:"0"`
	// CircuitBreakerCooldown — сколько automat остаётся открытым, прежде чем
	// пропустить одну пробную попытку (half-open). Игнорируется, если
	// CircuitBreakerThreshold <= 0.
	CircuitBreakerCooldown time.Duration `yaml:"circuitBreakerCooldown" env:"GATEWAY_HTTP_CIRCUIT_BREAKER_COOLDOWN" env-default:"30s"`
}

type configuration struct {
	ServiceName string     `yaml:"serviceName" env:"GATEWAY_SERVICE_NAME" env-required:"true"`
	Http        HttpConfig `yaml:"http"`
	LogLevel    string     `yaml:"logLevel" env:"GATEWAY_LOG_LEVEL" env-default:"info"`
	Env         string     `yaml:"env" env:"GATEWAY_ENV" env-default:"development"`

	// AuthJWTSecret — hex-секрет (≥32 байта), которым identity подписывает
	// access-токены (internal/pkg/authctx.Issuer) и которым гейтвей их
	// проверяет (authctx.Verifier) перед тем, как проксировать непубличный
	// запрос. Единственный секрет в проекте, общий не с одним конкретным
	// модулем (как antiEnumerationKey — только identity), а с gateway —
	// поэтому имя переменной без обычного префикса GATEWAY_/IDENTITY_, той
	// же категории, что NATS_URL/APP_ENV: значение должно быть побайтово
	// одинаковым в конфиге identity и в этом конфиге.
	AuthJWTSecret string `yaml:"authJwtSecret" env:"AUTH_JWT_SECRET" env-required:"true"`
}
