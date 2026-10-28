package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ilyakaznacheev/cleanenv"
	"github.com/nats-io/nats.go"

	"dmatech.ru/internal/pkg/logger"
	"dmatech.ru/internal/pkg/natsrpc"
	"dmatech.ru/internal/pkg/service"
)

type configuration struct {
	NatsURL         string        `env:"NATS_URL" env-default:"nats://nats:4222"`
	LogLevel        string        `env:"LOG_LEVEL" env-default:"info"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" env-default:"15s"`
}

func main() {
	os.Exit(run())
}

func run() int {
	var cfg configuration
	if err := cleanenv.ReadEnv(&cfg); err != nil {
		logger.NewModuleLogger("Bootstrap", "info").Error("Ошибка чтения конфигурации", "error", err)
		return 1
	}

	log := logger.NewModuleLogger("Bootstrap", cfg.LogLevel)
	log.Info("Инициализация платформы...")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	nc, err := natsrpc.NewConnection(cfg.NatsURL, log)
	if err != nil {
		log.Error("Не удалось подключиться к NATS", "url", cfg.NatsURL, "error", err)
		return 1
	}
	service.Provide[nats.Conn](nc)

	exitCode := 0
	if err := service.Load(ctx); err != nil {
		log.Error("Ошибка загрузки модулей", "error", err)
		exitCode = 1
	} else {
		log.Info("Платформа запущена и слушает события...")
		<-ctx.Done()
		log.Info("Сигнал остановки получен. Начало плавного shutdown...")
	}
	stop() // повторный сигнал завершит процесс сразу

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := service.Shutdown(shutdownCtx); err != nil {
		log.Error("Ошибки при остановке модулей", "error", err)
		exitCode = 1
	}
	if err := natsrpc.Drain(shutdownCtx, nc); err != nil {
		log.Error("Ошибка закрытия соединения с NATS", "error", err)
		exitCode = 1
	}

	log.Info("Все ресурсы очищены. Отключаемся!")
	return exitCode
}
