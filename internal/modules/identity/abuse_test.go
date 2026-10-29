package identity

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nuid"
)

func TestSanitizeAbuseKey(t *testing.T) {
	cases := map[string]string{
		"203.0.113.5":        "203_0_113_5",
		"alice@example.com":  "alice_example_com",
		"+79990001234":       "_79990001234",
		"already_safe-key42": "already_safe-key42",
	}
	for in, want := range cases {
		if got := sanitizeAbuseKey(in); got != want {
			t.Fatalf("sanitizeAbuseKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// Интеграционные тесты в этом файле требуют NATS с JetStream (как и
// internal/modules/captcha/store_test.go):
//
//	NATS_TEST_URL=nats://localhost:4222 go test ./internal/modules/identity/
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

func TestAttemptTracker_CountAndRecordAttempt(t *testing.T) {
	nc := connectTestNATS(t)
	ctx := context.Background()

	tracker, err := newAttemptTracker(ctx, nc, time.Minute)
	if err != nil {
		t.Fatalf("newAttemptTracker: %v", err)
	}
	scope := "test_scope"
	key := "key-" + nuid.Next()

	count, err := tracker.Count(ctx, scope, key)
	if err != nil {
		t.Fatalf("Count (unseen key): %v", err)
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0 for an unseen key", count)
	}

	for i := 1; i <= 3; i++ {
		if err := tracker.RecordAttempt(ctx, scope, key); err != nil {
			t.Fatalf("RecordAttempt #%d: %v", i, err)
		}
		count, err := tracker.Count(ctx, scope, key)
		if err != nil {
			t.Fatalf("Count after #%d: %v", i, err)
		}
		if count != i {
			t.Fatalf("count after %d attempts = %d, want %d", i, count, i)
		}
	}

	// Другой scope с тем же key не делит счётчик.
	otherCount, err := tracker.Count(ctx, "other_scope", key)
	if err != nil {
		t.Fatalf("Count (other scope): %v", err)
	}
	if otherCount != 0 {
		t.Fatalf("count in a different scope = %d, want 0 (scopes must not share counters)", otherCount)
	}
}
