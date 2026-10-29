package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"

	identityrepo "dmatech.ru/internal/modules/identity/repository"
	"dmatech.ru/internal/pkg/authctx"
)

// sessionManager реализует authhandler.SessionIssuer/SessionStore/
// SessionRevoker поверх identityrepo.Repository (identity.sessions,
// identity.users.permissions) и authctx.Issuer (подпись access-токена) —
// общая точка выпуска/ротации/гашения сессий для всех шагов, которые в
// них участвуют: вход, регистрация (авто-логин), обновление/логаут, сброс
// пароля (гасит все сессии сразу).
type sessionManager struct {
	repo   *identityrepo.Repository
	issuer *authctx.Issuer
}

func newSessionManager(repo *identityrepo.Repository, issuer *authctx.Issuer) *sessionManager {
	return &sessionManager{repo: repo, issuer: issuer}
}

// newRefreshToken генерирует новый refresh-токен (raw — то, что едет в
// куку) и его хеш (то, что хранится в identity.sessions) — тот же приём,
// что у OTP-кодов (hashOTP в registration.go): по хешу невозможно
// восстановить сам токен, то есть доступ к БД один не даёт подделать сессию.
func newRefreshToken() (raw string, hash []byte, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("session: generate refresh token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(raw))
	return raw, sum[:], nil
}

// IssueSession — новая сессия (успешный вход/регистрация): создаёт строку
// в identity.sessions и подписывает access-токен с текущими правами
// пользователя.
func (m *sessionManager) IssueSession(ctx context.Context, userID, userAgent string) (access, refresh, permissions string, accessExpiresAt time.Time, err error) {
	permissions, err = m.repo.GetPermissions(ctx, userID)
	if err != nil {
		return "", "", "", time.Time{}, fmt.Errorf("session: load permissions: %w", err)
	}

	refresh, hash, err := newRefreshToken()
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	sessionID, err := m.repo.CreateSession(ctx, userID, hash, userAgent)
	if err != nil {
		return "", "", "", time.Time{}, fmt.Errorf("session: create: %w", err)
	}

	access, accessExpiresAt, err = m.issuer.Issue(authctx.Claims{UserID: userID, Permissions: permissions, SessionID: sessionID})
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	return access, refresh, permissions, accessExpiresAt, nil
}

// Rotate — IdentitySessionService.Refresh: см. identityrepo.Repository.
// RotateSession про ErrNotFound/ErrSessionReused, которые отсюда
// возвращаются как есть — хендлер сам маппит их на connect-коды.
func (m *sessionManager) Rotate(ctx context.Context, oldRefreshToken, userAgent string) (access, refresh, permissions string, accessExpiresAt time.Time, err error) {
	oldHash := sha256.Sum256([]byte(oldRefreshToken))
	newRaw, newHash, err := newRefreshToken()
	if err != nil {
		return "", "", "", time.Time{}, err
	}

	sessionID, userID, err := m.repo.RotateSession(ctx, oldHash[:], newHash, userAgent)
	if err != nil {
		return "", "", "", time.Time{}, err
	}

	permissions, err = m.repo.GetPermissions(ctx, userID)
	if err != nil {
		return "", "", "", time.Time{}, fmt.Errorf("session: load permissions: %w", err)
	}

	access, accessExpiresAt, err = m.issuer.Issue(authctx.Claims{UserID: userID, Permissions: permissions, SessionID: sessionID})
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	return access, newRaw, permissions, accessExpiresAt, nil
}

// Revoke — IdentitySessionService.Logout.
func (m *sessionManager) Revoke(ctx context.Context, refreshToken string) error {
	hash := sha256.Sum256([]byte(refreshToken))
	return m.repo.RevokeSession(ctx, hash[:])
}

// RevokeAllSessions — IdentityRecoveryService.ResetPassword.
func (m *sessionManager) RevokeAllSessions(ctx context.Context, userID string) error {
	return m.repo.RevokeAllSessions(ctx, userID)
}
