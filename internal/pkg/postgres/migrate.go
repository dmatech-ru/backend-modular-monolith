package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	migrationsTableName = "schema_migrations"
	migrationsTimeout   = 5 * time.Minute
)

var schemaNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Имя файла: <версия>_<описание>.sql, например 0001_init.sql.
var migrationFileRe = regexp.MustCompile(`^(\d+)_([A-Za-z0-9_\-]+)\.sql$`)

type migration struct {
	version  int64
	name     string
	sql      string
	checksum string
}

// Migrate применяет к базе все ещё не применённые миграции из fsys (плоская папка *.sql).
//
// Каждый сервис владеет собственной схемой PostgreSQL: Migrate создаёт её (если нет) и хранит
// учёт применённых миграций внутри неё (<schema>.schema_migrations), поэтому версии разных
// сервисов в одной базе не пересекаются. Таблицы в самих миграциях указываются с префиксом схемы.
//
// Миграции выполняются строго по возрастанию версии, каждая в своей транзакции вместе с записью
// в таблицу schema_migrations. Параллельные вызовы Migrate сериализуются через advisory-lock.
// Запускайте миграции отдельной setup-командой, а не из каждого application pod.
// Применённую миграцию менять нельзя: изменённый файл определяется по контрольной сумме
// и приводит к ошибке.
func Migrate(parentCtx context.Context, pool *pgxpool.Pool, log *slog.Logger, schema string, fsys fs.FS) error {
	if !schemaNameRe.MatchString(schema) {
		return fmt.Errorf("миграции: некорректное имя схемы %q", schema)
	}
	migrationsTable := pgx.Identifier{schema, migrationsTableName}.Sanitize()

	if pool == nil {
		return fmt.Errorf("миграции: нет подключения к PostgreSQL")
	}

	migrations, err := loadMigrations(fsys)
	if err != nil {
		log.Error("PostgreSQL: не удалось прочитать миграции", "error", err)
		return err
	}

	ctx, cancel := context.WithTimeout(parentCtx, migrationsTimeout)
	defer cancel()

	// Lock сессионный, поэтому всё выполняется на одном выделенном соединении.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("миграции: не удалось получить соединение: %w", err)
	}
	defer conn.Release()

	lockKey := "migrations:" + schema
	log.Info("PostgreSQL: ожидание блокировки миграций")
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", lockKey); err != nil {
		return fmt.Errorf("миграции: не удалось взять блокировку: %w", err)
	}
	defer func() {
		// ctx может быть уже отменён, поэтому разблокировка идёт на отдельном контексте.
		unlockCtx, unlockCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer unlockCancel()
		if _, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock(hashtext($1))", lockKey); err != nil {
			log.Warn("PostgreSQL: не удалось снять блокировку миграций", "error", err)
		}
	}()

	if _, err := conn.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+pgx.Identifier{schema}.Sanitize()); err != nil {
		return fmt.Errorf("миграции: не удалось создать схему %s: %w", schema, err)
	}

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+migrationsTable+` (
		version    BIGINT      PRIMARY KEY,
		name       TEXT        NOT NULL,
		checksum   TEXT        NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("миграции: не удалось создать таблицу %s: %w", migrationsTable, err)
	}

	applied, err := appliedMigrations(ctx, conn, migrationsTable)
	if err != nil {
		return err
	}

	// Применённая версия обязана существовать в коде и совпадать по содержимому.
	known := make(map[int64]migration, len(migrations))
	for _, m := range migrations {
		known[m.version] = m
	}
	for version, checksum := range applied {
		m, ok := known[version]
		if !ok {
			return fmt.Errorf("миграции: версия %d применена в базе, но её файла нет в сервисе (запущена более старая сборка?)", version)
		}
		if m.checksum != checksum {
			return fmt.Errorf("миграции: файл %04d_%s.sql изменён после применения", m.version, m.name)
		}
	}

	count := 0
	for _, m := range migrations {
		if _, done := applied[m.version]; done {
			continue
		}
		if err := applyMigration(ctx, conn, migrationsTable, m); err != nil {
			log.Error("PostgreSQL: миграция не применена", "version", m.version, "name", m.name, "error", err)
			return fmt.Errorf("миграции: %04d_%s.sql: %w", m.version, m.name, err)
		}
		log.Info("PostgreSQL: миграция применена", "version", m.version, "name", m.name)
		count++
	}

	if count == 0 {
		log.Info("PostgreSQL: схема актуальна, миграций нет")
	}
	return nil
}

func applyMigration(ctx context.Context, conn interface {
	Begin(context.Context) (pgx.Tx, error)
}, migrationsTable string, m migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Без аргументов pgx выполняет текст как есть, поэтому в файле может быть несколько команд.
	if _, err := tx.Exec(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO `+migrationsTable+` (version, name, checksum) VALUES ($1, $2, $3)`,
		m.version, m.name, m.checksum); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func appliedMigrations(ctx context.Context, conn interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, migrationsTable string) (map[int64]string, error) {
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM `+migrationsTable)
	if err != nil {
		return nil, fmt.Errorf("миграции: не удалось прочитать %s: %w", migrationsTable, err)
	}
	defer rows.Close()

	applied := map[int64]string{}
	for rows.Next() {
		var version int64
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("миграции: %w", err)
		}
		applied[version] = checksum
	}
	return applied, rows.Err()
}

// loadMigrations читает *.sql из корня fsys и сортирует по версии.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("миграции: не удалось прочитать каталог: %w", err)
	}

	var result []migration
	seen := map[int64]string{}
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".sql" {
			continue
		}

		match := migrationFileRe.FindStringSubmatch(e.Name())
		if match == nil {
			return nil, fmt.Errorf("миграции: имя файла %q должно быть вида 0001_name.sql", e.Name())
		}
		version, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("миграции: некорректная версия в %q: %w", e.Name(), err)
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("миграции: версия %d повторяется (%s, %s)", version, prev, e.Name())
		}
		seen[version] = e.Name()

		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("миграции: не удалось прочитать %q: %w", e.Name(), err)
		}
		sum := sha256.Sum256(body)
		result = append(result, migration{
			version:  version,
			name:     match[2],
			sql:      string(body),
			checksum: hex.EncodeToString(sum[:]),
		})
	}

	sort.Slice(result, func(i, j int) bool { return result[i].version < result[j].version })
	return result, nil
}
