package config

import (
	"log"
	"os"

	"github.com/ilyakaznacheev/cleanenv"
)

func Load[T any](envKey string) (*T, error) {
	var cfg T

	configPath := os.Getenv(envKey)

	if configPath == "" {
		log.Printf("[%s] не задан. Модуль %T инициализируется дефолтными параметрами.", envKey, cfg)

		if err := cleanenv.ReadEnv(&cfg); err != nil {
			return nil, err
		}
		return &cfg, nil
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		log.Printf("Файл по пути %s не найден. Откат на дефолтные параметры.", configPath)

		if err := cleanenv.ReadEnv(&cfg); err != nil {
			return nil, err
		}
		return &cfg, nil
	}

	if err := cleanenv.ReadConfig(configPath, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}
