package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"

	"github.com/ilyakaznacheev/cleanenv"
)

// Load читает конфигурацию модуля из YAML-файла, путь к которому лежит в
// переменной окружения envKey. Переменные окружения перекрывают значения
// из файла. Если путь не задан или файла нет, конфигурация собирается только
// из окружения и значений по умолчанию.
func Load[T any](envKey string) (*T, error) {
	var cfg T

	configPath := os.Getenv(envKey)
	if configPath == "" {
		slog.Info("Config path is not set, using environment and defaults", "env", envKey, "config", fmt.Sprintf("%T", cfg))
		return readEnv(&cfg)
	}

	if _, err := os.Stat(configPath); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("config %s: %w", configPath, err)
		}
		slog.Warn("Config file not found, using environment and defaults", "path", configPath)
		return readEnv(&cfg)
	}

	if err := cleanenv.ReadConfig(configPath, &cfg); err != nil {
		return nil, fmt.Errorf("config %s: %w", configPath, err)
	}
	return &cfg, nil
}

func readEnv[T any](cfg *T) (*T, error) {
	if err := cleanenv.ReadEnv(cfg); err != nil {
		return nil, fmt.Errorf("config from env: %w", err)
	}
	return cfg, nil
}
