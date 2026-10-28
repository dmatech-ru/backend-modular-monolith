package v1

import (
	"log/slog"

	"github.com/nats-io/nats.go"

	"dmatech.ru/internal/pkg/natsrpc"
)

// Subscribe подписывает IdentityAuthService на входящие запросы через NATS:
// связывает HTTP-обработчик (см. auth.go) с транспортом natsrpc.ServeHandler,
// чтобы гейтвей мог достучаться до него по маршруту {env}.{serviceName}.{version}.
func Subscribe(nc *nats.Conn, log *slog.Logger, env, serviceName, version string, repo Repository) (*natsrpc.RPCSubscription, error) {
	_, handler := NewHandler(repo)
	return natsrpc.ServeHandler(nc, log, env, serviceName, version, handler)
}
