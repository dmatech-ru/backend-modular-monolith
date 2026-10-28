package setup

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func reset(t *testing.T) {
	t.Helper()
	mu.Lock()
	funcs = nil
	mu.Unlock()
}

func TestRunStopsOnFirstError(t *testing.T) {
	reset(t)
	var calls []int
	boom := errors.New("boom")

	Register(func(context.Context) error { calls = append(calls, 1); return nil })
	Register(func(context.Context) error { calls = append(calls, 2); return boom })
	Register(func(context.Context) error { calls = append(calls, 3); return nil })

	if err := Run(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Run error = %v, want %v", err, boom)
	}
	if !slices.Equal(calls, []int{1, 2}) {
		t.Fatalf("calls = %v, want [1 2]", calls)
	}
}

func TestRunExecutesAllOnSuccess(t *testing.T) {
	reset(t)
	var calls []int

	Register(func(context.Context) error { calls = append(calls, 1); return nil })
	Register(func(context.Context) error { calls = append(calls, 2); return nil })

	if err := Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.Equal(calls, []int{1, 2}) {
		t.Fatalf("calls = %v, want [1 2]", calls)
	}
}
