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
	// ErrCodeMismatch — присланный код подтверждения не совпал с сохранённым.
	ErrCodeMismatch = errors.New("repository: verification code does not match")
	// ErrTooManyAttempts — превышен лимит попыток ввода кода для этого шага.
	ErrTooManyAttempts = errors.New("repository: too many verification attempts")
	// ErrPhoneNotVerified — шаг требует подтверждённого телефона в этой сессии.
	ErrPhoneNotVerified = errors.New("repository: phone is not verified yet")
	// ErrEmailNotVerified — шаг требует подтверждённого email в этой сессии.
	ErrEmailNotVerified = errors.New("repository: email is not verified yet")
)

const (
	// registrationSessionTTL — общий дедлайн всей регистрации, независимо от
	// того, на каком шаге её забросили.
	registrationSessionTTL = 30 * time.Minute
	// verificationCodeTTL — срок жизни кода подтверждения телефона/email.
	verificationCodeTTL = 10 * time.Minute
)

// CreatePhoneSession создаёт новую сессию регистрации с кодом подтверждения
// телефона. codeHash — хеш кода (SHA-256), сам код в базу не попадает.
// Попутно чистит просроченные сессии — отдельная фоновая задача не нужна
// (тот же приём, что у CreateChallenge).
func (r *Repository) CreatePhoneSession(ctx context.Context, phone string, consent bool, codeHash []byte) (string, error) {
	now := time.Now()
	var id string
	err := r.pool.QueryRow(ctx,
		`WITH cleanup AS (
		     DELETE FROM identity.registration_sessions WHERE expires_at <= now()
		 )
		 INSERT INTO identity.registration_sessions
		     (phone, personal_data_consent, phone_code_hash, phone_code_expires_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id::text`,
		phone, consent, codeHash, now.Add(verificationCodeTTL), now.Add(registrationSessionTTL),
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create registration session: %w", err)
	}
	return id, nil
}

// VerifyPhoneCode проверяет код для sessionID. При совпадении отмечает
// телефон подтверждённым. При несовпадении увеличивает счётчик попыток и
// возвращает ErrCodeMismatch. ErrNotFound — сессия не найдена или просрочена
// (как целиком, так и именно код: expired-код трактуется как mismatch, см. ниже).
func (r *Repository) VerifyPhoneCode(ctx context.Context, sessionID string, codeHash []byte) error {
	return r.verifyCode(ctx, sessionID, codeHash,
		"phone_code_hash", "phone_code_expires_at", "phone_attempts", "phone_verified")
}

// VerifyEmailCode — аналогично VerifyPhoneCode, но для шага подтверждения email.
func (r *Repository) VerifyEmailCode(ctx context.Context, sessionID string, codeHash []byte) error {
	return r.verifyCode(ctx, sessionID, codeHash,
		"email_code_hash", "email_code_expires_at", "email_attempts", "email_verified")
}

// verifyCode — общая логика для VerifyPhoneCode/VerifyEmailCode: какие
// колонки читать/обновлять, параметризовано именами (обе колонки принадлежат
// одной и той же, заранее известной, таблице — конкатенация имён столбцов,
// а не пользовательского ввода, в SQL ниже безопасна).
func (r *Repository) verifyCode(ctx context.Context, sessionID string, codeHash []byte, hashCol, expiresCol, attemptsCol, verifiedCol string) error {
	var storedHash []byte
	var expiresAt *time.Time
	var attempts int
	err := r.pool.QueryRow(ctx,
		`SELECT `+hashCol+`, `+expiresCol+`, `+attemptsCol+`
		 FROM identity.registration_sessions
		 WHERE id = $1::uuid AND expires_at > now()`,
		sessionID,
	).Scan(&storedHash, &expiresAt, &attempts)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("verify code: %w", err)
	}

	if attempts >= r.maxCodeAttempts {
		return ErrTooManyAttempts
	}

	if storedHash == nil || expiresAt == nil || time.Now().After(*expiresAt) || !bytes.Equal(storedHash, codeHash) {
		if _, err := r.pool.Exec(ctx,
			`UPDATE identity.registration_sessions SET `+attemptsCol+` = `+attemptsCol+` + 1 WHERE id = $1::uuid`,
			sessionID,
		); err != nil {
			return fmt.Errorf("verify code: record attempt: %w", err)
		}
		return ErrCodeMismatch
	}

	if _, err := r.pool.Exec(ctx,
		`UPDATE identity.registration_sessions SET `+verifiedCol+` = true WHERE id = $1::uuid`,
		sessionID,
	); err != nil {
		return fmt.Errorf("verify code: mark verified: %w", err)
	}
	return nil
}

