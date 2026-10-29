package v1

import (
	"context"
	"errors"
	"net/http"
	"time"

	"connectrpc.com/connect"

	"dmatech.ru/internal/modules/identity/repository"
	"dmatech.ru/internal/pkg/authctx"
	sessionv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity/session"
	"dmatech.ru/internal/pkg/contracts/webapi/v1/identity/session/sessionv1connect"
)

// SessionStore ротирует/гасит refresh-сессии. Реализация — тонкая обёртка
// над identity.sessions + internal/pkg/authctx (см. identity/session.go).
type SessionStore interface {
	Rotate(ctx context.Context, refreshToken, userAgent string) (access, refresh, permissions string, accessExpiresAt time.Time, err error)
	Revoke(ctx context.Context, refreshToken string) error
	// ListSessions/RevokeSessionByID — самообслуживание (ListSessions/
	// RevokeSession ниже): в отличие от Rotate/Revoke, которым личность
	// приходит из самого refresh-токена, этим двум userID передаёт хендлер
	// — из access-токена, который гейтвей уже проверил (см. authctx).
	ListSessions(ctx context.Context, userID string) ([]repository.SessionSummary, error)
	RevokeSessionByID(ctx context.Context, userID, sessionID string) error
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
	case errors.Is(err, repository.ErrAccountNotActive):
		return nil, keyedError(connect.CodeFailedPrecondition, "identity.account.not_active", "account is not active")
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

// ListSessions — см. doc-комментарий метода в session.proto. Личность
// вызывающего — из access-токена (authctx.FromContext), не из куки: этот
// метод не public (см. session.proto), гейтвей уже проверил токен и
// положил claims в контекст до того, как запрос дошёл сюда (см.
// authctx.ServerInterceptor в auth.handler.go). ok=false здесь означать не
// должно вообще — непубличный метод без claims гейтвей бы не пропустил —
// но на случай расхождения конфигурации (например, вызов напрямую по NATS,
// минуя гейтвей) отвечаем Unauthenticated, а не паникуем на пустом userID.
func (h *SessionHandler) ListSessions(
	ctx context.Context,
	_ *connect.Request[sessionv1.ListSessionsRequest],
) (*connect.Response[sessionv1.ListSessionsResponse], error) {
	claims, ok := authctx.FromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing caller identity"))
	}

	sessions, err := h.sessions.ListSessions(ctx, claims.UserID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	resp := &sessionv1.ListSessionsResponse{Sessions: make([]*sessionv1.SessionInfo, len(sessions))}
	for i, s := range sessions {
		resp.Sessions[i] = &sessionv1.SessionInfo{
			SessionId: s.ID,
			UserAgent: s.UserAgent,
			CreatedAt: s.CreatedAt.Unix(),
			ExpiresAt: s.ExpiresAt.Unix(),
			Current:   s.ID == claims.SessionID,
		}
	}
	return connect.NewResponse(resp), nil
}

// RevokeSession — см. doc-комментарий метода в session.proto.
func (h *SessionHandler) RevokeSession(
	ctx context.Context,
	req *connect.Request[sessionv1.RevokeSessionRequest],
) (*connect.Response[sessionv1.RevokeSessionResponse], error) {
	claims, ok := authctx.FromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing caller identity"))
	}

	// session_id непустой — проверено до вызова этого метода (см.
	// buf.validate.field.cel на этом поле в session.proto).
	sessionID := req.Msg.GetSessionId()

	err := h.sessions.RevokeSessionByID(ctx, claims.UserID, sessionID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, keyedError(connect.CodeNotFound, "identity.session.not_found", "session not found")
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&sessionv1.RevokeSessionResponse{}), nil
}
