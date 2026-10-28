// Команда setup применяет миграции и сиды модулей одним прогоном перед
// раскаткой реплик. В отличие от cmd/bootstrap, она не поднимает NATS и не
// обслуживает трафик — только готовит схему БД.
//
// Набор модулей здесь не выбирается через cmd/builder: cmd/setup явно
// импортирует те модули, у которых есть что готовить (сейчас — identity).
// Новый модуль с собственной схемой добавляется таким же blank-импортом.
//
//	go run ./cmd/setup
package main

import (
	"context"
	"os"

	_ "dmatech.ru/internal/modules/identity"

	"dmatech.ru/internal/pkg/logger"
	"dmatech.ru/internal/pkg/setup"
)

func main() {
	os.Exit(run())
}

func run() int {
	log := logger.NewModuleLogger("Setup", "info")
	log.Info("Применение миграций и сидов...")

	if err := setup.Run(context.Background()); err != nil {
		log.Error("Setup завершился с ошибкой", "error", err)
		return 1
	}

	log.Info("Setup завершён успешно")
	return 0
}
