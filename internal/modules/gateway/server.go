package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"dmatech.ru/internal/pkg/httpserver"
	"dmatech.ru/internal/pkg/natsrpc"
)

// defaultProxyRequestTimeout подстраховывает programmatic-конструирование
// HttpConfig (например, в тестах) без явного ProxyRequestTimeout: обычная
// загрузка через config.Load всегда подставляет env-default.
const defaultProxyRequestTimeout = 10 * time.Second

type server struct {
	*httpserver.Server

	cfg      HttpConfig
	env      string
	log      *slog.Logger
	routes   *routeTable
	nc       *nats.Conn
	basePath string
	guard    *methodGuard
}

func newServer(cfg HttpConfig, env string, log *slog.Logger, routes *routeTable, nc *nats.Conn) *server {
	s := &server{cfg: cfg, env: env, log: log, routes: routes, nc: nc}
	s.basePath = strings.TrimSuffix(path.Join("/", cfg.BaseUrl), "/")
	s.guard = newMethodGuard(cfg.RateLimitRPS, cfg.RateLimitBurst, cfg.CircuitBreakerThreshold, cfg.CircuitBreakerCooldown)

	// healthz/readyz/_routes отвечают локально и быстро, поэтому делят общий
	// RequestTimeout; у проксирования — свой ProxyRequestTimeout (см. handleProxy),
	// не связанный с латентностью health-чеков, поэтому он не оборачивается здесь.
	static := func(h http.HandlerFunc) http.Handler {
		return httpserver.Timeout(h, cfg.RequestTimeout, "request timeout")
	}

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", static(s.handleHealth))
	mux.Handle("GET /readyz", static(s.handleReady))
	mux.Handle("GET "+s.basePath+"/_routes", static(s.handleRoutes))
	mux.HandleFunc(s.basePath+"/", s.handleProxy)

	s.Server = httpserver.New(httpserver.Config{
		Addr:                  cfg.Addr,
		TLSCert:               cfg.TLSCert,
		TLSKey:                cfg.TLSKey,
		ReadHeaderTimeout:     cfg.ReadHeaderTimeout,
		ReadTimeout:           cfg.ReadTimeout,
		IdleTimeout:           cfg.IdleTimeout,
		MaxHeaderBytes:        cfg.MaxHeaderBytes,
		MaxBodyBytes:          cfg.MaxBodyBytes,
		MaxConcurrentRequests: cfg.MaxConcurrentRequests,
		ShutdownDelay:         cfg.ShutdownDelay,
		Debug:                 cfg.Debug,
	}, log, mux)

	return s
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if !s.Ready() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *server) handleRoutes(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(s.routes.List()); err != nil {
		s.log.Warn("Failed to write routes response", "error", err)
	}
}

// handleProxy пересылает HTTP-запрос вида "/{package.Service}/{Method}"
// владеющему сервисом модулю через NATS request-reply и возвращает его ответ.
func (s *server) handleProxy(w http.ResponseWriter, r *http.Request) {
	methodPath := strings.TrimPrefix(r.URL.Path, s.basePath)

	serviceName, ok := parseServiceName(methodPath)
	if !ok {
		http.NotFound(w, r)
		return
	}

	rt, ok := s.routes.LookupService(serviceName)
	if !ok {
		http.Error(w, "service not found", http.StatusNotFound)
		return
	}

	switch s.guard.Before(extractClientIP(r, s.cfg.TrustProxyHeaders), methodPath) {
	case guardRateLimited:
		http.Error(w, "rate limit exceeded for this method", http.StatusTooManyRequests)
		return
	case guardCircuitOpen:
		http.Error(w, "circuit open: too many recent failures for this method", http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	timeout := s.cfg.ProxyRequestTimeout
	if timeout <= 0 {
		timeout = defaultProxyRequestTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	subject := natsrpc.RPCSubject(s.env, rt.Service, rt.Version, methodPath)
	reply, err := s.nc.RequestMsgWithContext(ctx, &nats.Msg{
		Subject: subject,
		Header:  nats.Header(r.Header.Clone()),
		Data:    body,
	})

	switch {
	case errors.Is(err, nats.ErrNoResponders):
		s.guard.After(methodPath, false)
		http.Error(w, "service instance unavailable", http.StatusServiceUnavailable)
		return
	case errors.Is(err, context.DeadlineExceeded):
		s.guard.After(methodPath, false)
		http.Error(w, "upstream request timed out", http.StatusGatewayTimeout)
		return
	case err != nil:
		s.guard.After(methodPath, false)
		s.log.Error("Failed to forward request over NATS", "subject", subject, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Брейкер реагирует только на отказы транспорта (выше): сам ответ
	// сервиса, даже 5xx, — не инфраструктурный отказ NATS, а бизнес-логика.
	s.guard.After(methodPath, true)
	writeProxyResponse(w, reply)
}

func writeProxyResponse(w http.ResponseWriter, reply *nats.Msg) {
	status := http.StatusOK
	if v := reply.Header.Get(natsrpc.RPCStatusHeader); v != "" {
		if code, err := strconv.Atoi(v); err == nil {
			status = code
		}
		reply.Header.Del(natsrpc.RPCStatusHeader)
	}

	for name, values := range reply.Header {
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(reply.Data)
}

// parseServiceName достаёт полное имя gRPC/Connect-сервиса из пути вида
// "/{package.Service}/{Method}".
func parseServiceName(p string) (string, bool) {
	trimmed := strings.TrimPrefix(p, "/")
	idx := strings.IndexByte(trimmed, '/')
	if idx <= 0 || idx == len(trimmed)-1 {
		return "", false
	}
	return trimmed[:idx], true
}

// extractClientIP возвращает адрес клиента для ключа лимитера (см.
// methodGuard). trustProxyHeaders — см. HttpConfig.TrustProxyHeaders:
// без доверенного реверс-прокси перед гейтвеем X-Forwarded-For/X-Real-IP
// — это просто то, что прислал сам клиент, и лимит по IP был бы
// тривиально обходим подстановкой нового значения на каждый запрос.
func extractClientIP(r *http.Request, trustProxyHeaders bool) string {
	if trustProxyHeaders {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Первый адрес в списке — исходный клиент, остальные добавляют
			// промежуточные прокси.
			if comma := strings.IndexByte(xff, ','); comma >= 0 {
				return strings.TrimSpace(xff[:comma])
			}
			return strings.TrimSpace(xff)
		}
		if xri := r.Header.Get("X-Real-IP"); xri != "" {
			return strings.TrimSpace(xri)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
