package gateway

import (
	"sync"
	"time"
)

// Таймауты обслуживания карт состояний methodGuard. Ключи приходят из
// запроса (путь и/или IP клиента), то есть контролируются атакующим — без
// выметания неактивных записей он мог бы раздувать карту бесконечным числом
// разных путей/адресов. Сами значения не настраиваются конфигом: это
// деталь реализации, а не поведение, которое стоит тюнить снаружи.
const (
	methodGuardSweepInterval = time.Minute
	methodGuardIdleTTL       = 10 * time.Minute
)

// guardDecision — что methodGuard.Before решил сделать с запросом, прежде
// чем он дойдёт до NATS.
type guardDecision int

const (
	guardAllow guardDecision = iota
	guardRateLimited
	guardCircuitOpen
)

// methodGuard — rate limiter (token bucket) и circuit breaker для
// проксируемых запросов шлюза. У них разные ключи, и это не случайно:
//   - лимитер — на (IP клиента, метод): один агрессивный клиент не должен
//     съедать общий бюджет метода и блокировать остальных (noisy neighbor).
//   - брейкер — только на метод, без IP: это защита самого NATS/бэкенда от
//     лавины запросов, когда он уже не отвечает. Если бы брейкер тоже был
//     per-IP, при реальном отказе сервиса каждый новый (или просто новый
//     для этого IP) клиент продолжал бы безнаказанно долбить мёртвый NATS —
//     ровно то, от чего брейкер должен защищать.
//
// nil-guard (оба порога выключены) — Before всегда разрешает, After —
// no-op, без единой лишней аллокации.
type methodGuard struct {
	limiterRPS       float64
	limiterBurst     float64
	breakerThreshold int
	breakerCooldown  time.Duration
	now              func() time.Time

	mu           sync.Mutex
	limiterState map[rateLimitKey]*limiterState
	breakerState map[string]*breakerState
	lastSweep    time.Time
}

// rateLimitKey — ключ лимитера: IP клиента + метод.
type rateLimitKey struct {
	clientIP   string
	methodPath string
}

type limiterState struct {
	// Token bucket: tokens пополняются до limiterBurst со скоростью limiterRPS/сек.
	tokens        float64
	tokenRefillAt time.Time
	lastSeen      time.Time
}

type breakerState struct {
	// closed, если openUntil — нулевое время.
	consecutiveFailures int
	openUntil           time.Time
	halfOpenInFlight    bool
	lastSeen            time.Time
}

// newMethodGuard возвращает nil, если ни лимитер, ни брейкер не включены
// (rps <= 0 и threshold <= 0) — тогда проверки в handleProxy становятся
// no-op без накладных расходов.
func newMethodGuard(rps float64, burst int, breakerThreshold int, cooldown time.Duration) *methodGuard {
	if rps <= 0 && breakerThreshold <= 0 {
		return nil
	}
	if rps > 0 && burst <= 0 {
		burst = 1 // иначе бакет стартует пустым и блокирует всё с первого запроса
	}
	return &methodGuard{
		limiterRPS:       rps,
		limiterBurst:     float64(burst),
		breakerThreshold: breakerThreshold,
		breakerCooldown:  cooldown,
		now:              time.Now,
		limiterState:     make(map[rateLimitKey]*limiterState),
		breakerState:     make(map[string]*breakerState),
	}
}

// Before проверяет лимитер (по clientIP+methodPath) и брейкер (по
// methodPath) и обновляет их состояние. Если возвращает guardAllow и
// брейкер включён, вызывающий код обязан затем вызвать After с результатом
// ровно этого запроса — Before может занять единственный пробный слот
// half-open, и его должен освободить именно последующий After.
func (g *methodGuard) Before(clientIP, methodPath string) guardDecision {
	if g == nil {
		return guardAllow
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	g.sweepIfDue(now)

	var bst *breakerState
	promoteHalfOpen := false
	if g.breakerThreshold > 0 {
		bst = g.breakerStateFor(methodPath, now)
		switch {
		case bst.openUntil.IsZero():
			// closed — продолжаем.
		case now.Before(bst.openUntil):
			return guardCircuitOpen
		case bst.halfOpenInFlight:
			return guardCircuitOpen
		default:
			promoteHalfOpen = true
		}
	}

	if g.limiterRPS > 0 {
		lst := g.limiterStateFor(clientIP, methodPath, now)
		elapsed := now.Sub(lst.tokenRefillAt).Seconds()
		lst.tokenRefillAt = now
		lst.tokens = min(g.limiterBurst, lst.tokens+elapsed*g.limiterRPS)
		if lst.tokens < 1 {
			// Не трогаем состояние брейкера: эта попытка так и не дошла до
			// NATS, promoteHalfOpen (если был) просто сгорает до следующего
			// Before, а halfOpenInFlight мы ещё не установили.
			return guardRateLimited
		}
		lst.tokens--
	}

	if promoteHalfOpen {
		bst.halfOpenInFlight = true
	}
	return guardAllow
}

// After сообщает результат запроса, для которого Before вернул guardAllow.
// Только для брейкера (методом, без IP — см. комментарий у methodGuard).
// success=false учитывает только отказы транспорта (NATS недоступен/не
// ответил за таймаут) — бизнес-ошибки сервиса (4xx/5xx самого ответа) не
// должны открывать брейкер, это не инфраструктурный отказ.
func (g *methodGuard) After(methodPath string, success bool) {
	if g == nil || g.breakerThreshold <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	bst, ok := g.breakerState[methodPath]
	if !ok {
		return
	}
	now := g.now()
	wasHalfOpen := bst.halfOpenInFlight
	bst.halfOpenInFlight = false

	if success {
		bst.consecutiveFailures = 0
		bst.openUntil = time.Time{}
		return
	}

	bst.consecutiveFailures++
	if wasHalfOpen || bst.consecutiveFailures >= g.breakerThreshold {
		bst.openUntil = now.Add(g.breakerCooldown)
	}
}

func (g *methodGuard) limiterStateFor(clientIP, methodPath string, now time.Time) *limiterState {
	key := rateLimitKey{clientIP: clientIP, methodPath: methodPath}
	st, ok := g.limiterState[key]
	if !ok {
		st = &limiterState{tokens: g.limiterBurst, tokenRefillAt: now}
		g.limiterState[key] = st
	}
	st.lastSeen = now
	return st
}

func (g *methodGuard) breakerStateFor(methodPath string, now time.Time) *breakerState {
	st, ok := g.breakerState[methodPath]
	if !ok {
		st = &breakerState{}
		g.breakerState[methodPath] = st
	}
	st.lastSeen = now
	return st
}

func (g *methodGuard) sweepIfDue(now time.Time) {
	if now.Sub(g.lastSweep) < methodGuardSweepInterval {
		return
	}
	g.lastSweep = now
	for key, st := range g.limiterState {
		if now.Sub(st.lastSeen) > methodGuardIdleTTL {
			delete(g.limiterState, key)
		}
	}
	for key, st := range g.breakerState {
		if now.Sub(st.lastSeen) > methodGuardIdleTTL {
			delete(g.breakerState, key)
		}
	}
}
