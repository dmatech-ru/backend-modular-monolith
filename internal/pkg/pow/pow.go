// Package pow — proof-of-work challenge/verify (hashcash-подобный), без
// внешних зависимостей. Задача: SHA-256(challenge || nonce) должен иметь не
// меньше difficulty ведущих нулевых бит. Подбор nonce стоит клиенту заметные
// (но контролируемые — difficulty настраивается) CPU-циклы, проверка на
// сервере — один хэш, независимо от difficulty.
//
// Не заменяет визуальную капчу против продвинутых ботов — здесь и не нужно:
// цель — поднять цену массового скриптового перебора (account enumeration,
// SMS/email-бомбёжка), а не остановить целевую атаку с бюджетом на вычисления.
// Выдачу challenge'ей, их one-shot хранение и TTL делает вызывающий код
// (см. internal/modules/captcha) — этот пакет только считает хэши.
package pow

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math/bits"
)

// NewChallenge возвращает случайную challenge-строку (32 байта, hex).
func NewChallenge() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// Verify — true, если SHA-256(challenge || nonce) имеет не меньше difficulty
// ведущих нулевых бит. difficulty <= 0 считается пройденным всегда (сервис
// сам не выдаёт такую сложность, но вызывающий код может использовать это
// как явный "без защиты" для своих тестов).
func Verify(challenge, nonce string, difficulty int) bool {
	if difficulty <= 0 {
		return true
	}
	if difficulty > sha256.Size*8 {
		return false // физически невозможно — не тратим время на подсчёт
	}
	sum := sha256.Sum256([]byte(challenge + nonce))
	return leadingZeroBits(sum[:]) >= difficulty
}

func leadingZeroBits(b []byte) int {
	count := 0
	for _, by := range b {
		if by == 0 {
			count += 8
			continue
		}
		return count + bits.LeadingZeros8(by)
	}
	return count
}

// Solve подбирает nonce методом перебора (счётчик, переведённый в hex) —
// референсная реализация клиента для тестов/демо, не для продакшен-сервера.
// maxAttempts ограничивает перебор, чтобы вызов не завис при неадекватной
// difficulty; при достижении лимита возвращает ok=false.
func Solve(challenge string, difficulty, maxAttempts int) (nonce string, ok bool) {
	for i := 0; i < maxAttempts; i++ {
		candidate := hex.EncodeToString([]byte{
			byte(i), byte(i >> 8), byte(i >> 16), byte(i >> 24),
		})
		if Verify(challenge, candidate, difficulty) {
			return candidate, true
		}
	}
	return "", false
}
