package natsrpc

import (
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

func NewConnection(url string, logger *slog.Logger) (*nats.Conn, error) {
	opts := []nats.Option{
		nats.Name("Modular Monolith"),
		nats.MaxReconnects(-1), // пытаться переподключиться бесконечно
		nats.ReconnectWait(2 * time.Second),

		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			logger.Warn("NATS disconnected", slog.Any("error", err))
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			logger.Info("NATS reconnected", slog.String("url", nc.ConnectedUrl()))
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			logger.Error("NATS connection closed completely")
		}),
	}

	nc, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, err
	}

	return nc, nil
}
