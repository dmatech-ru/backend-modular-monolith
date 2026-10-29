package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrRecoveryCodeNotVerified — ResetRecoveryPassword вызван раньше, чем
	// VerifyRecoveryCode подтвердил код в этой сессии.
	ErrRecoveryCodeNotVerified = errors.New("repository: recovery code has not been confirmed yet")
)

const (
	// recoverySessionTTL — общий дедлайн сессии восстановления, независимо
	// от того, на каком шаге её забросили.
	recoverySessionTTL = 30 * time.Minute
	// recoveryCodeTTL — срок жизни кода восстановления.
	recoveryCodeTTL = 10 * time.Minute
	// maxRecoveryCodeAttempts — лимит попыток ввода кода, после которого
	// VerifyRecoveryCode отвечает ErrTooManyAttempts.
	maxRecoveryCodeAttempts = 5
)

// RecoveryAccount — email/телефон аккаунта, нужные IdentityRecoveryService:
// какие каналы восстановления доступны (см. хендлер) и куда слать код.
// Копируется в identity.recovery_sessions на InitiateRecovery (см.
// CreateRecoverySession) — дальнейшие шаги читают сессию, а не identity.auth
// повторно, чтобы не зависеть от того, не поменялся ли email/телефон
// аккаунта за время сессии.
type RecoveryAccount struct {
	UserID string
	Email  *string
	Phone  *string
}

// RecoverySessionRecord — RecoveryAccount плюс отметка, подтверждён ли код в
// этой сессии (нужно ResetRecoveryPassword, см. ErrRecoveryCodeNotVerified).
type RecoverySessionRecord struct {
	RecoveryAccount
	CodeVerified bool
}

// FindAuthForRecovery ищет аккаунт по identifier (email/телефон/username —
// как FindAuth), но возвращает email/phone, а не SRP-credentials:
// IdentityRecoveryService не аутентифицирует через SRP-обмен, ResetPassword
// просто перезаписывает salt/verifier по подтверждённому коду.
func (r *Repository) FindAuthForRecovery(ctx context.Context, identifier string) (RecoveryAccount, error) {
	var acc RecoveryAccount
	err := r.pool.QueryRow(ctx,
		`SELECT user_id::text, email, phone
		 FROM identity.auth
		 WHERE lower(email) = lower($1) OR phone = $1 OR lower(username) = lower($1)`,
		identifier,
	).Scan(&acc.UserID, &acc.Email, &acc.Phone)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecoveryAccount{}, ErrNotFound
	}
	if err != nil {
		return RecoveryAccount{}, fmt.Errorf("find auth for recovery: %w", err)
	}
	return acc, nil
}

// CreateRecoverySession создаёт сессию восстановления для найденного
// аккаунта (email/phone — слепок, см. RecoveryAccount). Код пока не задан —
// появится в SetRecoveryCode. Попутно чистит просроченные сессии — тот же
// приём, что в CreatePhoneSession/CreateChallenge.
func (r *Repository) CreateRecoverySession(ctx context.Context, acc RecoveryAccount) (string, error) {
	var id string
	err := r.pool.QueryRow(ctx,
		`WITH cleanup AS (
		     DELETE FROM identity.recovery_sessions WHERE expires_at <= now()
		 )
		 INSERT INTO identity.recovery_sessions (user_id, email, phone, expires_at)
		 VALUES ($1::uuid, $2, $3, $4)
		 RETURNING id::text`,
		acc.UserID, acc.Email, acc.Phone, time.Now().Add(recoverySessionTTL),
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create recovery session: %w", err)
	}
	return id, nil
}

// GetRecoverySession загружает сессию по id — SendRecoveryCode выбирает по
// ней destination для канала, ResetRecoveryPassword проверяет CodeVerified
// (но делает это сам, в своей транзакции — см. ниже).
func (r *Repository) GetRecoverySession(ctx context.Context, sessionID string) (RecoverySessionRecord, error) {
	var rec RecoverySessionRecord
	err := r.pool.QueryRow(ctx,
		`SELECT user_id::text, email, phone, code_verified
		 FROM identity.recovery_sessions WHERE id = $1::uuid AND expires_at > now()`,
		sessionID,
	).Scan(&rec.UserID, &rec.Email, &rec.Phone, &rec.CodeVerified)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return RecoverySessionRecord{}, ErrNotFound
	}
	if err != nil {
		return RecoverySessionRecord{}, fmt.Errorf("get recovery session: %w", err)
	}
	return rec, nil
}

