package gateway

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go"

	"dmatech.ru/internal/pkg/config"
	"dmatech.ru/internal/pkg/logger"
	"dmatech.ru/internal/pkg/natsdiscovery"
	"dmatech.ru/internal/pkg/service"
)

const configPathEnv = "GATEWAY_CONFIG_PATH"

func init() {
	service.Register(initialization)
}

func initialization(ctx context.Context) error {
	cfg, err := config.Load[configuration](configPathEnv)
	if err != nil {
		return fmt.Errorf("gateway config: %w", err)
	}

	log := logger.NewModuleLogger(cfg.ServiceName, cfg.LogLevel)

	nc, err := service.Get[nats.Conn]()
	if err != nil {
		return fmt.Errorf("gateway: %w", err)
	}

	routes := newRouteTable(log)

	watcher, err := natsdiscovery.WatchRoutes(ctx, nc, log, cfg.Env, routes.Apply)
	if err != nil {
		return fmt.Errorf("failed to watch routes: %w", err)
	}
	service.OnShutdown(watcher.Close)

	srv := newServer(cfg.Http, cfg.Env, log, routes, nc)
	if err := srv.Start(); err != nil {
		return fmt.Errorf("failed to start http server: %w", err)
	}
	// Хуки выполняются в обратном порядке: сначала HTTP-сервер, затем watcher.
	service.OnShutdown(srv.Shutdown)

	log.Info("Модуль успешно запущен", "addr", cfg.Http.Addr)
	return nil
}