// StartEmailVerification сохраняет email и код подтверждения для уже
// существующей сессии. Требует подтверждённого телефона в этой сессии —
// иначе ErrPhoneNotVerified. ErrNotFound — сессия не найдена/просрочена.
func (r *Repository) StartEmailVerification(ctx context.Context, sessionID, email string, codeHash []byte) error {
	var phoneVerified bool
	err := r.pool.QueryRow(ctx,
		`SELECT phone_verified FROM identity.registration_sessions WHERE id = $1::uuid AND expires_at > now()`,
		sessionID,
	).Scan(&phoneVerified)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("start email verification: %w", err)
	}
	if !phoneVerified {
		return ErrPhoneNotVerified
	}

	if _, err := r.pool.Exec(ctx,
		`UPDATE identity.registration_sessions
		 SET email = $2, email_code_hash = $3, email_code_expires_at = $4,
		     email_attempts = 0, email_verified = false
		 WHERE id = $1::uuid`,
		sessionID, email, codeHash, time.Now().Add(verificationCodeTTL),
	); err != nil {
		return fmt.Errorf("start email verification: update: %w", err)
	}
	return nil
}

// CompleteRegistrationParams — данные последнего шага, которых нет в самой
// сессии (phone/email там уже есть в самой строке identity.registration_sessions), запрашивать
// их повторно у вызывающего кода незачем и рискует разойтись с тем, что
// реально подтверждено.
type CompleteRegistrationParams struct {
	Salt, Verifier                  []byte
	FirstName, LastName, MiddleName string
}

// CompleteRegistration — финальный шаг: требует подтверждённых телефона и
// email в этой сессии (иначе ErrPhoneNotVerified/ErrEmailNotVerified),
// создаёт пользователя с phone/email из самой сессии (не из p — на этом
// шаге их негде больше взять, кроме как из того, что реально подтверждено)
// и одной транзакцией удаляет сессию регистрации — повторный вызов с тем
// же sessionID вернёт ErrNotFound.
//
// Статус нового пользователя — сразу UserStatusActive, а не дефолтный
// UserStatusPending у identity.users: до этой строки дойти невозможно, не
// подтвердив телефон (см. проверку phoneVerified выше) — отдельного шага
// "активации" в проекте нет, CompleteRegistration — первый и единственный
// момент, когда для аккаунта вообще заводится строка identity.users, и к
// этому моменту условие активации уже выполнено.
func (r *Repository) CompleteRegistration(ctx context.Context, sessionID string, p CompleteRegistrationParams) (string, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("complete registration: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var phone string
	var email *string
	var phoneVerified, emailVerified bool
	if err := tx.QueryRow(ctx,
		`SELECT phone, email, phone_verified, email_verified
		 FROM identity.registration_sessions WHERE id = $1::uuid AND expires_at > now()`,
		sessionID,
	).Scan(&phone, &email, &phoneVerified, &emailVerified); err != nil {
		if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("complete registration: load session: %w", err)
	}
	if !phoneVerified {
		return "", ErrPhoneNotVerified
	}
	if !emailVerified {
		return "", ErrEmailNotVerified
	}

	var userID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO identity.users (status) VALUES ($1) RETURNING id::text`,
		UserStatusActive,
	).Scan(&userID); err != nil {
		return "", fmt.Errorf("complete registration: create user: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO identity.profiles (user_id, first_name, last_name, middle_name)
		 VALUES ($1::uuid, $2, $3, $4)`,
		userID, p.FirstName, p.LastName, p.MiddleName,
	); err != nil {
		return "", fmt.Errorf("complete registration: create profile: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO identity.auth (user_id, email, phone, srp_salt, srp_verifier)
		 VALUES ($1::uuid, $2, $3, $4, $5)`,
		userID, email, phone, p.Salt, p.Verifier,
	); err != nil {
		if isUniqueViolation(err) {
			return "", ErrAlreadyExists
		}
		return "", fmt.Errorf("complete registration: create auth: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM identity.registration_sessions WHERE id = $1::uuid`, sessionID,
	); err != nil {
		return "", fmt.Errorf("complete registration: delete session: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("complete registration: %w", err)
	}
	return userID, nil
}

// PhoneExists/EmailExists — используются StartPhoneVerification/
// StartEmailVerification для раннего (не атомарного — финальная гарантия
// уникальности всё равно на уникальных индексах identity.auth, см.
// CompleteRegistration) ответа ALREADY_EXISTS, не дожидаясь финального шага.
func (r *Repository) PhoneExists(ctx context.Context, phone string) (bool, error) {
	return r.exists(ctx, "phone = $1", phone)
}

func (r *Repository) EmailExists(ctx context.Context, email string) (bool, error) {
	return r.exists(ctx, "lower(email) = lower($1)", email)
}

func (r *Repository) exists(ctx context.Context, cond string, arg string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM identity.auth WHERE `+cond+`)`, arg,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check existence: %w", err)
	}
	return exists, nil
}
