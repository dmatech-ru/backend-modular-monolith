package captcha

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nuid"
)

// Интеграционные тесты в этом пакете требуют NATS с JetStream:
//
//	NATS_TEST_URL=nats://localhost:4222 go test ./internal/modules/captcha/
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

func TestKVStore_PutThenTake(t *testing.T) {
	nc := connectTestNATS(t)
	ctx := context.Background()

	store, err := newKVStore(ctx, nc, time.Minute)
	if err != nil {
		t.Fatalf("newKVStore: %v", err)
	}
	challenge := "test-challenge-" + nuid.Next()

	if err := store.put(ctx, challenge, 17); err != nil {
		t.Fatalf("put: %v", err)
	}

	difficulty, err := store.take(ctx, challenge)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if difficulty != 17 {
		t.Fatalf("difficulty = %d, want 17", difficulty)
	}
}

func TestKVStore_TakeIsOneShot(t *testing.T) {
	nc := connectTestNATS(t)
	ctx := context.Background()

	store, err := newKVStore(ctx, nc, time.Minute)
	if err != nil {
		t.Fatalf("newKVStore: %v", err)
	}
	challenge := "test-challenge-" + nuid.Next()

	if err := store.put(ctx, challenge, 10); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := store.take(ctx, challenge); err != nil {
		t.Fatalf("first take: %v", err)
	}
	if _, err := store.take(ctx, challenge); !errors.Is(err, ErrChallengeNotFound) {
		t.Fatalf("second take = %v, want ErrChallengeNotFound", err)
	}
}

func TestKVStore_TakeUnknownChallenge(t *testing.T) {
	nc := connectTestNATS(t)
	ctx := context.Background()

	store, err := newKVStore(ctx, nc, time.Minute)
	if err != nil {
		t.Fatalf("newKVStore: %v", err)
	}

	if _, err := store.take(ctx, "never-issued-"+nuid.Next()); !errors.Is(err, ErrChallengeNotFound) {
		t.Fatalf("take unknown challenge = %v, want ErrChallengeNotFound", err)
	}
}

func TestKVStore_ExpiresAfterTTL(t *testing.T) {
	nc := connectTestNATS(t)
	ctx := context.Background()

	const ttl = 1100 * time.Millisecond
	store, err := newKVStore(ctx, nc, ttl)
	if err != nil {
		t.Fatalf("newKVStore: %v", err)
	}
	challenge := "test-challenge-" + nuid.Next()

	if err := store.put(ctx, challenge, 10); err != nil {
		t.Fatalf("put: %v", err)
	}
	time.Sleep(ttl + 500*time.Millisecond)

	if _, err := store.take(ctx, challenge); !errors.Is(err, ErrChallengeNotFound) {
		t.Fatalf("take after TTL = %v, want ErrChallengeNotFound", err)
	}
}
