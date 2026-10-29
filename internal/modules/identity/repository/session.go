package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrSessionReused — refresh-токен, предъявленный Refresh, уже был
// погашен более ранней ротацией (не просто просрочен — см. RotateSession).
// Это не "сессия истекла", а признак того, что токен, скорее всего,
// скомпрометирован (кто-то другой уже его использовал) — RotateSession
// в ответ гасит все сессии пользователя, не только эту.
var ErrSessionReused = errors.New("repository: refresh token already used (possible compromise)")

const (
	// sessionRefreshTTL/sessionAccessTTL — см. doc-комментарий identity.sessions
	// в миграции: срок жизни refresh-токена (строки сессии) и access-токена
	// (JWT, не хранится в БД вообще, см. internal/pkg/authctx).
	sessionAccessTTL  = 15 * time.Minute
	sessionRefreshTTL = 30 * 24 * time.Hour
)

// AccessTokenTTL/RefreshTokenTTL — экспортированы, чтобы Go-код identity,
// выпускающий токены (см. identity/session.go), и репозиторий
// использовали одни и те же значения, не дублируя их.
func AccessTokenTTL() time.Duration  { return sessionAccessTTL }
func RefreshTokenTTL() time.Duration { return sessionRefreshTTL }

// CreateSession создаёt новую сессию (новый вход — CompleteAuthentication/
// CompleteRegistration): refreshTokenHash — хеш (SHA-256) сгенерированного
// refresh-токена, сам токен в базу не попадает, как и везде в этом модуле
// с чувствительными секретами.
func (r *Repository) CreateSession(ctx context.Context, userID string, refreshTokenHash []byte, userAgent string) (sessionID string, err error) {
	err = r.pool.QueryRow(ctx,
		`INSERT INTO identity.sessions (user_id, refresh_token_hash, user_agent, expires_at)
		 VALUES ($1::uuid, $2, $3, $4)
		 RETURNING id::text`,
		userID, refreshTokenHash, userAgent, time.Now().Add(sessionRefreshTTL),
	).Scan(&sessionID)
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return sessionID, nil
}

// RotateSession ротирует refresh-токен: находит сессию по хешу старого
// токена, проверяет что не погашена и не просрочена, гасит её и создаёт
// новую с новым хешем (та же длина жизни, что у свежего входа — ротация
// не продлевает исходную сессию на неопределённый срок дальше
// sessionRefreshTTL от момента ротации, это осознанно "скользящее окно",
// не fixed lifetime от первого входа).
//
// Сессия не найдена вообще или просрочена — ErrNotFound (обычный повод
// звать IdentityAuthService.InitializeSession заново). Сессия найдена, но
// уже погашена более ранней ротацией — ErrSessionReused: эта ветка гасит
// ВСЕ сессии пользователя, а не только найденную, в той же транзакции.
func (r *Repository) RotateSession(ctx context.Context, oldRefreshTokenHash, newRefreshTokenHash []byte, userAgent string) (newSessionID, userID string, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return "", "", fmt.Errorf("rotate session: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var sessionID string
	var revokedAt *time.Time
	var expiresAt time.Time
	err = tx.QueryRow(ctx,
		`SELECT id::text, user_id::text, revoked_at, expires_at
		 FROM identity.sessions WHERE refresh_token_hash = $1 FOR UPDATE`,
		oldRefreshTokenHash,
	).Scan(&sessionID, &userID, &revokedAt, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("rotate session: load session: %w", err)
	}

	if revokedAt != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE identity.sessions SET revoked_at = now() WHERE user_id = $1::uuid AND revoked_at IS NULL`,
			userID,
		); err != nil {
			return "", "", fmt.Errorf("rotate session: revoke all after reuse: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return "", "", fmt.Errorf("rotate session: %w", err)
		}
		return "", "", ErrSessionReused
	}
	if time.Now().After(expiresAt) {
		return "", "", ErrNotFound
	}

	if _, err := tx.Exec(ctx,
		`UPDATE identity.sessions SET revoked_at = now() WHERE id = $1::uuid`, sessionID,
	); err != nil {
		return "", "", fmt.Errorf("rotate session: revoke old: %w", err)
	}

	if err := tx.QueryRow(ctx,
		`INSERT INTO identity.sessions (user_id, refresh_token_hash, user_agent, expires_at)
		 VALUES ($1::uuid, $2, $3, $4)
		 RETURNING id::text`,
		userID, newRefreshTokenHash, userAgent, time.Now().Add(sessionRefreshTTL),
	).Scan(&newSessionID); err != nil {
		return "", "", fmt.Errorf("rotate session: create new: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", "", fmt.Errorf("rotate session: %w", err)
	}
	return newSessionID, userID, nil
}

// RevokeSession гасит сессию по хешу её refresh-токена (Logout).
// Отсутствующий или уже погашенный токен — не ошибка (см. doc-комментарий
// Logout в session.proto: результат для вызывающего кода тот же самый).
func (r *Repository) RevokeSession(ctx context.Context, refreshTokenHash []byte) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE identity.sessions SET revoked_at = now() WHERE refresh_token_hash = $1 AND revoked_at IS NULL`,
		refreshTokenHash,
	)
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

// RevokeAllSessions гасит все активные сессии пользователя — вызывается
// после смены пароля через восстановление доступа (см. doc-комментарий
// ResetPassword в recovery.proto): ни один ранее выданный refresh-токен
// больше не ротируется.
func (r *Repository) RevokeAllSessions(ctx context.Context, userID string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE identity.sessions SET revoked_at = now() WHERE user_id = $1::uuid AND revoked_at IS NULL`,
		userID,
	)
	if err != nil {
		return fmt.Errorf("revoke all sessions: %w", err)
	}
	return nil
}

// GetPermissions возвращает identity.users.permissions (битсет в base64,
// см. doc-комментарий у этой колонки в миграции 0001) — нужен при выпуске
// access-токена (см. internal/pkg/authctx.Claims.Permissions).
func (r *Repository) GetPermissions(ctx context.Context, userID string) (string, error) {
	var permissions string
	err := r.pool.QueryRow(ctx,
		`SELECT permissions FROM identity.users WHERE id = $1::uuid`, userID,
	).Scan(&permissions)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get permissions: %w", err)
	}
	return permissions, nil
}
