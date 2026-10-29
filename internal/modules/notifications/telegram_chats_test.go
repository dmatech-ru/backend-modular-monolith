package notifications

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nuid"

	"dmatech.ru/internal/modules/notifications/sender"
)

func TestNormalizePhone(t *testing.T) {
	cases := map[string]string{
		"+79990001234":     "79990001234",
		"79990001234":      "79990001234",
		"+7 999 000-12-34": "79990001234",
		"":                 "",
	}
	for in, want := range cases {
		if got := normalizePhone(in); got != want {
			t.Fatalf("normalizePhone(%q) = %q, want %q", in, got, want)
		}
	}
}

// Интеграционные тесты в этом файле требуют NATS с JetStream (как и
// internal/modules/captcha/store_test.go):
//
//	NATS_TEST_URL=nats://localhost:4222 go test ./internal/modules/notifications/
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

func TestTelegramChatStore_LinkThenResolve(t *testing.T) {
	nc := connectTestNATS(t)
	ctx := context.Background()

	store, err := newTelegramChatStore(ctx, nc)
	if err != nil {
		t.Fatalf("newTelegramChatStore: %v", err)
	}
	phone := "+7999" + nuid.Next()[:7]

	if _, err := store.Resolve(ctx, phone); !errors.Is(err, sender.ErrChatNotResolved) {
		t.Fatalf("Resolve before Link: err = %v, want ErrChatNotResolved", err)
	}

	if err := store.Link(ctx, phone, 42); err != nil {
		t.Fatalf("Link: %v", err)
	}
	chatID, err := store.Resolve(ctx, phone)
	if err != nil {
		t.Fatalf("Resolve after Link: %v", err)
	}
	if chatID != 42 {
		t.Fatalf("chatID = %d, want 42", chatID)
	}

	// Тот же номер без "+" — должен резолвиться в тот же chat_id (см.
	// doc-комментарий normalizePhone).
	chatID, err = store.Resolve(ctx, phone[1:])
	if err != nil {
		t.Fatalf("Resolve without leading '+': %v", err)
	}
	if chatID != 42 {
		t.Fatalf("chatID (no '+') = %d, want 42", chatID)
	}

	// Повторная привязка перезаписывает, не ошибается.
	if err := store.Link(ctx, phone, 43); err != nil {
		t.Fatalf("Link (overwrite): %v", err)
	}
	chatID, err = store.Resolve(ctx, phone)
	if err != nil {
		t.Fatalf("Resolve after overwrite: %v", err)
	}
	if chatID != 43 {
		t.Fatalf("chatID after overwrite = %d, want 43", chatID)
	}
}
