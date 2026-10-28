package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	seedsTableName = "schema_seeds"
	seedsTimeout   = 5 * time.Minute
)

// Seed применяет сиды сервиса из каталога на диске (в бинарь они не упаковываются).
//
// Раскладка: <seeds.db>/<level>/*.sql, где level берётся по порядку из seeds.levels
// конфига (например shared, development), а seeds.db — каталог сидов конкретного сервиса. Файлы внутри уровня
// выполняются по возрастанию имени (0001_permissions.sql, 0002_groups.sql ...).
//
// Учёт ведётся в <schema>.schema_seeds по паре (level, name) с контрольной суммой.
// Новый файл выполняется, изменённый — выполняется повторно, поэтому сиды должны быть
// идемпотентными (INSERT ... ON CONFLICT ...). Каждый файл — своя транзакция.
// Отсутствующий каталог уровня пропускается с предупреждением. Параллельные вызовы Seed
// сериализуются через advisory-lock. Вызывать после Migrate из отдельной setup-команды.
//
// dir — корневой каталог сидов сервиса, levels — уровни по порядку применения
// (например, "shared", "development"); раскладка dir/<level>/*.sql.
func Seed(parentCtx context.Context, pool *pgxpool.Pool, log *slog.Logger, schema string, dir string, levels []string) error {
	if dir == "" || len(levels) == 0 {
		log.Info("PostgreSQL: сиды не настроены, пропуск")
		return nil
	}
	if !schemaNameRe.MatchString(schema) {
		return fmt.Errorf("сиды: некорректное имя схемы %q", schema)
	}
	if pool == nil {
		return fmt.Errorf("сиды: нет подключения к PostgreSQL")
	}
	seedsTable := pgx.Identifier{schema, seedsTableName}.Sanitize()

	type seedFile struct {
		level, name, sql, checksum string
	}
	var files []seedFile
	for _, level := range levels {
		if level == "" || level != filepath.Base(level) || level == "." || level == ".." {
			return fmt.Errorf("сиды: некорректное имя уровня %q", level)
		}
		levelDir := filepath.Join(dir, level)
		entries, err := os.ReadDir(levelDir)
		if errors.Is(err, fs.ErrNotExist) {
			log.Warn("PostgreSQL: каталог сидов не найден, уровень пропущен", "level", level, "dir", levelDir)
			continue
		}
		if err != nil {
			return fmt.Errorf("сиды: не удалось прочитать %s: %w", levelDir, err)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(levelDir, e.Name()))
			if err != nil {
				return fmt.Errorf("сиды: не удалось прочитать %s/%s: %w", level, e.Name(), err)
			}
			sum := sha256.Sum256(data)
			files = append(files, seedFile{level, e.Name(), string(data), hex.EncodeToString(sum[:])})
		}
	}

	ctx, cancel := context.WithTimeout(parentCtx, seedsTimeout)
	defer cancel()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("сиды: не удалось получить соединение: %w", err)
	}
	defer conn.Release()

	lockKey := "seeds:" + schema
	log.Info("PostgreSQL: ожидание блокировки сидов")
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", lockKey); err != nil {
		return fmt.Errorf("сиды: не удалось взять блокировку: %w", err)
	}
	defer func() {
		unlockCtx, unlockCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer unlockCancel()
		if _, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock(hashtext($1))", lockKey); err != nil {
			log.Warn("PostgreSQL: не удалось снять блокировку сидов", "error", err)
		}
	}()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+seedsTable+` (
		level      TEXT        NOT NULL,
		name       TEXT        NOT NULL,
		checksum   TEXT        NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (level, name)
	)`); err != nil {
		return fmt.Errorf("сиды: не удалось создать таблицу %s: %w", seedsTable, err)
	}

	applied := map[string]string{}
	rows, err := conn.Query(ctx, `SELECT level, name, checksum FROM `+seedsTable)
	if err != nil {
		return fmt.Errorf("сиды: не удалось прочитать %s: %w", seedsTable, err)
	}
	for rows.Next() {
		var level, name, checksum string
		if err := rows.Scan(&level, &name, &checksum); err != nil {
			rows.Close()
			return fmt.Errorf("сиды: %w", err)
		}
		applied[level+"/"+name] = checksum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("сиды: %w", err)
	}

	count := 0
	for _, f := range files {
		if applied[f.level+"/"+f.name] == f.checksum {
			continue
		}
		if err := applySeed(ctx, conn, seedsTable, f.level, f.name, f.sql, f.checksum); err != nil {
			log.Error("PostgreSQL: сид не применён", "level", f.level, "name", f.name, "error", err)
			return fmt.Errorf("сиды: %s/%s: %w", f.level, f.name, err)
		}
		log.Info("PostgreSQL: сид применён", "level", f.level, "name", f.name)
		count++
	}
	if count == 0 {
		log.Info("PostgreSQL: сиды актуальны, изменений нет")
	}
	return nil
}

func applySeed(ctx context.Context, conn interface {
	Begin(context.Context) (pgx.Tx, error)
}, seedsTable, level, name, sql, checksum string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := tx.Exec(ctx, sql); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+seedsTable+` (level, name, checksum) VALUES ($1, $2, $3)
		ON CONFLICT (level, name) DO UPDATE SET checksum = EXCLUDED.checksum, applied_at = now()`,
		level, name, checksum); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
