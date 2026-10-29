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
//
// Попутно (в том же запросе) удаляются просроченные строки — тот же приём,
// что CreateChallenge/CreateRecoverySession/CreateRegistrationSession:
// отдельная фоновая задача (reaper) не нужна, каждый новый вход сам чистит
// накопившийся мусор. Условие — именно expires_at, не revoked_at: погашенная
// более ранней ротацией строка (см. RotateSession) должна дожить до
// собственного expires_at, иначе повторное предъявление угнанного
// refresh-токена после того, как её успели удалить, вернёт "сессия не
// найдена" вместо ErrSessionReused — то есть молча потеряло бы сигнал
// компрометации, который и есть весь смысл хранить погашенные строки.
func (r *Repository) CreateSession(ctx context.Context, userID string, refreshTokenHash []byte, userAgent string) (sessionID string, err error) {
	err = r.pool.QueryRow(ctx,
		`WITH cleanup AS (
		     DELETE FROM identity.sessions WHERE expires_at <= now()
		 )
		 INSERT INTO identity.sessions (user_id, refresh_token_hash, user_agent, expires_at)
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

	// Попутная чистка просроченных строк — тот же приём, что в CreateSession
	// (см. doc-комментарий там): Refresh происходит гораздо чаще нового
	// входа (раз в sessionAccessTTL на активного пользователя), так что это
	// основной момент, когда мусор реально выметается.
	if err := tx.QueryRow(ctx,
		`WITH cleanup AS (
		     DELETE FROM identity.sessions WHERE expires_at <= now()
		 )
		 INSERT INTO identity.sessions (user_id, refresh_token_hash, user_agent, expires_at)
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

// SessionSummary — одна сессия для самообслуживания (ListSessions): ровно
// то, что безопасно показать владельцу — без refresh_token_hash.
type SessionSummary struct {
	ID        string
	UserAgent string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// ListSessions возвращает активные (не погашенные, не просроченные) сессии
// пользователя — IdentitySessionService.ListSessions: "где я залогинен",
// без необходимости гасить всё сразу, как RevokeAllSessions. Погашенные
// строки не включаются: они сами уйдут при следующей попутной чистке (см.
// doc-комментарий CreateSession) и пользователю смотреть на них незачем.
func (r *Repository) ListSessions(ctx context.Context, userID string) ([]SessionSummary, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id::text, user_agent, created_at, expires_at
		 FROM identity.sessions
		 WHERE user_id = $1::uuid AND revoked_at IS NULL AND expires_at > now()
		 ORDER BY created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var sessions []SessionSummary
	for rows.Next() {
		var s SessionSummary
		if err := rows.Scan(&s.ID, &s.UserAgent, &s.CreatedAt, &s.ExpiresAt); err != nil {
			return nil, fmt.Errorf("list sessions: scan: %w", err)
		}
		sessions = append(sessions, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return sessions, nil
}

// RevokeSessionByID гасит ровно одну сессию пользователя по её id —
// IdentitySessionService.RevokeSession ("выйти с этого устройства"), в
// отличие от RevokeAllSessions (сброс пароля, гасит всё). userID в условии
// — не просто фильтр результата, а граница доступа: без него пользователь
// мог бы погасить чужую сессию, зная (или подобрав) её id — в отличие от
// refresh-токена, который Revoke проверяет по хешу, id сессии не секрет
// (его же отдаёт ListSessions), и это единственное, что мешает его
// подобрать для чужого аккаунта. Несуществующий id и чужой id дают одну и
// ту же ErrNotFound — отличать их незачем.
func (r *Repository) RevokeSessionByID(ctx context.Context, userID, sessionID string) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE identity.sessions SET revoked_at = now()
		 WHERE id = $1::uuid AND user_id = $2::uuid AND revoked_at IS NULL`,
		sessionID, userID,
	)
	if isInvalidUUID(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("revoke session by id: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
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
