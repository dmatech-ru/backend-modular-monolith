package identity

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go"

	"dmatech.ru/internal/pkg/config"
	"dmatech.ru/internal/pkg/logger"
	"dmatech.ru/internal/pkg/natsrpc"
	"dmatech.ru/internal/pkg/service"
)

const configPathEnv = "IDENTITY_CONFIG_PATH"

func init() {
	service.Register(initialization)
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

	// TODO: подставить дескриптор, собранный buf из proto-контракта модуля
	// (например, через //go:embed), когда контракт будет описан.
	var descriptorBytes []byte
	if len(descriptorBytes) == 0 {
		log.Warn("Дескриптор маршрутов пуст: гейтвей не сможет построить маршруты модуля")
	}

	reg, err := natsrpc.RegisterDescriptors(ctx, nc, log, cfg.Env, cfg.ServiceName, cfg.AppVersion, descriptorBytes)
	if err != nil {
		return fmt.Errorf("failed to register module descriptors: %w", err)
	}
	service.OnShutdown(reg.Close)

	log.Info("Модуль успешно запущен")
	return nil
}
