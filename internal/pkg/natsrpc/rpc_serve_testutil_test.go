package natsrpc

import (
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nuid"
)

// Интеграционные тесты в этом пакете требуют NATS:
//
//	NATS_TEST_URL=nats://localhost:4222 go test ./internal/pkg/natsrpc/
func connectTestNATS(t *testing.T) *nats.Conn {
	t.Helper()
	url := os.Getenv("NATS_TEST_URL")
	if url == "" {
		t.Skip("NATS_TEST_URL is not set")
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// uniqueEnv изолирует тесты друг от друга на общем NATS-сервере.
func uniqueEnv() string {
	return "test" + nuid.Next()
}
