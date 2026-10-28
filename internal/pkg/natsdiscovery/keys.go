// Package natsdiscovery — реактивный Service Discovery поверх NATS
// JetStream KV: публикация и отслеживание протобуф-дескрипторов модулей и
// хартбитов их инстансов.
//
// Данные discovery разнесены по двум бакетам:
//
//   - RoutesBucket — бинарные дескрипторы маршрутов, ключ {env}.{service}.{version}.
//     Ключи не имеют TTL и удаляются последней уходящей нодой версии.
//   - NodesBucket — хартбиты инстансов, ключ {env}.{service}.{version}.{instance}.
//     Ключи живут NodeHeartbeatTTL и продлеваются фоновым тикером.
package natsdiscovery

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

const (
	RoutesBucket = "service_routes"
	NodesBucket  = "service_nodes"

	NodeHeartbeatTTL = 10 * time.Second

	heartbeatInterval = NodeHeartbeatTTL / 3
	operationTimeout  = 2 * time.Second
)

var invalidTokenChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// sanitizeToken превращает строку в безопасный токен ключа NATS KV:
// точки (разделитель токенов), wildcard-символы и прочее заменяются на "_".
// Например, версия "1.0.0" становится "1_0_0".
func sanitizeToken(s string) string {
	return invalidTokenChars.ReplaceAllString(strings.TrimSpace(s), "_")
}

func routeKey(env, serviceName, version string) string {
	return env + "." + serviceName + "." + version
}

// parseRouteKey разбирает ключ RoutesBucket. Все токены санитизированы,
// поэтому корректный ключ состоит ровно из трёх сегментов.
func parseRouteKey(key string) (env, serviceName, version string, ok bool) {
	parts := strings.Split(key, ".")
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func requireTokens(values map[string]string) error {
	var errs []error
	for name, value := range values {
		if sanitizeToken(value) == "" {
			errs = append(errs, fmt.Errorf("%s must not be empty", name))
		}
	}
	return errors.Join(errs...)
}

func routesKV(ctx context.Context, js jetstream.JetStream) (jetstream.KeyValue, error) {
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      RoutesBucket,
		Description: "Service discovery: protobuf descriptors per service version",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open kv bucket %q: %w", RoutesBucket, err)
	}
	return kv, nil
}

func nodesKV(ctx context.Context, js jetstream.JetStream) (jetstream.KeyValue, error) {
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      NodesBucket,
		Description: "Service discovery: instance heartbeats",
		TTL:         NodeHeartbeatTTL,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open kv bucket %q: %w", NodesBucket, err)
	}
	return kv, nil
}
