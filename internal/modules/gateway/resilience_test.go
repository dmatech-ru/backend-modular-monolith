package gateway

import (
	"testing"
	"time"
)

const testClientIP = "198.51.100.1"

// fakeClock даёт тестам полный контроль над временем, которое видит methodGuard.
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock               { return &fakeClock{t: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)} }
func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func withClock(g *methodGuard, c *fakeClock) *methodGuard {
	g.now = c.now
	return g
}

func TestMethodGuard_NilWhenBothDisabled(t *testing.T) {
	g := newMethodGuard(0, 0, 0, 0)
	if g != nil {
		t.Fatalf("expected nil guard when both limiter and breaker are disabled, got %+v", g)
	}
	if g.Before(testClientIP, "/pkg.Svc/Method") != guardAllow {
		t.Fatal("nil guard must always allow")
	}
	g.After("/pkg.Svc/Method", false) // must not panic
}

func TestMethodGuard_RateLimiter_AllowsBurstThenBlocks(t *testing.T) {
	clock := newFakeClock()
	g := withClock(newMethodGuard(1, 3, 0, 0), clock)
	const key = "/pkg.Svc/Method"

	for i := 0; i < 3; i++ {
		if d := g.Before(testClientIP, key); d != guardAllow {
			t.Fatalf("request %d: decision = %v, want guardAllow", i, d)
		}
	}
	if d := g.Before(testClientIP, key); d != guardRateLimited {
		t.Fatalf("4th request within the burst window: decision = %v, want guardRateLimited", d)
	}

	clock.advance(time.Second) // 1 rps -> one token back
	if d := g.Before(testClientIP, key); d != guardAllow {
		t.Fatalf("after refill: decision = %v, want guardAllow", d)
	}
}

func TestMethodGuard_RateLimiter_PerMethodIndependent(t *testing.T) {
	clock := newFakeClock()
	g := withClock(newMethodGuard(1, 1, 0, 0), clock)

	if d := g.Before(testClientIP, "/pkg.Svc/A"); d != guardAllow {
		t.Fatalf("A: decision = %v", d)
	}
	if d := g.Before(testClientIP, "/pkg.Svc/A"); d != guardRateLimited {
		t.Fatalf("A again immediately: decision = %v, want guardRateLimited", d)
	}
	if d := g.Before(testClientIP, "/pkg.Svc/B"); d != guardAllow {
		t.Fatalf("B must not be throttled by A's bucket: decision = %v", d)
	}
}

// Ключевое свойство для защиты от одного агрессивного клиента: у каждого IP
// свой бюджет на метод, один клиент не может выжечь лимит другому.
func TestMethodGuard_RateLimiter_PerIPIndependent(t *testing.T) {
	clock := newFakeClock()
	g := withClock(newMethodGuard(1, 1, 0, 0), clock)
	const key = "/pkg.Svc/Method"

	if d := g.Before("1.1.1.1", key); d != guardAllow {
		t.Fatalf("client A: decision = %v", d)
	}
	if d := g.Before("1.1.1.1", key); d != guardRateLimited {
		t.Fatalf("client A again immediately: decision = %v, want guardRateLimited", d)
	}
	if d := g.Before("2.2.2.2", key); d != guardAllow {
		t.Fatalf("client B must not be throttled by client A's bucket: decision = %v", d)
	}
}

func TestMethodGuard_CircuitBreaker_OpensAfterThreshold(t *testing.T) {
	clock := newFakeClock()
	g := withClock(newMethodGuard(0, 0, 2, 10*time.Second), clock)
	const key = "/pkg.Svc/Method"

	for i := 0; i < 2; i++ {
		if d := g.Before(testClientIP, key); d != guardAllow {
			t.Fatalf("failure #%d: decision = %v, want guardAllow", i, d)
		}
		g.After(key, false)
	}

	if d := g.Before(testClientIP, key); d != guardCircuitOpen {
		t.Fatalf("after %d consecutive failures: decision = %v, want guardCircuitOpen", 2, d)
	}
}

func TestMethodGuard_CircuitBreaker_SuccessResetsFailureCount(t *testing.T) {
	clock := newFakeClock()
	g := withClock(newMethodGuard(0, 0, 2, 10*time.Second), clock)
	const key = "/pkg.Svc/Method"

	g.Before(testClientIP, key)
	g.After(key, false)
	g.Before(testClientIP, key)
	g.After(key, true) // сбрасывает счётчик до достижения порога

	g.Before(testClientIP, key)
	g.After(key, false)
	if d := g.Before(testClientIP, key); d != guardAllow {
		t.Fatalf("single failure after a reset must not open the breaker: decision = %v", d)
	}
}

