package natsrpc

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"sync"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// RouteUpdate — изменение маршрута сервиса, доставляемое в колбэк WatchRoutes.
type RouteUpdate struct {
	ServiceName string
	Version     string // санитизированная версия, например "1_0_0"
	Descriptor  []byte // сырые байты Protobuf-дескриптора (buf)
	IsDeleted   bool   // true, если последняя нода версии ушла и маршрут удалён
}

// RouteWatcher — подписка на изменения маршрутов. Close останавливает её
// и дожидается завершения обработчика.
type RouteWatcher struct {
	watcher   jetstream.KeyWatcher
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// WatchRoutes подписывается на маршруты окружения env. Сначала колбэк получает
// все существующие маршруты, затем — изменения. Повторная публикация
// идентичного дескриптора (рестарт реплики) отфильтровывается по SHA-256.
// Колбэк вызывается последовательно из одной горутины. Подписка работает до
// вызова RouteWatcher.Close.
func WatchRoutes(
	ctx context.Context,
	nc *nats.Conn,
	log *slog.Logger,
	env string,
	onUpdate func(RouteUpdate),
) (*RouteWatcher, error) {
	if err := requireTokens(map[string]string{"env": env}); err != nil {
		return nil, err
	}

	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("failed to init jetstream: %w", err)
	}

	// Бакет создаётся и здесь, чтобы гейтвей не зависел от порядка старта сервисов.
	kv, err := routesKV(ctx, js)
	if err != nil {
		return nil, err
	}

	// context.WithoutCancel: время жизни подписки управляется Close, а не ctx инициализации.
	watcher, err := kv.Watch(context.WithoutCancel(ctx), sanitizeToken(env)+".>")
	if err != nil {
		return nil, fmt.Errorf("failed to create kv watcher: %w", err)
	}

	w := &RouteWatcher{watcher: watcher, done: make(chan struct{})}
	go w.run(log, onUpdate)
	return w, nil
}

func (w *RouteWatcher) run(log *slog.Logger, onUpdate func(RouteUpdate)) {
	defer close(w.done)
	log.Info("Route watcher started")

	routeHashes := make(map[string][32]byte)

	for entry := range w.watcher.Updates() {
		if entry == nil {
			log.Debug("Initial routes received", "routes", len(routeHashes))
			continue
		}

		_, serviceName, version, ok := parseRouteKey(entry.Key())
		if !ok {
			log.Warn("Skipping malformed route key", "key", entry.Key())
			continue
		}
		routeID := serviceName + "." + version

		if op := entry.Operation(); op == jetstream.KeyValueDelete || op == jetstream.KeyValuePurge {
			if _, known := routeHashes[routeID]; !known {
				continue
			}
			delete(routeHashes, routeID)
			log.Warn("Route removed", "service", serviceName, "version", version)
			onUpdate(RouteUpdate{ServiceName: serviceName, Version: version, IsDeleted: true})
			continue
		}

		descriptor := entry.Value()
		hash := sha256.Sum256(descriptor)
		if old, known := routeHashes[routeID]; known && old == hash {
			continue // тот же дескриптор, переопубликованный новой репликой
		}
		routeHashes[routeID] = hash

		log.Info("Route updated", "service", serviceName, "version", version)
		onUpdate(RouteUpdate{ServiceName: serviceName, Version: version, Descriptor: descriptor})
	}

	log.Info("Route watcher stopped")
}

// Close останавливает подписку и ждёт завершения обработки. Повторные вызовы безопасны.
func (w *RouteWatcher) Close(ctx context.Context) error {
	w.closeOnce.Do(func() {
		w.closeErr = w.watcher.Stop()
	})
	select {
	case <-w.done:
		return w.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
