// Package repository — доступ к PostgreSQL для identity-модуля: поиск
// учётной записи для SRP-входа, одноразовые challenge'ы второго шага и
// регистрация.
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrNotFound — учётная запись, challenge или другая запись не найдена
	// (в том числе challenge, который уже истёк или был использован).
	ErrNotFound = errors.New("repository: not found")
	// ErrAlreadyExists — email/телефон уже зарегистрирован.
	ErrAlreadyExists = errors.New("repository: already exists")
	// ErrAccountNotActive — учётная запись найдена, но её identity.users.status
	// не UserStatusActive (см. RotateSession): блокировка аккаунта должна
	// остановить не только новые входы (CompleteAuthentication), но и
	// бесконечную ротацию уже выданного refresh-токена.
	ErrAccountNotActive = errors.New("repository: account is not active")
)

// Значения identity.users.status (CHECK-constraint, см. миграцию 0001) —
// именованные константы вместо магических строк в CompleteRegistration/
// GetUserStatus и вызывающем их коде.
const (
	UserStatusPending = "pending"
	UserStatusActive  = "active"
	UserStatusBlocked = "blocked"
	UserStatusDeleted = "deleted"
)

// challengeTTL — как долго живёт challenge второго шага SRP-аутентификации
// после InitializeSession.
const challengeTTL = 2 * time.Minute

// Repository — репозиторий identity-модуля поверх пула соединений PostgreSQL.
type Repository struct {
	pool *pgxpool.Pool
	// maxCodeAttempts — лимит попыток ввода кода подтверждения (телефон/
	// email при регистрации, код восстановления при сбросе пароля), после
	// которого VerifyPhoneCode/VerifyEmailCode/VerifyRecoveryCode отвечают
	// ErrTooManyAttempts. Конфигурируется (см. configuration.MaxCodeAttempts
	// в identity/config.go), а не захардкожен, в отличие от самого факта,
	// что это один и тот же счётчик для обоих каналов регистрации и для
	// восстановления — такое разделение (разные колонки/таблицы под одно и
	// то же число) не стоило выражать тремя разными конфигами.
	maxCodeAttempts int
}

// New оборачивает pool в Repository. maxCodeAttempts — см. doc-комментарий
// поля Repository.maxCodeAttempts.
func New(pool *pgxpool.Pool, maxCodeAttempts int) *Repository {
	return &Repository{pool: pool, maxCodeAttempts: maxCodeAttempts}
}

// AuthRecord — данные identity.auth, нужные для начала SRP-входа.
type AuthRecord struct {
	UserID   string
	Salt     []byte
	Verifier []byte
}

// FindAuth ищет учётную запись по identifier (email, телефон или username —
// email/username регистронезависимо). Возвращает ErrNotFound, если запись
// не найдена или для неё ещё не настроен SRP (легаси-аккаунт без salt/verifier).
func (r *Repository) FindAuth(ctx context.Context, identifier string) (AuthRecord, error) {
	var rec AuthRecord
	err := r.pool.QueryRow(ctx,
		`SELECT user_id::text, srp_salt, srp_verifier
		 FROM identity.auth
		 WHERE lower(email) = lower($1) OR phone = $1 OR lower(username) = lower($1)`,
		identifier,
	).Scan(&rec.UserID, &rec.Salt, &rec.Verifier)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthRecord{}, ErrNotFound
	}
	if err != nil {
		return AuthRecord{}, fmt.Errorf("find auth: %w", err)
	}
	if rec.Salt == nil || rec.Verifier == nil {
		return AuthRecord{}, ErrNotFound
	}
	return rec, nil
}

// ChallengeRecord — серверные эфемерные значения SRP-обмена, сохранённые
// CreateChallenge и восстановленные ConsumeChallenge. UserID — nil для
// фейкового challenge'а анти-перечисления (см. CreateChallenge).
type ChallengeRecord struct {
	UserID       *string
	Verifier     []byte
	ServerSecret []byte
	ServerPublic []byte
}

// CreateChallenge сохраняет verifier и эфемерные серверные значения SRP-
// обмена и возвращает идентификатор challenge'а (session_id для клиента).
// userID — nil для фейкового challenge'а несуществующего identifier (защита
// от перечисления аккаунтов, см. srp.FakeCredentials и AuthHandler.
// InitializeSession): у такого challenge'а нет реального владельца, и
// хранится он той же вставкой, что и настоящий, чтобы оба пути были
// неотличимы по форме и по времени выполнения.
//
// Попутно (в том же запросе) удаляются просроченные challenge'ы — отдельная
// фоновая задача для этого не нужна, так как каждый новый вход сам чистит
// накопившийся мусор.
func (r *Repository) CreateChallenge(ctx context.Context, userID *string, verifier, serverSecret, serverPublic []byte) (string, error) {
	var id string
	err := r.pool.QueryRow(ctx,
		`WITH cleanup AS (
		     DELETE FROM identity.auth_challenges WHERE expires_at <= now()
		 )
		 INSERT INTO identity.auth_challenges (user_id, verifier, server_secret, server_public, expires_at)
		 VALUES ($1::uuid, $2, $3, $4, $5)
		 RETURNING id::text`,
		userID, verifier, serverSecret, serverPublic, time.Now().Add(challengeTTL),
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create challenge: %w", err)
	}
	return id, nil
}

// ConsumeChallenge одноразово забирает challenge по id: повторный вызов с
// тем же id, просроченный или несуществующий challenge вернёт ErrNotFound.
func (r *Repository) ConsumeChallenge(ctx context.Context, challengeID string) (ChallengeRecord, error) {
	var rec ChallengeRecord
	err := r.pool.QueryRow(ctx,
		`DELETE FROM identity.auth_challenges
		 WHERE id = $1::uuid AND expires_at > now()
		 RETURNING user_id::text, verifier, server_secret, server_public`,
		challengeID,
	).Scan(&rec.UserID, &rec.Verifier, &rec.ServerSecret, &rec.ServerPublic)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return ChallengeRecord{}, ErrNotFound
	}
	if err != nil {
		return ChallengeRecord{}, fmt.Errorf("consume challenge: %w", err)
	}
	return rec, nil
}

// GetUserStatus возвращает identity.users.status — вызывается
// CompleteAuthentication после успешной проверки SRP-доказательства:
// войти может только пользователь в статусе UserStatusActive (см.
// doc-комментарий CompleteAuthentication в auth.go). Отдельный запрос, не
// часть ConsumeChallenge: challenge может принадлежать несуществующему
// пользователю (анти-перечисление, см. CreateChallenge), и до проверки
// доказательства решать, существует ли реальный аккаунт, незачем.
func (r *Repository) GetUserStatus(ctx context.Context, userID string) (string, error) {
	var status string
	err := r.pool.QueryRow(ctx,
		`SELECT status FROM identity.users WHERE id = $1::uuid`, userID,
	).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get user status: %w", err)
	}
	return status, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isInvalidUUID true, если ошибка вызвана тем, что переданная строка не
// является корректным UUID (клиент прислал мусор вместо session_id) — для
// вызывающего кода это равносильно "не найдено".
func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}
