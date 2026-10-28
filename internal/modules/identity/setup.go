package identity

import (
	"context"
	"fmt"

	"dmatech.ru/internal/modules/identity/migrations"
	"dmatech.ru/internal/pkg/config"
	"dmatech.ru/internal/pkg/logger"
	"dmatech.ru/internal/pkg/postgres"
)

// setupDatabase применяет миграции и SQL-сиды схемы identity. Выполняется
// один раз из cmd/setup перед раскаткой реплик — обычный старт модуля
// (initialization в identity.go) схему больше не трогает.
func setupDatabase(ctx context.Context) error {
	cfg, err := config.Load[configuration](configPathEnv)
	if err != nil {
		return fmt.Errorf("identity setup config: %w", err)
	}
	log := logger.NewModuleLogger(cfg.ServiceName, cfg.LogLevel)

	pool, err := postgres.Connect(ctx, postgres.Config{URL: cfg.PostgresURL, ServiceName: cfg.ServiceName}, log)
	if err != nil {
		return fmt.Errorf("identity setup: %w", err)
	}
	defer postgres.Close(pool, log)

	if err := postgres.Migrate(ctx, pool, log, postgresSchema, migrations.FS); err != nil {
		return fmt.Errorf("identity setup: %w", err)
	}
	if err := postgres.Seed(ctx, pool, log, postgresSchema, cfg.Seeds.DB, cfg.Seeds.Levels); err != nil {
		return fmt.Errorf("identity setup: %w", err)
	}
	return nil
}
