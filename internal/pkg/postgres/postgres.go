// Package postgres управляет подключением к PostgreSQL и применением
// SQL-миграций/сидов модуля.
package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	DefaultConnectTimeout    = 5 * time.Second
	DefaultHealthCheckPeriod = 30 * time.Second
)

// Config — параметры подключения к PostgreSQL.
type Config struct {
	// URL — DSN вида postgres://user:password@host:5432/db.
	URL string
	// ServiceName попадает в application_name — видно в pg_stat_activity.
	ServiceName string
	// ConnectTimeout и HealthCheckPeriod: 0 — использовать значение по умолчанию.
	ConnectTimeout    time.Duration
	HealthCheckPeriod time.Duration
}

// Connect создаёт пул соединений по cfg.URL, проверяет его ping'ом и
// возвращает готовый к работе пул.
func Connect(ctx context.Context, cfg Config, log *slog.Logger) (*pgxpool.Pool, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("postgres: url is required")
	}

	pgCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		// Ошибка парсинга может содержать DSN с паролем, поэтому в лог его не пишем.
		log.Error("PostgreSQL: некорректный url подключения")
		return nil, fmt.Errorf("postgres: некорректный url подключения")
	}

	connectTimeout := cfg.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = DefaultConnectTimeout
	}
	healthCheckPeriod := cfg.HealthCheckPeriod
	if healthCheckPeriod <= 0 {
		healthCheckPeriod = DefaultHealthCheckPeriod
	}

	pgCfg.ConnConfig.ConnectTimeout = connectTimeout
	pgCfg.HealthCheckPeriod = healthCheckPeriod
	if cfg.ServiceName != "" {
		pgCfg.ConnConfig.RuntimeParams["application_name"] = cfg.ServiceName
	}

	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(connectCtx, pgCfg)
	if err != nil {
		log.Error("PostgreSQL: не удалось создать пул", "error", err)
		return nil, fmt.Errorf("postgres: не удалось создать пул: %w", err)
	}

	if err := pool.Ping(connectCtx); err != nil {
		pool.Close()
		log.Error("PostgreSQL: не удалось подключиться",
			"host", pgCfg.ConnConfig.Host, "port", pgCfg.ConnConfig.Port, "database", pgCfg.ConnConfig.Database, "error", err)
		return nil, fmt.Errorf("postgres: не удалось подключиться (%s): %w", pgCfg.ConnConfig.Host, err)
	}

	log.Info("PostgreSQL: подключено",
		"host", pgCfg.ConnConfig.Host, "port", pgCfg.ConnConfig.Port, "database", pgCfg.ConnConfig.Database)
	return pool, nil
}

// Close закрывает пул, дожидаясь возврата выданных соединений.
func Close(pool *pgxpool.Pool, log *slog.Logger) {
	if pool == nil {
		return
	}
	pool.Close()
	log.Info("PostgreSQL: соединение закрыто")
}
