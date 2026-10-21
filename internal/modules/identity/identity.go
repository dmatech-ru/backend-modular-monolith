package identity

import (
	"context"
	"fmt"

	"dmatech.ru/internal/pkg/config"
	"dmatech.ru/internal/pkg/logger"
	"dmatech.ru/internal/pkg/natsrpc"
	"dmatech.ru/internal/pkg/service"
)

const IDENTITY_CONFIG_PATH = "IDENTITY_CONFIG_PATH"

func init() {
	service.Register(initialization)
}

func initialization(ctx context.Context) error {
	cfg, err := config.Load[configuration](IDENTITY_CONFIG_PATH)
	if err != nil {
		return err
	}
	log := logger.NewModuleLogger(cfg.ServiceName, cfg.LogLevel)

	var descriptorBytes []byte

	nc, err := natsrpc.NewConnection(cfg.NatsURL, log)
	if err != nil {
		return fmt.Errorf("failed to connect to NATS: %w", err)
	}

	err = natsrpc.RegisterDescriptors(ctx, nc, cfg.Env, cfg.ServiceName, descriptorBytes)
	if err != nil {
		return fmt.Errorf("failed to register module descriptors: %w", err)
	}

	log.Info("Redady")
	return nil
}
