package identity

import (
	"context"

	"dmatech.ru/internal/pkg/config"
	"dmatech.ru/internal/pkg/logger"
	"dmatech.ru/internal/pkg/service"
)

const IDENTITY_CONFIG_PATH = "IDENTITY_CONFIG_PATH"

func init() {
	service.Register(initialization)
}

func initialization(ctx context.Context) error {
	// 1. Загружаем чистый конфиг и логгер модуля
	cfg, err := config.Load[configuration]("IDENTITY_CONFIG_PATH")
	if err != nil {
		return err
	}
	log := logger.NewModuleLogger(cfg.ServiceName, cfg.LogLevel)

	log.Info("Redady")
	return nil
}
