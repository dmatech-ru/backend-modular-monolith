package natsdiscovery

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nuid"
)

// Интеграционные тесты требуют NATS с JetStream:
//
//	NATS_TEST_URL=nats://localhost:4222 go test ./internal/pkg/natsdiscovery/
func connectTestNATS(t *testing.T) (*nats.Conn, jetstream.JetStream) {
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
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return nc, js
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// uniqueEnv изолирует тесты друг от друга в общих бакетах.
func uniqueEnv() string {
	return "test" + nuid.Next()
}

type updateRecorder struct {
	mu      sync.Mutex
	updates []RouteUpdate
}

func (r *updateRecorder) record(u RouteUpdate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = append(r.updates, u)
}

func (r *updateRecorder) snapshot() []RouteUpdate {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RouteUpdate(nil), r.updates...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRoutesBucketHasNoTTL(t *testing.T) {
	nc, js := connectTestNATS(t)
	ctx := t.Context()

	reg, err := RegisterDescriptors(ctx, nc, discardLogger(), uniqueEnv(), "svc", "1.0.0", []byte("d"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer reg.Close(ctx)

	routes, _ := js.KeyValue(ctx, RoutesBucket)
	nodes, _ := js.KeyValue(ctx, NodesBucket)
	routesStatus, _ := routes.Status(ctx)
	nodesStatus, _ := nodes.Status(ctx)

	if ttl := routesStatus.TTL(); ttl != 0 {
		t.Errorf("routes bucket TTL = %v, want 0", ttl)
	}
	if ttl := nodesStatus.TTL(); ttl != NodeHeartbeatTTL {
		t.Errorf("nodes bucket TTL = %v, want %v", ttl, NodeHeartbeatTTL)
	}
}

func TestLastNodeRemovesRoute(t *testing.T) {
	nc, js := connectTestNATS(t)
	ctx := t.Context()
	env := uniqueEnv()
	log := discardLogger()

	first, err := RegisterDescriptors(ctx, nc, log, env, "identity", "1.0.0", []byte("v1"))
	if err != nil {
		t.Fatalf("register first: %v", err)
	}
	second, err := RegisterDescriptors(ctx, nc, log, env, "identity", "1.0.0", []byte("v1"))
	if err != nil {
		t.Fatalf("register second: %v", err)
	}

	routes, _ := js.KeyValue(ctx, RoutesBucket)
	key := routeKey(env, "identity", "1_0_0")

	if err := first.Close(ctx); err != nil {
		t.Fatalf("close first: %v", err)
	}
	if _, err := routes.Get(ctx, key); err != nil {
		t.Fatalf("route must survive while another node is alive: %v", err)
	}

	if err := second.Close(ctx); err != nil {
		t.Fatalf("close second: %v", err)
	}
	if _, err := routes.Get(ctx, key); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Fatalf("route must be removed by the last node, got err=%v", err)
	}

	// Повторный Close безопасен.
	if err := second.Close(ctx); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestRouteIsRestoredWhileNodeIsAlive(t *testing.T) {
	nc, js := connectTestNATS(t)
	ctx := t.Context()
	env := uniqueEnv()

	reg, err := RegisterDescriptors(ctx, nc, discardLogger(), env, "svc", "2.0.0", []byte("v2"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer reg.Close(ctx)

	routes, _ := js.KeyValue(ctx, RoutesBucket)
	key := routeKey(env, "svc", "2_0_0")
	if err := routes.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}

	deadline := time.Now().Add(2 * heartbeatInterval)
	for {
		entry, err := routes.Get(ctx, key)
		if err == nil {
			if string(entry.Value()) != "v2" {
				t.Fatalf("restored descriptor = %q", entry.Value())
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("route was not restored: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestWatchRoutes(t *testing.T) {
	nc, _ := connectTestNATS(t)
	ctx := t.Context()
	env := uniqueEnv()
	log := discardLogger()

	// Гейтвей стартует первым: бакета ещё может не быть.
	rec := &updateRecorder{}
	watcher, err := WatchRoutes(ctx, nc, log, env, rec.record)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	first, err := RegisterDescriptors(ctx, nc, log, env, "identity", "1.0.0", []byte("v1"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	waitFor(t, "initial update", func() bool { return len(rec.snapshot()) == 1 })

	// Реплика с тем же дескриптором не должна порождать событие.
	second, err := RegisterDescriptors(ctx, nc, log, env, "identity", "1.0.0", []byte("v1"))
	if err != nil {
		t.Fatalf("register replica: %v", err)
	}
	// Новый дескриптор той же версии — должен.
	third, err := RegisterDescriptors(ctx, nc, log, env, "identity", "1.0.0", []byte("v1-fixed"))
	if err != nil {
		t.Fatalf("register rebuilt: %v", err)
	}
	waitFor(t, "descriptor change", func() bool { return len(rec.snapshot()) == 2 })

	for _, reg := range []*Registration{first, second, third} {
		if err := reg.Close(ctx); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	waitFor(t, "delete update", func() bool { return len(rec.snapshot()) == 3 })

	if err := watcher.Close(ctx); err != nil {
		t.Fatalf("close watcher: %v", err)
	}

	got := rec.snapshot()
	want := []RouteUpdate{
		{ServiceName: "identity", Version: "1_0_0", Descriptor: []byte("v1")},
		{ServiceName: "identity", Version: "1_0_0", Descriptor: []byte("v1-fixed")},
		{ServiceName: "identity", Version: "1_0_0", IsDeleted: true},
	}
	for i := range want {
		if got[i].ServiceName != want[i].ServiceName || got[i].Version != want[i].Version ||
			string(got[i].Descriptor) != string(want[i].Descriptor) || got[i].IsDeleted != want[i].IsDeleted {
			t.Errorf("update #%d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Падение без graceful shutdown: хартбит удаляется напрямую (имитирует
// истечение TTL), Close (и тем самым деregister маршрута) никогда не
// вызывается — ровно пробел, который должен закрывать ReapStaleRoutes.
func TestReapStaleRoutes_RemovesRouteWithoutLiveNodes(t *testing.T) {
	nc, js := connectTestNATS(t)
	ctx := t.Context()
	env := uniqueEnv()
	log := discardLogger()

	reg, err := RegisterDescriptors(ctx, nc, log, env, "identity", "1.0.0", []byte("v1"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	nodes, _ := js.KeyValue(ctx, NodesBucket)
	if err := nodes.Delete(ctx, reg.nodeKey); err != nil {
		t.Fatalf("delete node heartbeat: %v", err)
	}

	routes, _ := js.KeyValue(ctx, RoutesBucket)
	key := routeKey(env, "identity", "1_0_0")
	if _, err := routes.Get(ctx, key); err != nil {
		t.Fatalf("route should still exist before reaping: %v", err)
	}

	if err := ReapStaleRoutes(ctx, nc, log, env); err != nil {
		t.Fatalf("ReapStaleRoutes: %v", err)
	}

	if _, err := routes.Get(ctx, key); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Fatalf("expected stale route to be removed, got err=%v", err)
	}
}

func TestReapStaleRoutes_KeepsRouteWithLiveNode(t *testing.T) {
	nc, js := connectTestNATS(t)
	ctx := t.Context()
	env := uniqueEnv()
	log := discardLogger()

	reg, err := RegisterDescriptors(ctx, nc, log, env, "identity", "1.0.0", []byte("v1"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer reg.Close(ctx)

	if err := ReapStaleRoutes(ctx, nc, log, env); err != nil {
		t.Fatalf("ReapStaleRoutes: %v", err)
	}

	routes, _ := js.KeyValue(ctx, RoutesBucket)
	key := routeKey(env, "identity", "1_0_0")
	if _, err := routes.Get(ctx, key); err != nil {
		t.Fatalf("route with a live node must survive reaping: %v", err)
	}
}

func TestRegisterValidatesInput(t *testing.T) {
	ctx := context.Background()
	if _, err := RegisterDescriptors(ctx, nil, discardLogger(), "", "svc", "1.0.0", nil); err == nil {
		t.Fatal("expected error for empty env")
	}
	if _, err := WatchRoutes(ctx, nil, discardLogger(), "", nil); err == nil {
		t.Fatal("expected error for empty env")
	}
}
