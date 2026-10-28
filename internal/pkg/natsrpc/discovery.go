package natsrpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nuid"
)

// Registration — зарегистрированный в discovery инстанс сервиса.
// Пока она открыта, фоновый тикер продлевает хартбит ноды и восстанавливает
// маршрут, если тот пропал. Close снимает регистрацию.
type Registration struct {
	routes jetstream.KeyValue
	nodes  jetstream.KeyValue
	log    *slog.Logger

	routeKey    string
	nodeKey     string
	nodesPrefix string
	descriptor  []byte

	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// RegisterDescriptors публикует дескриптор маршрутов сервиса и хартбит текущего
// инстанса. Фоновый keep-alive работает до вызова Registration.Close
// (жизненный цикл не привязан к ctx: ctx ограничивает только саму регистрацию).
func RegisterDescriptors(
	ctx context.Context,
	nc *nats.Conn,
	log *slog.Logger,
	env string,
	serviceName string,
	version string,
	descriptorBytes []byte,
) (*Registration, error) {
	if err := requireTokens(map[string]string{"env": env, "serviceName": serviceName, "version": version}); err != nil {
		return nil, err
	}

	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("failed to init jetstream: %w", err)
	}
	routes, err := routesKV(ctx, js)
	if err != nil {
		return nil, err
	}
	nodes, err := nodesKV(ctx, js)
	if err != nil {
		return nil, err
	}

	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("failed to get hostname: %w", err)
	}

	env, serviceName, version = sanitizeToken(env), sanitizeToken(serviceName), sanitizeToken(version)
	// nuid делает ключ уникальным даже для нескольких процессов на одном хосте.
	instanceID := sanitizeToken(hostname) + "-" + nuid.Next()

	r := &Registration{
		routes:      routes,
		nodes:       nodes,
		log:         log,
		routeKey:    routeKey(env, serviceName, version),
		nodesPrefix: routeKey(env, serviceName, version) + ".",
		descriptor:  descriptorBytes,
		done:        make(chan struct{}),
	}
	r.nodeKey = r.nodesPrefix + instanceID

	// Порядок важен: сначала хартбит, затем маршрут. Тогда уходящая
	// конкурентно «последняя» нода либо увидит наш хартбит, либо наша запись
	// маршрута сменит ревизию и её условное удаление не пройдёт (см. deregister).
	if _, err := nodes.Put(ctx, r.nodeKey, []byte("alive")); err != nil {
		return nil, fmt.Errorf("failed to put initial node heartbeat: %w", err)
	}
	// Маршрут пишется при каждом старте: так новая сборка той же версии
	// обновляет дескриптор, а гейтвей отбрасывает дубликаты по хэшу.
	if _, err := routes.Put(ctx, r.routeKey, descriptorBytes); err != nil {
		_ = nodes.Delete(ctx, r.nodeKey)
		return nil, fmt.Errorf("failed to put routes: %w", err)
	}

	loopCtx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go r.keepAlive(loopCtx)

	log.Info("Service registered in discovery", "route", r.routeKey, "node", r.nodeKey)
	return r, nil
}

func (r *Registration) keepAlive(ctx context.Context) {
	defer close(r.done)

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			opCtx, cancel := context.WithTimeout(ctx, operationTimeout)
			if _, err := r.nodes.Put(opCtx, r.nodeKey, []byte("alive")); err != nil {
				r.log.Warn("Failed to renew node heartbeat", "node", r.nodeKey, "error", err)
			}
			if err := r.ensureRoute(opCtx); err != nil {
				r.log.Warn("Failed to ensure route", "route", r.routeKey, "error", err)
			}
			cancel()
		}
	}
}

// ensureRoute восстанавливает маршрут, если его удалили, пока нода жива
// (например, в гонке с уходом другой реплики).
func (r *Registration) ensureRoute(ctx context.Context) error {
	_, err := r.routes.Get(ctx, r.routeKey)
	if !errors.Is(err, jetstream.ErrKeyNotFound) {
		return err
	}
	_, err = r.routes.Create(ctx, r.routeKey, r.descriptor)
	if errors.Is(err, jetstream.ErrKeyExists) {
		return nil // маршрут уже восстановила другая реплика
	}
	if err != nil {
		return err
	}
	r.log.Warn("Route was missing and has been restored", "route", r.routeKey)
	return nil
}

// Close останавливает keep-alive, удаляет хартбит ноды и, если нода была
// последней для своей версии, удаляет маршрут. Повторные вызовы безопасны.
func (r *Registration) Close(ctx context.Context) error {
	r.closeOnce.Do(func() {
		r.cancel()
		<-r.done
		r.closeErr = r.deregister(ctx)
		if r.closeErr == nil {
			r.log.Info("Service deregistered from discovery", "node", r.nodeKey)
		}
	})
	return r.closeErr
}

func (r *Registration) deregister(ctx context.Context) error {
	if err := r.nodes.Delete(ctx, r.nodeKey); err != nil {
		return fmt.Errorf("failed to delete node heartbeat: %w", err)
	}

	// Ревизия маршрута фиксируется ДО проверки соседей: если реплика стартует
	// конкурентно, она либо уже видна в NodesBucket, либо перезапишет маршрут
	// и удаление с LastRevision завершится ErrKeyRevisionMismatch.
	entry, err := r.routes.Get(ctx, r.routeKey)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read route: %w", err)
	}

	others, err := r.hasOtherNodes(ctx)
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}
	if others {
		return nil
	}

	// «Последний тушит свет».
	err = r.routes.Delete(ctx, r.routeKey, jetstream.LastRevision(entry.Revision()))
	if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
		r.log.Debug("Route was republished by another replica, keeping it", "route", r.routeKey)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to delete route: %w", err)
	}
	return nil
}

func (r *Registration) hasOtherNodes(ctx context.Context) (bool, error) {
	lister, err := r.nodes.ListKeysFiltered(ctx, r.nodesPrefix+">")
	if err != nil {
		return false, err
	}
	defer lister.Stop()

	for key := range lister.Keys() {
		if key != r.nodeKey {
			return true, nil
		}
	}
	return false, ctx.Err()
}
