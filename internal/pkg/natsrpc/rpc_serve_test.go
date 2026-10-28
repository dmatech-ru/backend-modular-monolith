package natsrpc

import (
	"io"
	"net/http"
	"testing"
	"time"
)

// Интеграционный тест требует NATS:
//
//	NATS_TEST_URL=nats://localhost:4222 go test ./internal/pkg/natsrpc/
func TestServeHandler_RoundTrip(t *testing.T) {
	nc := connectTestNATS(t)
	log := discardLogger()

	env, serviceName, version := uniqueEnv(), "identity", "1_0_0"

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/webapi.identity.auth.v1.IdentityAuthService/InitializeSession" {
			t.Errorf("unexpected method path: %q", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/custom" {
			t.Errorf("unexpected content type: %q", ct)
		}
		body, _ := io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "application/custom")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("echo:" + string(body)))
	})

	sub, err := ServeHandler(nc, log, env, serviceName, version, handler)
	if err != nil {
		t.Fatalf("ServeHandler: %v", err)
	}
	defer sub.Close(t.Context())

	subject := RPCSubject(env, serviceName, version, "/webapi.identity.auth.v1.IdentityAuthService/InitializeSession")
	reply, err := nc.Request(subject, []byte("hello"), 2*time.Second)
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	if status := reply.Header.Get(RPCStatusHeader); status != "201" {
		t.Fatalf("status header = %q, want 201", status)
	}
	if ct := reply.Header.Get("Content-Type"); ct != "application/custom" {
		t.Fatalf("content-type = %q", ct)
	}
	if string(reply.Data) != "echo:hello" {
		t.Fatalf("body = %q", reply.Data)
	}
}

func TestServeHandler_NoReplySubjectIsDropped(t *testing.T) {
	nc := connectTestNATS(t)
	log := discardLogger()

	env, serviceName, version := uniqueEnv(), "identity", "1_0_0"

	called := make(chan struct{}, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- struct{}{}
		w.WriteHeader(http.StatusOK)
	})

	sub, err := ServeHandler(nc, log, env, serviceName, version, handler)
	if err != nil {
		t.Fatalf("ServeHandler: %v", err)
	}
	defer sub.Close(t.Context())

	subject := RPCSubject(env, serviceName, version, "/pkg.Service/Method")
	if err := nc.Publish(subject, []byte("fire-and-forget")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	select {
	case <-called:
		t.Fatal("handler should not be invoked without a reply subject")
	case <-time.After(200 * time.Millisecond):
	}
}

// Любой путь под подпиской "...rpc.>" доходит до handler.ServeHTTP — маршрутизация
// по конкретным методам (404 на неизвестных) остаётся задачей самого handler'а.
func TestServeHandler_UnknownMethodStillDispatches(t *testing.T) {
	nc := connectTestNATS(t)
	log := discardLogger()

	env, serviceName, version := uniqueEnv(), "identity", "1_0_0"

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	sub, err := ServeHandler(nc, log, env, serviceName, version, handler)
	if err != nil {
		t.Fatalf("ServeHandler: %v", err)
	}
	defer sub.Close(t.Context())

	subject := RPCSubject(env, serviceName, version, "/unknown.Service/Method")
	reply, err := nc.Request(subject, nil, 2*time.Second)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if status := reply.Header.Get(RPCStatusHeader); status != "404" {
		t.Fatalf("status header = %q, want 404", status)
	}
}
