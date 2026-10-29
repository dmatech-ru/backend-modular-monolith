package v1

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"connectrpc.com/connect"

	"dmatech.ru/internal/modules/identity/repository"
	"dmatech.ru/internal/pkg/authctx"
	sessionv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity/session"
)

func requestWithRefreshCookie[T any](msg *T, value string) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Cookie", (&http.Cookie{Name: authctx.CookieRefreshToken, Value: value}).String())
	return req
}

func TestSessionHandler_Refresh_MissingCookie(t *testing.T) {
	h := NewSessionHandler(newFakeSessions())

	_, err := h.Refresh(context.Background(), connect.NewRequest(&sessionv1.RefreshRequest{}))
	if code := connectCode(t, err); code != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want CodeUnauthenticated", code)
	}
	if key := errorKey(t, err); key != "identity.refresh_token.missing" {
		t.Fatalf("key = %q, want %q", key, "identity.refresh_token.missing")
	}
}

func TestSessionHandler_Refresh_InvalidToken(t *testing.T) {
	sessions := newFakeSessions()
	sessions.rotateErr = repository.ErrNotFound
	h := NewSessionHandler(sessions)

	_, err := h.Refresh(context.Background(), requestWithRefreshCookie(&sessionv1.RefreshRequest{}, "some-refresh-token"))
	if code := connectCode(t, err); code != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want CodeUnauthenticated", code)
	}
	if key := errorKey(t, err); key != "identity.refresh_token.invalid" {
		t.Fatalf("key = %q, want %q", key, "identity.refresh_token.invalid")
	}
}

func TestSessionHandler_Refresh_ReusedToken(t *testing.T) {
	sessions := newFakeSessions()
	sessions.rotateErr = repository.ErrSessionReused
	h := NewSessionHandler(sessions)

	_, err := h.Refresh(context.Background(), requestWithRefreshCookie(&sessionv1.RefreshRequest{}, "some-refresh-token"))
	if code := connectCode(t, err); code != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want CodeUnauthenticated", code)
	}
	if key := errorKey(t, err); key != "identity.refresh_token.reused" {
		t.Fatalf("key = %q, want %q", key, "identity.refresh_token.reused")
	}
}

func TestSessionHandler_Refresh_SuccessSetsCookies(t *testing.T) {
	h := NewSessionHandler(newFakeSessions())

	resp, err := h.Refresh(context.Background(), requestWithRefreshCookie(&sessionv1.RefreshRequest{}, "some-refresh-token"))
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	cookies := (&http.Response{Header: resp.Header()}).Cookies()
	names := map[string]bool{}
	for _, c := range cookies {
		names[c.Name] = true
	}
	for _, want := range []string{authctx.CookieAccessToken, authctx.CookieRefreshToken, authctx.CookieSessionInfo} {
		if !names[want] {
			t.Fatalf("missing %s cookie, got %v", want, names)
		}
	}
}

func TestSessionHandler_Logout_NoCookieStillClearsAndSucceeds(t *testing.T) {
	sessions := newFakeSessions()
	h := NewSessionHandler(sessions)

	resp, err := h.Logout(context.Background(), connect.NewRequest(&sessionv1.LogoutRequest{}))
	if err != nil {
		t.Fatalf("Logout: %v", err)
	}

	cookies := (&http.Response{Header: resp.Header()}).Cookies()
	if len(cookies) != 3 {
		t.Fatalf("len(cookies) = %d, want 3 (all cleared)", len(cookies))
	}
	for _, c := range cookies {
		if c.MaxAge >= 0 {
			t.Fatalf("cookie %s MaxAge = %d, want negative (deleted)", c.Name, c.MaxAge)
		}
	}
}

func TestSessionHandler_Logout_RevokesPresentedToken(t *testing.T) {
	sessions := newFakeSessions()
	h := NewSessionHandler(sessions)

	resp, err := h.Logout(context.Background(), requestWithRefreshCookie(&sessionv1.LogoutRequest{}, "some-refresh-token"))
	if err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if len(resp.Header().Values("Set-Cookie")) != 3 {
		t.Fatalf("expected 3 Set-Cookie headers, got %d", len(resp.Header().Values("Set-Cookie")))
	}
	if len(sessions.revokedRefresh) != 1 || sessions.revokedRefresh[0] != "some-refresh-token" {
		t.Fatalf("revokedRefresh = %v, want [\"some-refresh-token\"]", sessions.revokedRefresh)
	}
}

func TestSessionHandler_Logout_PropagatesRevokeError(t *testing.T) {
	sessions := newFakeSessions()
	sessions.revokeErr = errors.New("boom")
	h := NewSessionHandler(sessions)

	_, err := h.Logout(context.Background(), requestWithRefreshCookie(&sessionv1.LogoutRequest{}, "some-refresh-token"))
	if code := connectCode(t, err); code != connect.CodeInternal {
		t.Fatalf("code = %v, want CodeInternal", code)
	}
}
