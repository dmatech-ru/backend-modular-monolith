// Package httpserver — переиспользуемый HTTP-сервер для модулей монолита:
// таймауты, ограничение размера тела и числа параллельных запросов,
// опциональный TLS и плавная остановка с паузой перед закрытием слушателя.
package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Config — параметры HTTP-сервера.
type Config struct {
	Addr    string
	TLSCert string
	TLSKey  string

	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	IdleTimeout       time.Duration
	// RequestTimeout оборачивает весь handler, переданный в New, в
	// http.TimeoutHandler. Подходит для простого случая "один таймаут на
	// все маршруты"; если разным маршрутам нужны разные таймауты (например,
	// проксированию на медленный backend — больший, чем у health-чеков),
	// оставьте это поле нулевым и примените Timeout к нужным маршрутам
	// самостоятельно до сборки mux.
	RequestTimeout time.Duration

	// MaxBodyBytes ограничивает размер тела запроса; <=0 — без ограничения.
	MaxBodyBytes int64
	// MaxConcurrentRequests ограничивает число параллельно обрабатываемых
	// запросов; <=0 — без ограничения.
	MaxConcurrentRequests int
	MaxHeaderBytes        int

	// ShutdownDelay — пауза между снятием готовности и остановкой приёма
	// соединений, чтобы балансировщик успел убрать инстанс из ротации.
	ShutdownDelay time.Duration

	// Debug включает логирование каждого запроса.
	Debug bool
}

// Server — HTTP-сервер со стандартным набором защитных middleware
// (таймаут обработки, лимит тела запроса, лимит конкурентности).
type Server struct {
	cfg     Config
	log     *slog.Logger
	httpSrv *http.Server
	ready   atomic.Bool
	served  chan struct{}
}

// New оборачивает handler защитными middleware и готовит сервер к запуску.
func New(cfg Config, log *slog.Logger, handler http.Handler) *Server {
	s := &Server{cfg: cfg, log: log, served: make(chan struct{})}

	wrapped := handler
	if cfg.Debug {
		wrapped = s.logRequests(wrapped)
	}
	if cfg.RequestTimeout > 0 {
		wrapped = http.TimeoutHandler(wrapped, cfg.RequestTimeout, "request timeout")
	}
	wrapped = s.limitBody(wrapped)
	wrapped = s.limitConcurrency(wrapped)

	s.httpSrv = &http.Server{
		Addr:              cfg.Addr,
		Handler:           wrapped,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return s
}

// Start открывает порт синхронно (ошибка привязки возвращается сразу)
// и обслуживает соединения в фоне.
func (s *Server) Start() error {
	if (s.cfg.TLSCert == "") != (s.cfg.TLSKey == "") {
		return errors.New("both tlsCertFile and tlsKeyFile must be set to enable TLS")
	}

	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return err
	}

	go func() {
		defer close(s.served)
		var err error
		if s.cfg.TLSCert != "" {
			err = s.httpSrv.ServeTLS(ln, s.cfg.TLSCert, s.cfg.TLSKey)
		} else {
			err = s.httpSrv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("HTTP server stopped unexpectedly", "error", err)
		}
	}()

	s.ready.Store(true)
	return nil
}

// Ready сообщает, принимает ли сервер запросы (например, для /readyz).
func (s *Server) Ready() bool {
	return s.ready.Load()
}

// Handler возвращает итоговый http.Handler с учётом всех middleware —
// пригодно для тестирования без реального сетевого listener'а.
func (s *Server) Handler() http.Handler {
	return s.httpSrv.Handler
}

// Timeout оборачивает handler в http.TimeoutHandler: если handler не
// успевает ответить за d, клиенту отправляется 503 с текстом msg, а d<=0
// возвращает handler без изменений. В отличие от Config.RequestTimeout,
// применяется к конкретному маршруту, а не ко всему серверу — полезно, когда
// у разных маршрутов должны быть разные таймауты.
func Timeout(handler http.Handler, d time.Duration, msg string) http.Handler {
	if d <= 0 {
		return handler
	}
	return http.TimeoutHandler(handler, d, msg)
}

// Shutdown снимает готовность, выжидает ShutdownDelay и плавно останавливает сервер.
func (s *Server) Shutdown(ctx context.Context) error {
	s.ready.Store(false)

	if s.cfg.ShutdownDelay > 0 {
		timer := time.NewTimer(s.cfg.ShutdownDelay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
		}
	}

	err := s.httpSrv.Shutdown(ctx)
	select {
	case <-s.served:
	case <-ctx.Done():
	}
	return err
}

func (s *Server) limitBody(next http.Handler) http.Handler {
	if s.cfg.MaxBodyBytes <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) limitConcurrency(next http.Handler) http.Handler {
	if s.cfg.MaxConcurrentRequests <= 0 {
		return next
	}
	slots := make(chan struct{}, s.cfg.MaxConcurrentRequests)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "too many concurrent requests", http.StatusServiceUnavailable)
		}
	})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		s.log.Debug("HTTP request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
	})
}
