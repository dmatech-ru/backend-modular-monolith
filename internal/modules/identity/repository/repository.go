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
)

// challengeTTL — как долго живёт challenge второго шага SRP-аутентификации
// после InitializeSession.
const challengeTTL = 2 * time.Minute

// Repository — репозиторий identity-модуля поверх пула соединений PostgreSQL.
type Repository struct {
	pool *pgxpool.Pool
}

// New оборачивает pool в Repository.
func New(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
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
