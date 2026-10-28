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

// FindAuth ищет учётную запись по identifier (email или телефон,
// регистронезависимо для email). Возвращает ErrNotFound, если запись не
// найдена или для неё ещё не настроен SRP (легаси-аккаунт без salt/verifier).
func (r *Repository) FindAuth(ctx context.Context, identifier string) (AuthRecord, error) {
	var rec AuthRecord
	err := r.pool.QueryRow(ctx,
		`SELECT user_id::text, srp_salt, srp_verifier
		 FROM identity.auth
		 WHERE lower(email) = lower($1) OR phone = $1`,
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
// CreateChallenge и восстановленные ConsumeChallenge.
type ChallengeRecord struct {
	UserID       string
	Verifier     []byte
	ServerSecret []byte
	ServerPublic []byte
}

// CreateChallenge сохраняет verifier и эфемерные серверные значения SRP-
// обмена и возвращает идентификатор challenge'а (session_id для клиента).
func (r *Repository) CreateChallenge(ctx context.Context, userID string, verifier, serverSecret, serverPublic []byte) (string, error) {
	var id string
	err := r.pool.QueryRow(ctx,
		`INSERT INTO identity.auth_challenges (user_id, verifier, server_secret, server_public, expires_at)
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

// CreateUserParams — данные для регистрации нового пользователя. Salt и
// Verifier вычислены клиентом (сервер пароль не видит); ровно одно из
// Email/Phone должно быть заполнено.
type CreateUserParams struct {
	Email, Phone                    string
	Salt, Verifier                  []byte
	FirstName, LastName, MiddleName string
}

// CreateUser создаёт пользователя, профиль и запись SRP-аутентификации
// одной транзакцией. Новый пользователь получает статус 'pending' (значение
// по умолчанию в identity.users) — активация/подтверждение identifier'а не
// входит в эту функцию. Возвращает ErrAlreadyExists при конфликте email/телефона.
func (r *Repository) CreateUser(ctx context.Context, p CreateUserParams) (string, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("create user: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var userID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO identity.users DEFAULT VALUES RETURNING id::text`,
	).Scan(&userID); err != nil {
		return "", fmt.Errorf("create user: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO identity.profiles (user_id, first_name, last_name, middle_name)
		 VALUES ($1::uuid, $2, $3, $4)`,
		userID, p.FirstName, p.LastName, p.MiddleName,
	); err != nil {
		return "", fmt.Errorf("create profile: %w", err)
	}

	var email, phone *string
	if p.Email != "" {
		email = &p.Email
	}
	if p.Phone != "" {
		phone = &p.Phone
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO identity.auth (user_id, email, phone, srp_salt, srp_verifier)
		 VALUES ($1::uuid, $2, $3, $4, $5)`,
		userID, email, phone, p.Salt, p.Verifier,
	); err != nil {
		if isUniqueViolation(err) {
			return "", ErrAlreadyExists
		}
		return "", fmt.Errorf("create auth: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("create user: %w", err)
	}
	return userID, nil
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
