package captcha

import (
	"context"
	"errors"
	"fmt"
	"time"

	"dmatech.ru/internal/pkg/pow"
)

// ErrChallengeNotFound — challenge не существует, просрочен или уже был
// использован (take — одноразовая операция).
var ErrChallengeNotFound = errors.New("captcha: challenge not found or expired")

// challengeStore — хранилище выданных challenge'ей, нужное Service.
// Интерфейс объявлен на стороне потребителя, чтобы unit-тесты Service не
// нуждались в реальном NATS (см. store.go — *kvStore его реализует поверх
// NATS JetStream KV с TTL-бакетом).
type challengeStore interface {
	// put регистрирует challenge с указанной сложностью. challenge уникален
	// (криптослучайная строка), повторная put с тем же значением не ожидается.
	put(ctx context.Context, challenge string, difficulty int32) error
	// take одноразово забирает сложность, с которой был выдан challenge:
	// повторный вызов с тем же challenge возвращает ErrChallengeNotFound.
	take(ctx context.Context, challenge string) (int32, error)
}

// Service — доменная логика captcha: выдача и проверка PoW-challenge'ей.
// Используют оба хендлера (webapi и internal) — они только переводят вызовы
// в свои сгенерированные connect-интерфейсы, вся логика здесь, в одном месте.
type Service struct {
	store             challengeStore
	defaultDifficulty int32
	ttl               time.Duration
	now               func() time.Time
}

// NewService — defaultDifficulty используется, когда IssueChallenge
// попросили difficulty <= 0; ttl — срок жизни выданного challenge'а.
func NewService(store challengeStore, defaultDifficulty int32, ttl time.Duration) *Service {
	return &Service{store: store, defaultDifficulty: defaultDifficulty, ttl: ttl, now: time.Now}
}

// IssueChallenge выдаёт новый challenge. difficulty <= 0 — использовать
// дефолт сервиса (так вызывает публичный, webapi, эндпоинт: сложность
// клиент не выбирает).
func (s *Service) IssueChallenge(ctx context.Context, difficulty int32) (challenge string, echoedDifficulty int32, expiresAt time.Time, err error) {
	if difficulty <= 0 {
		difficulty = s.defaultDifficulty
	}
	challenge, err = pow.NewChallenge()
	if err != nil {
		return "", 0, time.Time{}, fmt.Errorf("captcha: generate challenge: %w", err)
	}
	if err := s.store.put(ctx, challenge, difficulty); err != nil {
		return "", 0, time.Time{}, fmt.Errorf("captcha: store challenge: %w", err)
	}
	return challenge, difficulty, s.now().Add(s.ttl), nil
}

// VerifyChallenge проверяет решение nonce для challenge. One-shot:
// независимо от результата, challenge после этого вызова больше не валиден.
// Несуществующий/просроченный/повторно использованный challenge — валидный
// исход "не прошёл" (false, nil), а не ошибка вызова.
func (s *Service) VerifyChallenge(ctx context.Context, challenge, nonce string) (bool, error) {
	difficulty, err := s.store.take(ctx, challenge)
	if errors.Is(err, ErrChallengeNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("captcha: take challenge: %w", err)
	}
	return pow.Verify(challenge, nonce, int(difficulty)), nil
}
