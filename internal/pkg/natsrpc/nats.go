package natsrpc

import (
	"context"
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
			if err != nil {
				logger.Warn("NATS disconnected", slog.Any("error", err))
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			logger.Info("NATS reconnected", slog.String("url", nc.ConnectedUrl()))
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			if err := nc.LastError(); err != nil {
				logger.Error("NATS connection closed", slog.Any("error", err))
				return
			}
			logger.Info("NATS connection closed")
		}),
	}

	return nats.Connect(url, opts...)
}

// Drain переводит соединение в режим drain и ждёт его полного закрытия.
// Если ctx истекает раньше, соединение закрывается принудительно.
func Drain(ctx context.Context, nc *nats.Conn) error {
	if nc == nil || nc.IsClosed() {
		return nil
	}
	if err := nc.Drain(); err != nil {
		nc.Close()
		return err
	}

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for !nc.IsClosed() {
		select {
		case <-ctx.Done():
			nc.Close()
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return nil
}
