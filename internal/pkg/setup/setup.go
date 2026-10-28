// Package setup — реестр одноразовых задач подготовки окружения модулей
// (миграции, сиды), которые выполняются в cmd/setup отдельным прогоном перед
// раскаткой реплик. В отличие от service, который управляет жизненным циклом
// работающего инстанса (NATS, discovery, подписки), setup ничего не знает о
// рантайме — модуль регистрирует здесь только то, что нужно выполнить один
// раз до старта трафика.
package setup

import (
	"context"
	"fmt"
	"sync"
)

type Func func(ctx context.Context) error

var (
	mu    sync.Mutex
	funcs []Func
)

// Register добавляет функцию подготовки окружения модуля. Вызывается из init() модуля.
func Register(fn Func) {
	mu.Lock()
	defer mu.Unlock()
	funcs = append(funcs, fn)
}

// Run последовательно выполняет все зарегистрированные функции и
// останавливается на первой ошибке.
func Run(ctx context.Context) error {
	mu.Lock()
	fns := append([]Func(nil), funcs...)
	mu.Unlock()

	for i, fn := range fns {
		if err := fn(ctx); err != nil {
			return fmt.Errorf("setup #%d: %w", i+1, err)
		}
	}
	return nil
}
