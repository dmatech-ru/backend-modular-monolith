package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	authv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity"
	"dmatech.ru/internal/pkg/natsdiscovery"
	"dmatech.ru/internal/pkg/natsrpc"
	"dmatech.ru/internal/pkg/protoset"
)

const testEnv = "test"

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testServer(cfg HttpConfig, nc *nats.Conn) (*server, *routeTable) {
	log := discardLogger()
	routes := newRouteTable(log)
	return newServer(cfg, testEnv, log, routes, nc), routes
}

func serve(s *server, method, target string, body io.Reader) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(method, target, body))
	return rec
}

func identityDescriptor(t *testing.T) []byte {
	t.Helper()
	descriptor, err := protoset.Build(authv1.File_webapi_v1_identity_auth_proto)
	if err != nil {
		t.Fatalf("build descriptor: %v", err)
	}
	return descriptor
}

func TestRouteTable(t *testing.T) {
	table := newRouteTable(discardLogger())
	table.Apply(natsdiscovery.RouteUpdate{ServiceName: "b", Version: "1_0_0", Descriptor: []byte("xx")})
	table.Apply(natsdiscovery.RouteUpdate{ServiceName: "a", Version: "2_0_0", Descriptor: []byte("x")})
	table.Apply(natsdiscovery.RouteUpdate{ServiceName: "a", Version: "1_0_0", Descriptor: []byte("x")})
	table.Apply(natsdiscovery.RouteUpdate{ServiceName: "a", Version: "2_0_0", IsDeleted: true})

	list := table.List()
	if len(list) != 2 || list[0].Service != "a" || list[1].Service != "b" || list[1].Size != 2 {
		t.Fatalf("unexpected routes %+v", list)
	}
}

func TestRouteTable_LookupService(t *testing.T) {
	table := newRouteTable(discardLogger())
	table.Apply(natsdiscovery.RouteUpdate{ServiceName: "identity", Version: "1_0_0", Descriptor: identityDescriptor(t)})

	rt, ok := table.LookupService("webapi.identity.auth.v1.IdentityAuthService")
	if !ok || rt.Service != "identity" || rt.Version != "1_0_0" {
		t.Fatalf("unexpected lookup result: %+v, ok=%v", rt, ok)
	}

	table.Apply(natsdiscovery.RouteUpdate{ServiceName: "identity", Version: "1_0_0", IsDeleted: true})
	if _, ok := table.LookupService("webapi.identity.auth.v1.IdentityAuthService"); ok {
		t.Fatal("expected service index to be cleared after route deletion")
	}
}

func TestHealthAndReadiness(t *testing.T) {
	s, _ := testServer(HttpConfig{BaseUrl: "/api", Addr: "127.0.0.1:0"}, nil)

	if rec := serve(s, http.MethodGet, "/healthz", nil); rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d", rec.Code)
	}
	if rec := serve(s, http.MethodGet, "/readyz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before start = %d", rec.Code)
	}

	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Shutdown(context.Background())

	if rec := serve(s, http.MethodGet, "/readyz", nil); rec.Code != http.StatusOK {
		t.Fatalf("readyz after start = %d", rec.Code)
	}
}

func TestRoutesEndpointAndProxyNotFound(t *testing.T) {
	s, routes := testServer(HttpConfig{BaseUrl: "api/"}, nil)
	routes.Apply(natsdiscovery.RouteUpdate{ServiceName: "identity", Version: "1_0_0", Descriptor: nil})

	rec := serve(s, http.MethodGet, "/api/_routes", nil)
	var got []route
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != 1 || got[0].Service != "identity" {
		t.Fatalf("routes response %d %q: %v", rec.Code, rec.Body.String(), err)
	}

	// Дескриптор не был распарсен (пустой), поэтому сервис не проиндексирован.
	if rec := serve(s, http.MethodPost, "/api/webapi.identity.auth.v1.IdentityAuthService/InitializeSession", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("proxy for unknown service = %d", rec.Code)
	}

	if rec := serve(s, http.MethodGet, "/api/", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("malformed proxy path = %d", rec.Code)
	}
}

