package gateway

import (
	"context"
	"fmt"

	"dmatech.ru/internal/pkg/config"
	"dmatech.ru/internal/pkg/logger"
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

	log.Info("модуль успешно запущен!")

	return nil
}
