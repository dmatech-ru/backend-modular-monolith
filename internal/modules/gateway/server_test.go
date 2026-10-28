package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dmatech.ru/internal/pkg/natsrpc"
)

func testServer(cfg HttpConfig) (*server, *routeTable) {
	routes := newRouteTable()
	return newServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), routes), routes
}

func serve(s *server, method, target string, body io.Reader) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, httptest.NewRequest(method, target, body))
	return rec
}

func TestRouteTable(t *testing.T) {
	table := newRouteTable()
	table.Apply(natsrpc.RouteUpdate{ServiceName: "b", Version: "1_0_0", Descriptor: []byte("xx")})
	table.Apply(natsrpc.RouteUpdate{ServiceName: "a", Version: "2_0_0", Descriptor: []byte("x")})
	table.Apply(natsrpc.RouteUpdate{ServiceName: "a", Version: "1_0_0", Descriptor: []byte("x")})
	table.Apply(natsrpc.RouteUpdate{ServiceName: "a", Version: "2_0_0", IsDeleted: true})

	list := table.List()
	if len(list) != 2 || list[0].Service != "a" || list[1].Service != "b" || list[1].Size != 2 {
		t.Fatalf("unexpected routes %+v", list)
	}
}

func TestHealthAndReadiness(t *testing.T) {
	s, _ := testServer(HttpConfig{BaseUrl: "/api"})

	if rec := serve(s, http.MethodGet, "/healthz", nil); rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d", rec.Code)
	}
	if rec := serve(s, http.MethodGet, "/readyz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before start = %d", rec.Code)
	}
	s.ready.Store(true)
	if rec := serve(s, http.MethodGet, "/readyz", nil); rec.Code != http.StatusOK {
		t.Fatalf("readyz after start = %d", rec.Code)
	}
}

func TestRoutesEndpointAndProxyStub(t *testing.T) {
	s, routes := testServer(HttpConfig{BaseUrl: "api/"})
	routes.Apply(natsrpc.RouteUpdate{ServiceName: "identity", Version: "1_0_0", Descriptor: []byte("d")})

	rec := serve(s, http.MethodGet, "/api/_routes", nil)
	var got []route
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != 1 || got[0].Service != "identity" {
		t.Fatalf("routes response %d %q: %v", rec.Code, rec.Body.String(), err)
	}

	if rec := serve(s, http.MethodPost, "/api/identity/login", nil); rec.Code != http.StatusNotImplemented {
		t.Fatalf("proxy stub = %d", rec.Code)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	s := &server{cfg: HttpConfig{MaxConcurrentRequests: 1}}
	release := make(chan struct{})
	entered := make(chan struct{})
	handler := s.limitConcurrency(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
	}))

	go handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	<-entered

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	close(release)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("second request = %d, want 503", rec.Code)
	}
}

func TestBodyLimit(t *testing.T) {
	s := &server{cfg: HttpConfig{MaxBodyBytes: 4}}
	handler := s.limitBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		}
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("too large")))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestStartAndShutdown(t *testing.T) {
	s, _ := testServer(HttpConfig{Addr: "127.0.0.1:0", ShutdownDelay: 10 * time.Millisecond})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if !s.ready.Load() {
		t.Fatal("server must be ready after start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if s.ready.Load() {
		t.Fatal("server must not be ready after shutdown")
	}
}

func TestStartRejectsPartialTLS(t *testing.T) {
	s, _ := testServer(HttpConfig{Addr: "127.0.0.1:0", TLSCert: "cert.pem"})
	if err := s.Start(); err == nil {
		t.Fatal("expected error for TLS cert without key")
	}
}
