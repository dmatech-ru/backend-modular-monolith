package gateway

import (
	"context"
	"fmt"

	"dmatech.ru/internal/pkg/config"
	"dmatech.ru/internal/pkg/logger"
	"dmatech.ru/internal/pkg/natsrpc"
	"dmatech.ru/internal/pkg/service"
)

const GATEWAY_CONFIG_PATH = "GATEWAY_CONFIG_PATH"

func init() {
	service.Register(initialization)
}

func initialization(ctx context.Context) error {

	cfg, err := config.Load[configuration](GATEWAY_CONFIG_PATH)
	if err != nil {
		return fmt.Errorf("Gateway config error: %w", err)
	}

	log := logger.NewModuleLogger("Gateway", cfg.LogLevel)

	nc, err := natsrpc.NewConnection(cfg.NatsURL, log)
	if err != nil {
		return fmt.Errorf("failed to connect to NATS: %w", err)
	}

	err = natsrpc.WatchRoutes(ctx, nc, log, cfg.Env, func(service, version string, bufBytes []byte, isDeleted bool) {
		if isDeleted {
			// router.Remove(service, version)
			return
		}

		// router.CompileAndAdd(service, version, bufBytes)
	})

	log.Info("модуль успешно запущен!")

	return nil
}
