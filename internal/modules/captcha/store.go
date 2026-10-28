package captcha

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// challengeBucket — бакет NATS JetStream KV для выданных challenge'ей.
// Бакетный TTL делает истечение бесплатным (JetStream сам убирает
// просроченные ключи — отдельная задача на очистку не нужна), а ключ —
// сам challenge: он криптослучайный (pow.NewChallenge), так что коллизии
// между разными выдачами физически исключены.
const challengeBucket = "captcha_challenges"

// kvStore реализует challengeStore поверх NATS JetStream KV.
type kvStore struct {
	kv jetstream.KeyValue
}

func newKVStore(ctx context.Context, nc *nats.Conn, ttl time.Duration) (*kvStore, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("captcha: jetstream: %w", err)
	}
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      challengeBucket,
		Description: "Captcha: issued proof-of-work challenges awaiting verification",
		TTL:         ttl,
	})
	if err != nil {
		return nil, fmt.Errorf("captcha: open kv bucket %q: %w", challengeBucket, err)
	}
	return &kvStore{kv: kv}, nil
}

func (s *kvStore) put(ctx context.Context, challenge string, difficulty int32) error {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, uint32(difficulty))
	if _, err := s.kv.Create(ctx, challenge, buf); err != nil {
		return fmt.Errorf("captcha: create challenge record: %w", err)
	}
	return nil
}

func (s *kvStore) take(ctx context.Context, challenge string) (int32, error) {
	entry, err := s.kv.Get(ctx, challenge)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return 0, ErrChallengeNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("captcha: get challenge record: %w", err)
	}

	// One-shot: удаляем сразу после чтения. Challenge криптослучаен и
	// никогда не переиспользуется как ключ, поэтому условный Delete по
	// ревизии (как в natsdiscovery, где ключи реально конкурентно
	// перезаписываются) здесь не нужен.
	if err := s.kv.Delete(ctx, challenge); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return 0, fmt.Errorf("captcha: delete challenge record: %w", err)
	}

	data := entry.Value()
	if len(data) != 4 {
		return 0, fmt.Errorf("captcha: corrupt challenge record (%d bytes)", len(data))
	}
	return int32(binary.LittleEndian.Uint32(data)), nil
}
