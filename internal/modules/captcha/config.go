package captcha

import "time"

type configuration struct {
	ServiceName string `yaml:"serviceName" env:"CAPTCHA_SERVICE_NAME" env-required:"true"`
	LogLevel    string `yaml:"logLevel" env:"CAPTCHA_LOG_LEVEL" env-default:"info"`
	AppVersion  string `yaml:"appVersion" env:"CAPTCHA_APP_VERSION" env-default:"0.0.0"`
	Env         string `yaml:"env" env:"CAPTCHA_ENV" env-default:"development"`

	// DefaultDifficulty — сложность (число ведущих нулевых бит в
	// SHA-256(challenge||nonce)), которую IssueChallenge использует, если
	// запросили difficulty <= 0 — так всегда вызывает публичный webapi-эндпоинт.
	// 18 — несколько сотен тысяч попыток, доли секунды CPU в браузере.
	DefaultDifficulty int32 `yaml:"defaultDifficulty" env:"CAPTCHA_DEFAULT_DIFFICULTY" env-default:"18"`
	// ChallengeTTL — как долго challenge действителен после выдачи.
	ChallengeTTL time.Duration `yaml:"challengeTtl" env:"CAPTCHA_CHALLENGE_TTL" env-default:"2m"`
}
