package identity

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// abuseBucket — NATS JetStream KV-бакет для счётчиков непройденных/неверных
// попыток PoW на (scope, key) — адаптивный PoW, см. doc-комментарий
// adaptivePoWGate в handlers/webapi/v1/pow.go и doc-комментарий сервисов в
// registration.proto/recovery.proto про "N попыток без решения". TTL на
// бакет — тот же приём, что у captcha_challenges (internal/modules/captcha/
// store.go): окно само закрывается без отдельной задачи на чистку, просто
// заметно более длинное (окно адаптивного PoW, не срок жизни одного
// challenge'а).
const abuseBucket = "identity_pow_attempts"

var invalidAbuseKeyChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// sanitizeAbuseKey — тот же приём, что sanitizeToken в natsdiscovery/
// natsrpc/notifications: ключ KV не может содержать произвольные символы
// (IP — точки и двоеточия у IPv6, email/телефон — "@"/"+" и т.п.).
func sanitizeAbuseKey(s string) string {
	return invalidAbuseKeyChars.ReplaceAllString(strings.TrimSpace(s), "_")
}

// attemptTracker реализует authhandler.AbuseTracker поверх NATS JetStream
// KV — тот же приём, что captcha.kvStore для challenge'ей (см.
// internal/modules/captcha/store.go), но без one-shot чтения: счётчик
// читается и увеличивается многократно, пока не истечёт TTL бакета.
type attemptTracker struct {
	kv jetstream.KeyValue
}

func newAttemptTracker(ctx context.Context, nc *nats.Conn, window time.Duration) (*attemptTracker, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("identity: jetstream: %w", err)
	}
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      abuseBucket,
		Description: "Identity: adaptive PoW — count of unsolved/invalid attempts per scope+key",
		TTL:         window,
	})
	if err != nil {
		return nil, fmt.Errorf("identity: open kv bucket %q: %w", abuseBucket, err)
	}
	return &attemptTracker{kv: kv}, nil
}

func (t *attemptTracker) key(scope, key string) string {
	return sanitizeAbuseKey(scope) + "." + sanitizeAbuseKey(key)
}

// Count — см. doc-комментарий authhandler.AbuseTracker.
func (t *attemptTracker) Count(ctx context.Context, scope, key string) (int, error) {
	entry, err := t.kv.Get(ctx, t.key(scope, key))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("identity: get attempt count: %w", err)
	}
	data := entry.Value()
	if len(data) != 4 {
		return 0, fmt.Errorf("identity: corrupt attempt count record (%d bytes)", len(data))
	}
	return int(binary.LittleEndian.Uint32(data)), nil
}

// RecordAttempt увеличивает счётчик на 1. Не атомарно (Get, затем Put, без
// CAS по ревизии, в отличие от "последний тушит свет" в natsdiscovery,
// которому гонка была бы дорога): это мягкий, не security-критичный
// сигнал — твёрдая граница от массового перебора — rate limit на гейтвее
// (см. README), редкая гонка на инкременте в худшем случае отложит
// срабатывание порога на один запрос, не больше.
func (t *attemptTracker) RecordAttempt(ctx context.Context, scope, key string) error {
	count, err := t.Count(ctx, scope, key)
	if err != nil {
		return err
	}
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, uint32(count+1))
	if _, err := t.kv.Put(ctx, t.key(scope, key), buf); err != nil {
		return fmt.Errorf("identity: record attempt: %w", err)
	}
	return nil
}
