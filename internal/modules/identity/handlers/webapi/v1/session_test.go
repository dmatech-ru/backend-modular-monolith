package v1

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

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

// ctxAsUser — контекст с личностью, как её кладёт authctx.ServerInterceptor
// после проверки access-токена гейтвеем (см. doc-комментарий ListSessions/
// RevokeSession) — тестам не нужен настоящий JWT, только то, что хендлер
// читает из context.Context.
func ctxAsUser(userID, sessionID string) context.Context {
	return authctx.NewContext(context.Background(), authctx.Claims{UserID: userID, SessionID: sessionID})
}

func TestSessionHandler_ListSessions_RequiresCallerIdentity(t *testing.T) {
	h := NewSessionHandler(newFakeSessions())

	_, err := h.ListSessions(context.Background(), connect.NewRequest(&sessionv1.ListSessionsRequest{}))
	if code := connectCode(t, err); code != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want CodeUnauthenticated", code)
	}
}

// Пользователь видит только свои сессии, не чужие, и свою текущую —
// помеченной current (см. doc-комментарий SessionInfo.current в session.proto).
func TestSessionHandler_ListSessions_ReturnsOwnSessionsWithCurrentMarked(t *testing.T) {
	sessions := newFakeSessions()
	now := time.Now()
	sessions.seedSession("session-1", "user-1", "Chrome on macOS", now, now.Add(30*24*time.Hour))
	sessions.seedSession("session-2", "user-1", "Safari on iOS", now, now.Add(30*24*time.Hour))
	sessions.seedSession("session-3", "user-2", "Firefox on Linux", now, now.Add(30*24*time.Hour))
	h := NewSessionHandler(sessions)

	resp, err := h.ListSessions(ctxAsUser("user-1", "session-1"), connect.NewRequest(&sessionv1.ListSessionsRequest{}))
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(resp.Msg.GetSessions()) != 2 {
		t.Fatalf("len(sessions) = %d, want 2 (only user-1's own)", len(resp.Msg.GetSessions()))
	}

	current := map[string]bool{}
	for _, s := range resp.Msg.GetSessions() {
		current[s.GetSessionId()] = s.GetCurrent()
		if s.GetSessionId() == "session-3" {
			t.Fatal("must not return another user's session")
		}
	}
	if !current["session-1"] || current["session-2"] {
		t.Fatalf("current flags = %v, want only session-1 marked current", current)
	}
}

func TestSessionHandler_RevokeSession_RequiresCallerIdentity(t *testing.T) {
	h := NewSessionHandler(newFakeSessions())

	_, err := h.RevokeSession(context.Background(), connect.NewRequest(&sessionv1.RevokeSessionRequest{SessionId: "session-1"}))
	if code := connectCode(t, err); code != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want CodeUnauthenticated", code)
	}
}

func TestSessionHandler_RevokeSession_RevokesOwnSession(t *testing.T) {
	sessions := newFakeSessions()
	now := time.Now()
	sessions.seedSession("session-1", "user-1", "Chrome on macOS", now, now.Add(30*24*time.Hour))
	h := NewSessionHandler(sessions)

	if _, err := h.RevokeSession(ctxAsUser("user-1", "session-other"), connect.NewRequest(&sessionv1.RevokeSessionRequest{SessionId: "session-1"})); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if !sessions.sessions["session-1"].revoked {
		t.Fatal("expected session-1 to be revoked")
	}
}

// Чужая сессия и несуществующая сессия должны давать одну и ту же ошибку
// (NotFound) — см. doc-комментарий RevokeSession в session.proto про то,
// почему их незачем различать.
func TestSessionHandler_RevokeSession_ForeignOrMissingSessionIsNotFound(t *testing.T) {
	sessions := newFakeSessions()
	now := time.Now()
	sessions.seedSession("session-1", "user-2", "Chrome on macOS", now, now.Add(30*24*time.Hour))
	h := NewSessionHandler(sessions)

	_, err := h.RevokeSession(ctxAsUser("user-1", "session-other"), connect.NewRequest(&sessionv1.RevokeSessionRequest{SessionId: "session-1"}))
	if code := connectCode(t, err); code != connect.CodeNotFound {
		t.Fatalf("code = %v, want CodeNotFound for another user's session", code)
	}

	_, err = h.RevokeSession(ctxAsUser("user-1", "session-other"), connect.NewRequest(&sessionv1.RevokeSessionRequest{SessionId: "does-not-exist"}))
	if code := connectCode(t, err); code != connect.CodeNotFound {
		t.Fatalf("code = %v, want CodeNotFound for a missing session", code)
	}
}
