package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"sync/atomic"
	"time"
)

type server struct {
	cfg     HttpConfig
	log     *slog.Logger
	routes  *routeTable
	httpSrv *http.Server
	ready   atomic.Bool
	served  chan struct{}
}

func newServer(cfg HttpConfig, log *slog.Logger, routes *routeTable) *server {
	s := &server{cfg: cfg, log: log, routes: routes, served: make(chan struct{})}

	base := strings.TrimSuffix(path.Join("/", cfg.BaseUrl), "/")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET "+base+"/_routes", s.handleRoutes)
	mux.HandleFunc(base+"/", s.handleProxy)

	var handler http.Handler = mux
	if cfg.RequestTimeout > 0 {
		handler = http.TimeoutHandler(handler, cfg.RequestTimeout, "request timeout")
	}
	handler = s.limitBody(handler)
	handler = s.limitConcurrency(handler)
	if cfg.Debug {
		handler = s.logRequests(handler)
	}

	s.httpSrv = &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
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
func (s *server) Start() error {
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

// Shutdown снимает готовность, выжидает ShutdownDelay и плавно останавливает сервер.
func (s *server) Shutdown(ctx context.Context) error {
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

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
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

// handleProxy — точка подключения проксирования на основе дескрипторов.
func (s *server) handleProxy(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "proxying is not implemented yet", http.StatusNotImplemented)
}

func (s *server) limitBody(next http.Handler) http.Handler {
	if s.cfg.MaxBodyBytes <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

func (s *server) limitConcurrency(next http.Handler) http.Handler {
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

func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		s.log.Debug("HTTP request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
	})
}
