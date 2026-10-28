package service

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
)

func reset(t *testing.T) {
	t.Helper()
	mu.Lock()
	modules, shutdowns, deps = nil, nil, map[reflect.Type]any{}
	mu.Unlock()
}

func TestLoadStopsOnFirstError(t *testing.T) {
	reset(t)
	var calls []int
	boom := errors.New("boom")

	Register(func(context.Context) error { calls = append(calls, 1); return nil })
	Register(func(context.Context) error { calls = append(calls, 2); return boom })
	Register(func(context.Context) error { calls = append(calls, 3); return nil })

	if err := Load(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Load error = %v, want %v", err, boom)
	}
	if !slices.Equal(calls, []int{1, 2}) {
		t.Fatalf("calls = %v, want [1 2]", calls)
	}
}

func TestShutdownRunsInReverseOrderAndJoinsErrors(t *testing.T) {
	reset(t)
	var calls []int
	errA, errB := errors.New("a"), errors.New("b")

	OnShutdown(func(context.Context) error { calls = append(calls, 1); return errA })
	OnShutdown(func(context.Context) error { calls = append(calls, 2); return nil })
	OnShutdown(func(context.Context) error { calls = append(calls, 3); return errB })

	err := Shutdown(context.Background())
	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Fatalf("Shutdown error = %v, want both errors", err)
	}
	if !slices.Equal(calls, []int{3, 2, 1}) {
		t.Fatalf("calls = %v, want [3 2 1]", calls)
	}

	// Хуки выполняются один раз.
	if err := Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown = %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("hooks ran twice: %v", calls)
	}
}

func TestProvideGet(t *testing.T) {
	reset(t)
	type conn struct{ name string }

	if _, err := Get[conn](); err == nil {
		t.Fatal("expected error for missing dependency")
	}

	c := &conn{name: "nats"}
	Provide(c)
	got, err := Get[conn]()
	if err != nil || got != c {
		t.Fatalf("Get = %v, %v", got, err)
	}
}
