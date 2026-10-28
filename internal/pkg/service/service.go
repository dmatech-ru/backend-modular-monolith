// Package service — реестр модулей монолита: их инициализация, хуки
// остановки и общие зависимости (например, соединение с NATS).
package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
)

type InitFunc func(ctx context.Context) error

type ShutdownFunc func(ctx context.Context) error

var (
	mu        sync.Mutex
	modules   []InitFunc
	shutdowns []ShutdownFunc
	deps      = map[reflect.Type]any{}
)

// Register добавляет функцию инициализации модуля. Вызывается из init() модуля.
func Register(initFn InitFunc) {
	mu.Lock()
	defer mu.Unlock()
	modules = append(modules, initFn)
}

// OnShutdown добавляет хук остановки. Хуки выполняются в обратном порядке регистрации.
func OnShutdown(fn ShutdownFunc) {
	mu.Lock()
	defer mu.Unlock()
	shutdowns = append(shutdowns, fn)
}

// Load последовательно инициализирует модули и останавливается на первой ошибке.
// Хуки уже инициализированных модулей остаются зарегистрированными, поэтому
// после ошибки вызывающий код должен выполнить Shutdown.
func Load(ctx context.Context) error {
	mu.Lock()
	initFns := append([]InitFunc(nil), modules...)
	mu.Unlock()

	for i, initFn := range initFns {
		if err := initFn(ctx); err != nil {
			return fmt.Errorf("module #%d: %w", i+1, err)
		}
	}
	return nil
}

// Shutdown выполняет хуки остановки в обратном порядке и возвращает все ошибки.
func Shutdown(ctx context.Context) error {
	mu.Lock()
	hooks := shutdowns
	shutdowns = nil
	mu.Unlock()

	var errs []error
	for i := len(hooks) - 1; i >= 0; i-- {
		if err := hooks[i](ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Provide публикует общую зависимость для модулей.
func Provide[T any](v *T) {
	mu.Lock()
	defer mu.Unlock()
	deps[reflect.TypeFor[T]()] = v
}

// Get возвращает зависимость, опубликованную через Provide.
func Get[T any]() (*T, error) {
	mu.Lock()
	defer mu.Unlock()
	v, ok := deps[reflect.TypeFor[T]()]
	if !ok {
		return nil, fmt.Errorf("dependency %s is not provided", reflect.TypeFor[T]())
	}
	return v.(*T), nil
}
