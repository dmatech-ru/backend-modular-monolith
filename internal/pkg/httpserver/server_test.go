package httpserver

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestLimitBody(t *testing.T) {
	s := New(Config{MaxBodyBytes: 4}, discardLogger(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("12345"))
	s.httpSrv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized body, got %d", rec.Code)
	}
}

func TestLimitConcurrency(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 2)

	s := New(Config{MaxConcurrentRequests: 1}, discardLogger(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))

	done := make(chan *httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		go func() {
			rec := httptest.NewRecorder()
			s.httpSrv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			done <- rec
		}()
	}

	<-started
	time.Sleep(20 * time.Millisecond) // дать второму запросу упереться в лимит

	rec1 := <-done
	if rec1.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected second request to be rejected with 503, got %d", rec1.Code)
	}

	close(release)
	rec2 := <-done
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected first request to succeed, got %d", rec2.Code)
	}
}

func TestTimeout(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	rec := httptest.NewRecorder()
	Timeout(ok, 0, "too slow").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("d<=0 should pass through unmodified, got %d", rec.Code)
	}

	slow := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })

	rec = httptest.NewRecorder()
	Timeout(slow, 10*time.Millisecond, "too slow").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if rec.Body.String() != "too slow" {
		t.Fatalf("body = %q, want %q", rec.Body.String(), "too slow")
	}
}

func TestReadyToggles(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"}, discardLogger(), http.NotFoundHandler())
	if s.Ready() {
		t.Fatal("server should not be ready before Start")
	}
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !s.Ready() {
		t.Fatal("server should be ready after Start")
	}
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if s.Ready() {
		t.Fatal("server should not be ready after Shutdown")
	}
}
