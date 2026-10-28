package identity

import (
	"context"
	"fmt"

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
)

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

	sub, err := authhandler.Subscribe(nc, log, cfg.Env, cfg.ServiceName, cfg.AppVersion, identityrepo.New(pool))
	if err != nil {
		return fmt.Errorf("failed to subscribe IdentityAuthService: %w", err)
	}
	service.OnShutdown(sub.Close)

	log.Info("Модуль успешно запущен")
	return nil
}
