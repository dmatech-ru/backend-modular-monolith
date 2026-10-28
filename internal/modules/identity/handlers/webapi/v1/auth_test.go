package v1

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	identityv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity"
)

type stubLookup struct {
	exists bool
	err    error
	got    string
}

func (s *stubLookup) Exists(_ context.Context, identifier string) (bool, error) {
	s.got = identifier
	return s.exists, s.err
}

func TestInitializeSession_Exists(t *testing.T) {
	lookup := &stubLookup{exists: true}
	h := NewAuthHandler(lookup)

	resp, err := h.InitializeSession(context.Background(), connect.NewRequest(&identityv1.InitializeSessionRequest{
		Identifier: "  user@example.com  ",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Msg.GetExists() {
		t.Fatalf("expected exists=true")
	}
	if lookup.got != "user@example.com" {
		t.Fatalf("expected trimmed identifier, got %q", lookup.got)
	}
}

func TestInitializeSession_NotExists(t *testing.T) {
	lookup := &stubLookup{exists: false}
	h := NewAuthHandler(lookup)

	resp, err := h.InitializeSession(context.Background(), connect.NewRequest(&identityv1.InitializeSessionRequest{
		Identifier: "unknown",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Msg.GetExists() {
		t.Fatalf("expected exists=false")
	}
}

func TestInitializeSession_EmptyIdentifier(t *testing.T) {
	h := NewAuthHandler(&stubLookup{})

	_, err := h.InitializeSession(context.Background(), connect.NewRequest(&identityv1.InitializeSessionRequest{
		Identifier: "   ",
	}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("expected CodeInvalidArgument, got %v", err)
	}
}

func TestInitializeSession_LookupError(t *testing.T) {
	lookup := &stubLookup{err: errors.New("boom")}
	h := NewAuthHandler(lookup)

	_, err := h.InitializeSession(context.Background(), connect.NewRequest(&identityv1.InitializeSessionRequest{
		Identifier: "user@example.com",
	}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInternal {
		t.Fatalf("expected CodeInternal, got %v", err)
	}
}
