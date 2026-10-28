package identity

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/nats-io/nats.go"

	authhandler "dmatech.ru/internal/modules/identity/handlers/webapi/v1"
	identityrepo "dmatech.ru/internal/modules/identity/repository"
	authv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity"

	"dmatech.ru/internal/pkg/config"
	"dmatech.ru/internal/pkg/logger"
	"dmatech.ru/internal/pkg/natsdiscovery"
	"dmatech.ru/internal/pkg/postgres"
	"dmatech.ru/internal/pkg/protoset"
	"dmatech.ru/internal/pkg/service"
	"dmatech.ru/internal/pkg/setup"
)

const (
	configPathEnv  = "IDENTITY_CONFIG_PATH"
	postgresSchema = "identity"

	// minAntiEnumKeyBytes — минимальная длина antiEnumerationKey (см.
	// srp.FakeCredentials): 256 бит, обычный запас для HMAC-секрета.
	minAntiEnumKeyBytes = 32
)

// decodeAntiEnumKey декодирует antiEnumerationKey из конфига и проверяет его
// длину — короткий или пустой секрет делает srp.FakeCredentials предсказуемым
// и обесценивает защиту от перечисления аккаунтов.
func decodeAntiEnumKey(hexKey string) ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil {
		return nil, fmt.Errorf("antiEnumerationKey: invalid hex: %w", err)
	}
	if len(key) < minAntiEnumKeyBytes {
		return nil, fmt.Errorf("antiEnumerationKey: must be at least %d bytes, got %d", minAntiEnumKeyBytes, len(key))
	}
	return key, nil
}

func init() {
	service.Register(initialization)
	setup.Register(setupDatabase)
}

func initialization(ctx context.Context) error {
	cfg, err := config.Load[configuration](configPathEnv)
	if err != nil {
		return fmt.Errorf("identity config: %w", err)
	}
	log := logger.NewModuleLogger(cfg.ServiceName, cfg.LogLevel)

	nc, err := service.Get[nats.Conn]()
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}

	antiEnumKey, err := decodeAntiEnumKey(cfg.AntiEnumerationKey)
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}

	pool, err := postgres.Connect(ctx, postgres.Config{URL: cfg.PostgresURL, ServiceName: cfg.ServiceName}, log)
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	service.OnShutdown(func(_ context.Context) error {
		postgres.Close(pool, log)
		return nil
	})

	descriptorBytes, err := protoset.Build(authv1.File_webapi_v1_identity_auth_proto)
	if err != nil {
		return fmt.Errorf("identity: failed to build descriptor set: %w", err)
	}

	reg, err := natsdiscovery.RegisterDescriptors(ctx, nc, log, cfg.Env, cfg.ServiceName, cfg.AppVersion, descriptorBytes)
	if err != nil {
		return fmt.Errorf("failed to register module descriptors: %w", err)
	}
	service.OnShutdown(reg.Close)

	sub, err := authhandler.Subscribe(nc, log, cfg.Env, cfg.ServiceName, cfg.AppVersion, identityrepo.New(pool), antiEnumKey)
	if err != nil {
		return fmt.Errorf("failed to subscribe IdentityAuthService: %w", err)
	}
	service.OnShutdown(sub.Close)

	log.Info("Модуль успешно запущен")
	return nil
}