func TestMethodGuard_CircuitBreaker_HalfOpenCloseOnSuccess(t *testing.T) {
	clock := newFakeClock()
	g := withClock(newMethodGuard(0, 0, 1, 10*time.Second), clock)
	const key = "/pkg.Svc/Method"

	g.Before(testClientIP, key)
	g.After(key, false) // открывает автомат с первой же неудачи (threshold=1)
	if d := g.Before(testClientIP, key); d != guardCircuitOpen {
		t.Fatalf("immediately after opening: decision = %v, want guardCircuitOpen", d)
	}

	clock.advance(10 * time.Second)
	if d := g.Before(testClientIP, key); d != guardAllow {
		t.Fatalf("half-open trial: decision = %v, want guardAllow", d)
	}
	// Конкурентный запрос пока идёт пробный — тоже отклоняется.
	if d := g.Before(testClientIP, key); d != guardCircuitOpen {
		t.Fatalf("concurrent request during half-open trial: decision = %v, want guardCircuitOpen", d)
	}

	g.After(key, true) // пробный запрос успешен — закрываем автомат
	if d := g.Before(testClientIP, key); d != guardAllow {
		t.Fatalf("after half-open success: decision = %v, want guardAllow", d)
	}
}

func TestMethodGuard_CircuitBreaker_HalfOpenFailureReopens(t *testing.T) {
	clock := newFakeClock()
	g := withClock(newMethodGuard(0, 0, 1, 10*time.Second), clock)
	const key = "/pkg.Svc/Method"

	g.Before(testClientIP, key)
	g.After(key, false)
	clock.advance(10 * time.Second)

	g.Before(testClientIP, key) // half-open trial
	g.After(key, false)         // trial fails -> reopen

	if d := g.Before(testClientIP, key); d != guardCircuitOpen {
		t.Fatalf("right after a failed trial: decision = %v, want guardCircuitOpen", d)
	}
	clock.advance(9 * time.Second)
	if d := g.Before(testClientIP, key); d != guardCircuitOpen {
		t.Fatalf("before the new cooldown elapsed: decision = %v, want guardCircuitOpen", d)
	}
	clock.advance(2 * time.Second)
	if d := g.Before(testClientIP, key); d != guardAllow {
		t.Fatalf("after the new cooldown elapsed: decision = %v, want guardAllow (next trial)", d)
	}
}

// В отличие от лимитера, брейкер общий для всех клиентов — это защита
// самого бэкенда/NATS от лавины запросов, когда он уже не отвечает, а не
// персональный лимит одному IP. Поэтому отказы от одного клиента должны
// открыть автомат и для всех остальных.
func TestMethodGuard_CircuitBreaker_SharedAcrossClients(t *testing.T) {
	clock := newFakeClock()
	g := withClock(newMethodGuard(0, 0, 1, 10*time.Second), clock)
	const key = "/pkg.Svc/Method"

	g.Before("1.1.1.1", key)
	g.After(key, false) // клиент A вызвал отказ

	if d := g.Before("2.2.2.2", key); d != guardCircuitOpen {
		t.Fatalf("client B after client A's failure: decision = %v, want guardCircuitOpen (breaker is shared)", d)
	}
}

// Неактивные ключи выметаются — иначе атакующий мог бы раздувать карту
// бесконечным числом разных путей/IP.
func TestMethodGuard_SweepsIdleKeys(t *testing.T) {
	clock := newFakeClock()
	g := withClock(newMethodGuard(1, 1, 0, 0), clock)

	for i := 0; i < 50; i++ {
		g.Before(testClientIP, "/pkg.Svc/Method"+string(rune('A'+i)))
	}
	if len(g.limiterState) != 50 {
		t.Fatalf("limiterState size = %d, want 50", len(g.limiterState))
	}

	clock.advance(methodGuardIdleTTL + time.Second)
	g.Before(testClientIP, "/pkg.Svc/Trigger") // новый запрос попутно запускает sweep

	if len(g.limiterState) != 1 {
		t.Fatalf("limiterState size after sweep = %d, want 1 (only the triggering key)", len(g.limiterState))
	}
	if _, ok := g.limiterState[rateLimitKey{clientIP: testClientIP, methodPath: "/pkg.Svc/Trigger"}]; !ok {
		t.Fatal("the triggering key itself must survive its own sweep pass")
	}
}