// Интеграционные тесты ниже требуют NATS:
//
//	NATS_TEST_URL=nats://localhost:4222 go test ./internal/modules/gateway/
func connectTestNATS(t *testing.T) *nats.Conn {
	t.Helper()
	url := os.Getenv("NATS_TEST_URL")
	if url == "" {
		t.Skip("NATS_TEST_URL is not set")
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func TestHandleProxy_RoundTrip(t *testing.T) {
	nc := connectTestNATS(t)

	s, routes := testServer(HttpConfig{BaseUrl: "/api", ProxyRequestTimeout: time.Second}, nc)
	routes.Apply(natsdiscovery.RouteUpdate{ServiceName: "identity", Version: "1_0_0", Descriptor: identityDescriptor(t)})

	const methodPath = "/webapi.identity.auth.v1.IdentityAuthService/InitializeSession"
	subject := natsrpc.RPCSubject(testEnv, "identity", "1_0_0", methodPath)

	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		if ct := msg.Header.Get("Content-Type"); ct != "application/custom" {
			t.Errorf("unexpected content type forwarded: %q", ct)
		}
		reply := nats.NewMsg(msg.Reply)
		reply.Header.Set(natsrpc.RPCStatusHeader, "201")
		reply.Header.Set("Content-Type", "application/custom")
		reply.Data = []byte("echo:" + string(msg.Data))
		if err := msg.RespondMsg(reply); err != nil {
			t.Errorf("respond: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	req := httptest.NewRequest(http.MethodPost, "/api"+methodPath, strings.NewReader("hello"))
	req.Header.Set("Content-Type", "application/custom")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "echo:hello" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/custom" {
		t.Fatalf("content-type = %q", ct)
	}
	if rec.Header().Get(natsrpc.RPCStatusHeader) != "" {
		t.Fatal("internal status header leaked to client")
	}
}

// TestHandleProxy_ProxyTimeoutIsIndependentOfStaticRequestTimeout — регрессия:
// до рефакторинга handleProxy оборачивался тем же http.TimeoutHandler, что и
// healthz/readyz/_routes, и короткий общий RequestTimeout обрезал запрос
// раньше, чем успевал сработать выделенный ProxyRequestTimeout.
func TestHandleProxy_ProxyTimeoutIsIndependentOfStaticRequestTimeout(t *testing.T) {
	nc := connectTestNATS(t)

	s, routes := testServer(HttpConfig{
		BaseUrl:             "/api",
		RequestTimeout:      20 * time.Millisecond,
		ProxyRequestTimeout: 2 * time.Second,
	}, nc)
	routes.Apply(natsdiscovery.RouteUpdate{ServiceName: "identity", Version: "1_0_0", Descriptor: identityDescriptor(t)})

	const methodPath = "/webapi.identity.auth.v1.IdentityAuthService/InitializeSession"
	subject := natsrpc.RPCSubject(testEnv, "identity", "1_0_0", methodPath)

	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		time.Sleep(100 * time.Millisecond) // дольше, чем короткий RequestTimeout выше
		reply := nats.NewMsg(msg.Reply)
		reply.Header.Set(natsrpc.RPCStatusHeader, "200")
		_ = msg.RespondMsg(reply)
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	req := httptest.NewRequest(http.MethodPost, "/api"+methodPath, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (proxy must not be cut short by the static RequestTimeout), body=%q", rec.Code, rec.Body.String())
	}
}

func TestHandleProxy_NoResponders(t *testing.T) {
	nc := connectTestNATS(t)

	s, routes := testServer(HttpConfig{BaseUrl: "/api", ProxyRequestTimeout: time.Second}, nc)
	routes.Apply(natsdiscovery.RouteUpdate{ServiceName: "identity", Version: "1_0_0", Descriptor: identityDescriptor(t)})

	const methodPath = "/webapi.identity.auth.v1.IdentityAuthService/InitializeSession"
	req := httptest.NewRequest(http.MethodPost, "/api"+methodPath, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%q", rec.Code, rec.Body.String())
	}
}
