package identity

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nuid"
)

func TestSanitizeAbuseScope(t *testing.T) {
	cases := map[string]string{
		"check_identifier":         "check_identifier",
		"start_phone_verification": "start_phone_verification",
		"already_safe-key42":       "already_safe-key42",
	}
	for in, want := range cases {
		if got := sanitizeAbuseScope(in); got != want {
			t.Fatalf("sanitizeAbuseScope(%q) = %q, want %q", in, got, want)
		}
	}
}

// hashAbuseKey не должен схлопывать разные реальные адреса в один бакет —
// именно это делала прежняя лоссовая regex-санитизация (".", "@" оба
// превращались в "_"), позволяя специально подобранным email/телефоном
// заранее сжечь "бесплатные" попытки чужого адреса (см. doc-комментарий
// hashAbuseKey в abuse.go).
func TestHashAbuseKey_DifferentInputsNeverCollide(t *testing.T) {
	cases := []string{
		"a.b@example.com",
		"a@b.example.com",
		"+79990001234",
		"+7999000-1234",
		"203.0.113.5",
		"203_0_113_5",
	}
	seen := map[string]string{}
	for _, in := range cases {
		got := hashAbuseKey(in)
		if prev, ok := seen[got]; ok {
			t.Fatalf("hashAbuseKey(%q) = hashAbuseKey(%q) = %q, want distinct hashes", in, prev, got)
		}
		seen[got] = in
	}
}

// Один и тот же вход должен всегда давать один и тот же хеш — иначе
// Count/RecordAttempt никогда бы не совпадали по ключу.
func TestHashAbuseKey_Deterministic(t *testing.T) {
	if hashAbuseKey("alice@example.com") != hashAbuseKey("alice@example.com") {
		t.Fatal("hashAbuseKey is not deterministic")
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
