package natsrpc

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const NodeHeartbeatTTL = 10 * time.Second

func RegisterDescriptors(
	ctx context.Context,
	nc *nats.Conn,
	env string,
	serviceName string,
	version string,
	descriptorBytes []byte,
) error {
	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("failed to init jetstream: %w", err)
	}

	safeVersion := sanitizeVersion(version)

	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      "service_discovery",
		Description: "Split-routing NATS JetStream KV Service Discovery with Versioning",
		TTL:         NodeHeartbeatTTL,
	})
	if err != nil {
		return fmt.Errorf("failed to get kv bucket: %w", err)
	}

	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("failed to get hostname: %w", err)
	}

	routeKey := fmt.Sprintf("discovery.%s.routes.%s.%s", env, serviceName, safeVersion)
	nodeKey := fmt.Sprintf("discovery.%s.nodes.%s.%s.%s", env, serviceName, safeVersion, hostname)

	exists, err := hasRoute(ctx, kv, routeKey)
	if err != nil {
		return fmt.Errorf("failed to check existing routes: %w", err)
	}

	if !exists {
		_, err = kv.Put(ctx, routeKey, descriptorBytes)
		if err != nil {
			return fmt.Errorf("failed to put routes: %w", err)
		}
	}

	_, err = kv.Put(ctx, nodeKey, []byte("alive"))
	if err != nil {
		return fmt.Errorf("failed to put initial node heartbeat: %w", err)
	}

	pingCtx, pingCancel := context.WithCancel(ctx)

	go func() {
		defer pingCancel()

		ticker := time.NewTicker(NodeHeartbeatTTL / 3)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				putCtx, cancel := context.WithTimeout(pingCtx, 2*time.Second)
				_, _ = kv.Put(putCtx, nodeKey, []byte("alive"))
				cancel()

			case <-pingCtx.Done():
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				_ = kv.Delete(cleanupCtx, nodeKey)

				lister, err := kv.ListKeys(cleanupCtx)

				hasOtherNodes := false
				if err == nil {
					// Define the target prefix filter manually: "discovery.dev.nodes.identity.1_0_0."
					targetPrefix := fmt.Sprintf("discovery.%s.nodes.%s.%s.", env, serviceName, safeVersion)

					for key := range lister.Keys() {
						// Match if there is any other key sharing this exact environment, service, and version
						if key != "" && strings.HasPrefix(key, targetPrefix) {
							hasOtherNodes = true
							break
						}
					}
				}

				// If no other replicas for this specific version exist, turn off the lights
				if !hasOtherNodes {
					_ = kv.Delete(cleanupCtx, routeKey)
				}
				return
			}
		}
	}()

	return nil
}

type RouteUpdate struct {
	ServiceName string
	Version     string
	Bytes       []byte // Сырые байты Protobuf-дескриптора (buf)
	IsDeleted   bool   // true, если последняя нода ушла и маршрут удален
}

func WatchRoutes(
	ctx context.Context,
	nc *nats.Conn,
	log *slog.Logger,
	env string,
	onUpdate func(serviceName, version string, descriptorBytes []byte, isDeleted bool),
) error {
	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("failed to init jetstream: %w", err)
	}

	kv, err := js.KeyValue(ctx, "service_discovery")
	if err != nil {
		return fmt.Errorf("failed to connect to kv bucket: %w", err)
	}

	searchPattern := fmt.Sprintf("discovery.%s.routes.>", env)
	watcher, err := kv.Watch(ctx, searchPattern)
	if err != nil {
		return fmt.Errorf("failed to create kv watcher: %w", err)
	}

	go func() {
		defer watcher.Stop()
		log.Info("Dynamic route watcher started inside discovery")

		// Локальный кэш хэшей изолирован внутри горутины дискавери
		routeHashes := make(map[string][32]byte)

		for entry := range watcher.Updates() {
			if entry == nil {
				continue
			}

			keySegments := strings.Split(entry.Key(), ".")
			if len(keySegments) < 5 {
				continue
			}

			serviceName := keySegments[3]
			version := keySegments[4]
			routeID := serviceName + "." + version

			// 1. Обработка жесткого удаления маршрута (последняя нода версии ушла)
			if entry.Operation() == jetstream.KeyValueDelete || entry.Operation() == jetstream.KeyValuePurge {
				log.Warn("Route removed completely", "service", serviceName, "version", version)
				delete(routeHashes, routeID)

				// Дергаем колбэк гейтвея на удаление
				onUpdate(serviceName, version, nil, true)
				continue
			}

			// 2. Обработка создания или обновления маршрута (KeyValuePut)
			descriptorBytes := entry.Value()
			currentHash := sha256.Sum256(descriptorBytes)

			// Проверяем хэш. Если совпал — это холостой пинг продления TTL, игнорируем его!
			if oldHash, exists := routeHashes[routeID]; exists && oldHash == currentHash {
				continue
			}

			// Хэш изменился — это реальный деплой или первый запуск ноды
			routeHashes[routeID] = currentHash
			log.Info("New route version detected, triggering configuration", "service", serviceName, "version", version)

			// Передаем чистые данные в колбэк гейтвея
			onUpdate(serviceName, version, descriptorBytes, false)
		}

		log.Info("Route watcher stopped cleanly inside discovery")
	}()

	return nil
}

func hasRoute(ctx context.Context, kv jetstream.KeyValue, routeKey string) (bool, error) {
	_, err := kv.Get(ctx, routeKey)
	if err == nil {
		return true, nil
	}
	if err == jetstream.ErrKeyNotFound {
		return false, nil
	}
	return false, err
}

func sanitizeVersion(v string) string {
	return strings.ReplaceAll(v, ".", "_")
}
