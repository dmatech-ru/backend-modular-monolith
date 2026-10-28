package natsrpc

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClient_RoundTrip(t *testing.T) {
	nc := connectTestNATS(t)
	log := discardLogger()
	env, service, version := uniqueEnv(), "echo", "1.0.0"

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pkg.Service/Method" {
			t.Errorf("unexpected path on server side: %q", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/custom" {
			t.Errorf("unexpected content-type forwarded: %q", ct)
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/custom")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("echo:" + string(body)))
	})

	sub, err := ServeHandler(nc, log, env, service, version, handler)
	if err != nil {
		t.Fatalf("ServeHandler: %v", err)
	}
	defer sub.Close(context.Background())

	client := NewClient(nc, env, service, version, 2*time.Second)
	req, err := http.NewRequest(http.MethodPost, "/pkg.Service/Method", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/custom")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/custom" {
		t.Fatalf("content-type = %q", ct)
	}
	if resp.Header.Get(RPCStatusHeader) != "" {
		t.Fatal("internal status header leaked into the response")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "echo:hello" {
		t.Fatalf("body = %q", body)
	}
}

func TestClient_NoResponders(t *testing.T) {
	nc := connectTestNATS(t)
	client := NewClient(nc, uniqueEnv(), "nobody-home", "1.0.0", time.Second)

	req, err := http.NewRequest(http.MethodPost, "/pkg.Service/Method", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	if _, err := client.Do(req); err == nil {
		t.Fatal("expected an error when there are no live responders")
	}
}
