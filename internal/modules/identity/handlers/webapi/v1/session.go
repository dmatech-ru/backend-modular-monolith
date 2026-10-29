package v1

import (
	"context"
	"errors"
	"net/http"
	"time"

	"connectrpc.com/connect"

	"dmatech.ru/internal/modules/identity/repository"
	sessionv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity/session"
	"dmatech.ru/internal/pkg/contracts/webapi/v1/identity/session/sessionv1connect"
)

// SessionStore ротирует/гасит refresh-сессии. Реализация — тонкая обёртка
// над identity.sessions + internal/pkg/authctx (см. identity/session.go).
type SessionStore interface {
	Rotate(ctx context.Context, refreshToken, userAgent string) (access, refresh, permissions string, accessExpiresAt time.Time, err error)
	Revoke(ctx context.Context, refreshToken string) error
}

// Sessions — все операции с сессиями, которые нужны identity: выпуск при
// входе/регистрации (SessionIssuer), ротация/логаут через
// IdentitySessionService (SessionStore), гашение всех сразу при сбросе
// пароля (SessionRevoker). Один интерфейс, не три отдельных параметра в
// Subscribe — реализация одна (identity/session.go), её просто видят с
// разных сторон разные хендлеры.
type Sessions interface {
	SessionIssuer
	SessionStore
	SessionRevoker
}

// SessionHandler реализует sessionv1connect.IdentitySessionServiceHandler.
type SessionHandler struct {
	sessionv1connect.UnimplementedIdentitySessionServiceHandler

	sessions SessionStore
}

func NewSessionHandler(sessions SessionStore) *SessionHandler {
	return &SessionHandler{sessions: sessions}
}

// NewSessionServiceHandler собирает HTTP-обработчик IdentitySessionService
// для монтирования в веб-сервер.
func NewSessionServiceHandler(sessions SessionStore, opts ...connect.HandlerOption) (string, http.Handler) {
	return sessionv1connect.NewIdentitySessionServiceHandler(NewSessionHandler(sessions), opts...)
}

// Refresh — см. doc-комментарий сервиса в session.proto.
func (h *SessionHandler) Refresh(
	ctx context.Context,
	req *connect.Request[sessionv1.RefreshRequest],
) (*connect.Response[sessionv1.RefreshResponse], error) {
	refreshToken, ok := readRefreshToken(req.Header())
	if !ok {
		return nil, keyedError(connect.CodeUnauthenticated, "identity.refresh_token.missing", "refresh token is missing")
	}

	access, refresh, permissions, accessExpiresAt, err := h.sessions.Rotate(ctx, refreshToken, req.Header().Get("User-Agent"))
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return nil, keyedError(connect.CodeUnauthenticated, "identity.refresh_token.invalid", "refresh token is invalid or expired")
	case errors.Is(err, repository.ErrSessionReused):
		return nil, keyedError(connect.CodeUnauthenticated, "identity.refresh_token.reused", "refresh token has already been used")
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	resp := connect.NewResponse(&sessionv1.RefreshResponse{})
	setSessionCookies(resp.Header(), access, refresh, permissions, accessExpiresAt)
	return resp, nil
}

// Logout — см. doc-комментарий сервиса в session.proto: отсутствующий или
// уже недействительный refresh-токен — не ошибка, результат (разлогинен)
// тот же самый, поэтому Revoke не вызывается вовсе, если куки не было, но
// ответ всё равно успешный и куки всё равно чистятся.
func (h *SessionHandler) Logout(
	ctx context.Context,
	req *connect.Request[sessionv1.LogoutRequest],
) (*connect.Response[sessionv1.LogoutResponse], error) {
	if refreshToken, ok := readRefreshToken(req.Header()); ok {
		if err := h.sessions.Revoke(ctx, refreshToken); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}

	resp := connect.NewResponse(&sessionv1.LogoutResponse{})
	clearSessionCookies(resp.Header())
	return resp, nil
}
