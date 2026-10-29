package v1

import (
	"context"
	"errors"
	"testing"
)

const testAdaptiveScope = "test_scope"

// Пока накопленных попыток меньше threshold, вызов проходит без решения
// вовсе — ровно то "единичный легитимный пользователь не замечает этого
// шага", что задумано в doc-комментарии сервиса в registration.proto.
func TestAdaptivePoWGate_FreeAttemptsUnderThreshold(t *testing.T) {
	captcha := &fakeCaptchaClient{result: false}
	abuse := newFakeAbuseTracker()

	for i := 0; i < 3; i++ {
		ok, err := adaptivePoWGate(context.Background(), captcha, abuse, 3, testAdaptiveScope, "key-1", "", "")
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if !ok {
			t.Fatalf("attempt %d: ok = false, want true (still under threshold)", i)
		}
	}

	count, err := abuse.Count(context.Background(), testAdaptiveScope, "key-1")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 3 {
		t.Fatalf("count = %d, want 3 (every unsolved attempt counted)", count)
	}
}

// Как только накопленных попыток становится не меньше threshold, решение
// становится обязательным.
func TestAdaptivePoWGate_RequiresSolutionAtThreshold(t *testing.T) {
	captcha := &fakeCaptchaClient{result: false}
	abuse := newFakeAbuseTracker()

	for i := 0; i < 2; i++ {
		if _, err := adaptivePoWGate(context.Background(), captcha, abuse, 2, testAdaptiveScope, "key-1", "", ""); err != nil {
			t.Fatalf("warm-up attempt %d: %v", i, err)
		}
	}

	ok, err := adaptivePoWGate(context.Background(), captcha, abuse, 2, testAdaptiveScope, "key-1", "", "")
	if err != nil {
		t.Fatalf("adaptivePoWGate: %v", err)
	}
	if ok {
		t.Fatal("ok = true, want false (threshold reached, no solution provided)")
	}
}

// Валидное решение всегда пропускает вызов, даже до достижения порога — и
// не увеличивает счётчик: атакующий, который решает каждый раз, уже платит
// ровно ту цену, которую PoW должен был наложить.
func TestAdaptivePoWGate_ValidSolutionAlwaysPassesAndDoesNotIncrement(t *testing.T) {
	captcha := &fakeCaptchaClient{result: true}
	abuse := newFakeAbuseTracker()

	ok, err := adaptivePoWGate(context.Background(), captcha, abuse, 0, testAdaptiveScope, "key-1", "challenge", "nonce")
	if err != nil {
		t.Fatalf("adaptivePoWGate: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true (valid solution provided)")
	}

	count, err := abuse.Count(context.Background(), testAdaptiveScope, "key-1")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0 (a valid solution must not count against the caller)", count)
	}
}

// После порога неверное решение всё равно отказывается (не путается с
// "решения не было вовсе") и продолжает увеличивать счётчик.
func TestAdaptivePoWGate_InvalidSolutionAtThresholdIsRejected(t *testing.T) {
	captcha := &fakeCaptchaClient{result: false}
	abuse := newFakeAbuseTracker()

	ok, err := adaptivePoWGate(context.Background(), captcha, abuse, 0, testAdaptiveScope, "key-1", "challenge", "wrong-nonce")
	if err != nil {
		t.Fatalf("adaptivePoWGate: %v", err)
	}
	if ok {
		t.Fatal("ok = true, want false (invalid solution)")
	}
}

// Разные ключи (например, разные IP или разные номера телефонов) не делят
// счётчик — иначе один агрессивный ключ "сжигал" бы невидимую фазу для
// всех остальных.
func TestAdaptivePoWGate_DifferentKeysDoNotShareCounter(t *testing.T) {
	captcha := &fakeCaptchaClient{result: false}
	abuse := newFakeAbuseTracker()

	for i := 0; i < 5; i++ {
		if _, err := adaptivePoWGate(context.Background(), captcha, abuse, 2, testAdaptiveScope, "attacker-ip", "", ""); err != nil {
			t.Fatalf("attacker attempt %d: %v", i, err)
		}
	}

	ok, err := adaptivePoWGate(context.Background(), captcha, abuse, 2, testAdaptiveScope, "victim-ip", "", "")
	if err != nil {
		t.Fatalf("adaptivePoWGate: %v", err)
	}
	if !ok {
		t.Fatal("ok = false for a different key, want true (counters must not be shared across keys)")
	}
}

// Ошибка самой проверки у captcha-модуля должна возвращаться вызывающему
// коду как есть, не превращаться молча в CAPTCHA_REQUIRED.
func TestAdaptivePoWGate_PropagatesCaptchaError(t *testing.T) {
	boom := errors.New("captcha unavailable")
	captcha := &fakeCaptchaClient{err: boom}
	abuse := newFakeAbuseTracker()

	_, err := adaptivePoWGate(context.Background(), captcha, abuse, 0, testAdaptiveScope, "key-1", "challenge", "nonce")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}
