package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"dmatech.ru/internal/pkg/logger"
	"dmatech.ru/internal/pkg/service"
)

const drainTimeout = 1 * time.Second

func main() {
	log := logger.NewModuleLogger("Bootstrap", "info")
	log.Info("Инициализация платформы...")

	signal.Ignore(os.Interrupt, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	if err := service.Load(ctx); err != nil {
		log.Error("Ошибка загрузки модулей: %v", err)
	}

	log.Info("Платформа запущена и слушает события...")

	<-sigChan

	log.Info("Сигнал остановки получен. Начало плавного shutdown...")

	cancel()

	time.Sleep(drainTimeout)

	log.Info("Все ресурсы очищены. Отключаемся!")
	os.Exit(0)
}