// SetRecoveryCode сохраняет новый код для sessionID и сбрасывает попытки/
// подтверждение — повторный вызов (в т.ч. с другим каналом) всегда
// инвалидирует предыдущий код, как того требует doc-комментарий
// SendRecoveryCode в recovery.proto.
func (r *Repository) SetRecoveryCode(ctx context.Context, sessionID string, codeHash []byte) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE identity.recovery_sessions
		 SET code_hash = $2, code_expires_at = $3, attempts = 0, code_verified = false
		 WHERE id = $1::uuid AND expires_at > now()`,
		sessionID, codeHash, time.Now().Add(recoveryCodeTTL),
	)
	if isInvalidUUID(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("set recovery code: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// VerifyRecoveryCode — та же логика, что verifyCode для регистрации (см.
// registration.go), но для identity.recovery_sessions: общий набор ошибок
// (ErrNotFound/ErrTooManyAttempts/ErrCodeMismatch, см. repository.go).
func (r *Repository) VerifyRecoveryCode(ctx context.Context, sessionID string, codeHash []byte) error {
	var storedHash []byte
	var expiresAt *time.Time
	var attempts int
	err := r.pool.QueryRow(ctx,
		`SELECT code_hash, code_expires_at, attempts
		 FROM identity.recovery_sessions
		 WHERE id = $1::uuid AND expires_at > now()`,
		sessionID,
	).Scan(&storedHash, &expiresAt, &attempts)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("verify recovery code: %w", err)
	}

	if attempts >= maxRecoveryCodeAttempts {
		return ErrTooManyAttempts
	}

	if storedHash == nil || expiresAt == nil || time.Now().After(*expiresAt) || !bytes.Equal(storedHash, codeHash) {
		if _, err := r.pool.Exec(ctx,
			`UPDATE identity.recovery_sessions SET attempts = attempts + 1 WHERE id = $1::uuid`,
			sessionID,
		); err != nil {
			return fmt.Errorf("verify recovery code: record attempt: %w", err)
		}
		return ErrCodeMismatch
	}

	if _, err := r.pool.Exec(ctx,
		`UPDATE identity.recovery_sessions SET code_verified = true WHERE id = $1::uuid`,
		sessionID,
	); err != nil {
		return fmt.Errorf("verify recovery code: mark verified: %w", err)
	}
	return nil
}

// ResetRecoveryPassword — финальный шаг: требует подтверждённого кода в этой
// сессии (иначе ErrRecoveryCodeNotVerified), перезаписывает SRP salt/verifier
// и password_changed_at у аккаунта и одной транзакцией удаляет сессию —
// повторный вызов с тем же sessionID вернёт ErrNotFound (как и
// CompleteRegistration с registration_sessions). Возвращает email/phone из
// того же снимка сессии (RecoveryAccount), а не только user_id — хендлеру
// они нужны, чтобы уведомить владельца о смене пароля на все известные
// каналы (см. PasswordChangedNotifier), не только на тот, что использовался
// для самого восстановления.
func (r *Repository) ResetRecoveryPassword(ctx context.Context, sessionID string, salt, verifier []byte) (acc RecoveryAccount, passwordChangedAt time.Time, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return RecoveryAccount{}, time.Time{}, fmt.Errorf("reset recovery password: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var codeVerified bool
	if err := tx.QueryRow(ctx,
		`SELECT user_id::text, email, phone, code_verified
		 FROM identity.recovery_sessions WHERE id = $1::uuid AND expires_at > now()`,
		sessionID,
	).Scan(&acc.UserID, &acc.Email, &acc.Phone, &codeVerified); err != nil {
		if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
			return RecoveryAccount{}, time.Time{}, ErrNotFound
		}
		return RecoveryAccount{}, time.Time{}, fmt.Errorf("reset recovery password: load session: %w", err)
	}
	if !codeVerified {
		return RecoveryAccount{}, time.Time{}, ErrRecoveryCodeNotVerified
	}

	now := time.Now()
	if _, err := tx.Exec(ctx,
		`UPDATE identity.auth SET srp_salt = $2, srp_verifier = $3, password_changed_at = $4 WHERE user_id = $1::uuid`,
		acc.UserID, salt, verifier, now,
	); err != nil {
		return RecoveryAccount{}, time.Time{}, fmt.Errorf("reset recovery password: update auth: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM identity.recovery_sessions WHERE id = $1::uuid`, sessionID,
	); err != nil {
		return RecoveryAccount{}, time.Time{}, fmt.Errorf("reset recovery password: delete session: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return RecoveryAccount{}, time.Time{}, fmt.Errorf("reset recovery password: %w", err)
	}
	return acc, now, nil
}
