package identity

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
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

// sanitizeAbuseScope — тот же приём, что sanitizeToken в natsdiscovery/
// natsrpc/notifications: ключ KV не может содержать произвольные символы.
// Безопасно лоссовая (regex-замена спецсимволов на "_"), в отличие от
// hashAbuseKey ниже: scope — всегда одна из фиксированных abuseScope*
// констант (см. pow.go), без спецсимволов и без атакующего на входе, так
// что схлопывание здесь просто не может столкнуть два разных scope.
func sanitizeAbuseScope(s string) string {
	return invalidAbuseKeyChars.ReplaceAllString(strings.TrimSpace(s), "_")
}

// hashAbuseKey — key, в отличие от scope, приходит от вызывающего кода и
// может быть чем угодно (IP, email, телефон, адресат восстановления).
// Раньше здесь была такая же лоссовая regex-замена, как у scope, но она
// схлопывала разные реальные адреса в один бакет — например,
// "a.b@example.com" и "a@b.example.com" оба превращались в
// "a_b_example_com", то есть специально подобранным email можно было
// заранее сжечь "бесплатные" попытки чужого адреса. Хеш не схлопывает
// разные входы друг в друга (коллизия SHA-256 не входит в модель угрозы
// здесь) и поэтому безопасен для произвольного содержимого key.
func hashAbuseKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
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
	return sanitizeAbuseScope(scope) + "." + hashAbuseKey(key)
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
