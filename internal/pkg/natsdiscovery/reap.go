package natsdiscovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// RouteReaper — фоновая проверка маршрутов окружения на наличие живых нод,
// запущенная StartStaleRouteReaper. Close останавливает её.
type RouteReaper struct {
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
}

// StartStaleRouteReaper запускает фоновый цикл: каждые interval вызывает
// ReapStaleRoutes для окружения env. Предназначена для единственного
// постоянного потребителя маршрутов (гейтвея) — именно ему важно, чтобы
// версия без единой живой ноды не считалась рабочей неограниченно долго.
func StartStaleRouteReaper(nc *nats.Conn, log *slog.Logger, env string, interval time.Duration) *RouteReaper {
	ctx, cancel := context.WithCancel(context.Background())
	rr := &RouteReaper{cancel: cancel, done: make(chan struct{})}
	go rr.run(ctx, nc, log, env, interval)
	return rr
}

func (rr *RouteReaper) run(ctx context.Context, nc *nats.Conn, log *slog.Logger, env string, interval time.Duration) {
	defer close(rr.done)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			opCtx, cancel := context.WithTimeout(ctx, operationTimeout)
			if err := ReapStaleRoutes(opCtx, nc, log, env); err != nil {
				log.Warn("Stale route reap failed", "error", err)
			}
			cancel()
		}
	}
}

// Close останавливает фоновый цикл. Повторные вызовы безопасны.
func (rr *RouteReaper) Close(ctx context.Context) error {
	rr.closeOnce.Do(rr.cancel)
	select {
	case <-rr.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReapStaleRoutes один раз проверяет все маршруты окружения env и удаляет
// те, для которых в NodesBucket не осталось ни одной живой ноды — например,
// все реплики этой версии упали без graceful shutdown, и деregister'ить
// маршрут было некому. Удаление — то же условное Delete по ревизии, что и в
// Registration.Close, чтобы не задеть маршрут, который конкурентно
// переопубликовала новая реплика той же версии.
func ReapStaleRoutes(ctx context.Context, nc *nats.Conn, log *slog.Logger, env string) error {
	if err := requireTokens(map[string]string{"env": env}); err != nil {
		return err
	}

	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("failed to init jetstream: %w", err)
	}
	routes, err := routesKV(ctx, js)
	if err != nil {
		return err
	}
	nodes, err := nodesKV(ctx, js)
	if err != nil {
		return err
	}

	lister, err := routes.ListKeysFiltered(ctx, sanitizeToken(env)+".>")
	if err != nil {
		return fmt.Errorf("failed to list routes: %w", err)
	}
	defer lister.Stop()

	var errs []error
	for key := range lister.Keys() {
		_, serviceName, version, ok := parseRouteKey(key)
		if !ok {
			continue
		}

		alive, err := anyKeys(ctx, nodes, key+".>")
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			continue
		}
		if alive {
			continue
		}

		entry, err := routes.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			continue
		}

		if err := routes.Delete(ctx, key, jetstream.LastRevision(entry.Revision())); err != nil {
			if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
				continue // маршрут переопубликован конкурентно — не трогаем
			}
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			continue
		}
		log.Warn("Stale route reaped: no live nodes for this version", "service", serviceName, "version", version)
	}
	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// anyKeys true, если под bucket найдётся хоть один ключ, подходящий под filter.
func anyKeys(ctx context.Context, bucket jetstream.KeyValue, filter string) (bool, error) {
	lister, err := bucket.ListKeysFiltered(ctx, filter)
	if err != nil {
		return false, err
	}
	defer lister.Stop()

	for range lister.Keys() {
		return true, nil
	}
	return false, ctx.Err()
}
