package minio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	minioSDK "github.com/minio/minio-go/v7"
)

type assetFile struct {
	level string
	name  string
	path  string
}

// SeedAssets заливает файловые ассеты сервиса из каталога dir в bucket.
//
// Раскладка: dir/<level>/*, где level берётся по порядку из levels
// (например "shared", "development"); более поздний уровень переопределяет
// файлы с тем же путём из более раннего (оверлей). Загрузка идемпотентна:
// файл перезаливается только если изменилась его контрольная сумма (hash
// хранится в пользовательских метаданных объекта).
func SeedAssets(ctx context.Context, client *minioSDK.Client, log *slog.Logger, bucket string, timeout time.Duration, dir string, levels []string) error {
	if dir == "" || len(levels) == 0 {
		log.Info("MinIO: сиды файлов не настроены, пропуск")
		return nil
	}
	if bucket == "" {
		return fmt.Errorf("сиды файлов: bucket is required")
	}
	if timeout <= 0 {
		return fmt.Errorf("сиды файлов: timeout must be positive")
	}
	if client == nil {
		return fmt.Errorf("сиды файлов: нет подключения к MinIO")
	}

	files, missingLevels, err := collectAssets(dir, levels)
	if err != nil {
		return err
	}
	for _, level := range missingLevels {
		log.Warn("MinIO: каталог сидов не найден, уровень пропущен",
			"level", level, "dir", filepath.Join(dir, level))
	}
	if len(files) == 0 {
		log.Info("MinIO: файловые сиды отсутствуют")
		return nil
	}

	seedCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ensureBucket(seedCtx, client, bucket); err != nil {
		return fmt.Errorf("сиды файлов: bucket %q: %w", bucket, err)
	}

	uploaded := 0
	for _, file := range files {
		changed, err := seedAsset(seedCtx, client, bucket, file)
		if err != nil {
			log.Error("MinIO: файл не загружен", "level", file.level, "name", file.name, "error", err)
			return fmt.Errorf("сиды файлов: %s/%s: %w", file.level, file.name, err)
		}
		if changed {
			log.Info("MinIO: файл загружен", "level", file.level, "name", file.name, "key", file.name)
			uploaded++
		}
	}
	log.Info("MinIO: файловые сиды завершены", "bucket", bucket,
		"uploaded", uploaded, "checked", len(files))
	return nil
}

func collectAssets(root string, levels []string) ([]assetFile, []string, error) {
	filesByKey := make(map[string]assetFile)
	missingLevels := make([]string, 0)

	for _, level := range levels {
		if level == "" || level != filepath.Base(level) || level == "." || level == ".." {
			return nil, nil, fmt.Errorf("сиды файлов: некорректное имя уровня %q", level)
		}
		levelDir := filepath.Join(root, level)
		levelInfo, err := os.Lstat(levelDir)
		if errors.Is(err, fs.ErrNotExist) {
			missingLevels = append(missingLevels, level)
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("сиды файлов: не удалось проверить %s: %w", levelDir, err)
		}
		if levelInfo.Mode()&fs.ModeSymlink != 0 {
			return nil, nil, fmt.Errorf("сиды файлов: уровень не может быть символической ссылкой: %s", levelDir)
		}
		if !levelInfo.IsDir() {
			return nil, nil, fmt.Errorf("сиды файлов: %s не является каталогом", levelDir)
		}

		err = fs.WalkDir(os.DirFS(levelDir), ".", func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if name == "." || entry.IsDir() {
				return nil
			}
			if entry.Type()&fs.ModeSymlink != 0 {
				return fmt.Errorf("символические ссылки не поддерживаются: %s/%s", level, name)
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("поддерживаются только обычные файлы: %s/%s", level, name)
			}

			key := path.Clean(filepath.ToSlash(name))
			if key == "." || key == ".." || strings.HasPrefix(key, "../") || strings.HasPrefix(key, "/") {
				return fmt.Errorf("некорректный путь объекта %q", name)
			}
			filesByKey[key] = assetFile{
				level: level,
				name:  key,
				path:  filepath.Join(levelDir, filepath.FromSlash(name)),
			}
			return nil
		})
		if err != nil {
			return nil, nil, fmt.Errorf("сиды файлов: ошибка обхода %s: %w", levelDir, err)
		}
	}

	files := make([]assetFile, 0, len(filesByKey))
	for _, file := range filesByKey {
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return files, missingLevels, nil
}

func ensureBucket(ctx context.Context, client *minioSDK.Client, bucket string) error {
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if err := client.MakeBucket(ctx, bucket, minioSDK.MakeBucketOptions{}); err != nil {
		if minioSDK.ToErrorResponse(err).Code == "BucketAlreadyOwnedByYou" ||
			minioSDK.ToErrorResponse(err).Code == "BucketAlreadyExists" {
			exists, checkErr := client.BucketExists(ctx, bucket)
			if checkErr != nil {
				return fmt.Errorf("создание bucket вернуло %v, повторная проверка завершилась ошибкой: %w", err, checkErr)
			}
			if exists {
				return nil
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

func seedAsset(ctx context.Context, client *minioSDK.Client, bucket string, file assetFile) (bool, error) {
	source, err := os.Open(file.path)
	if err != nil {
		return false, fmt.Errorf("не удалось открыть файл: %w", err)
	}
	defer source.Close()

	info, err := source.Stat()
	if err != nil {
		return false, fmt.Errorf("не удалось прочитать метаданные файла: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("источник больше не является обычным файлом")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, source); err != nil {
		return false, fmt.Errorf("не удалось вычислить контрольную сумму: %w", err)
	}
	checksum := hex.EncodeToString(hash.Sum(nil))
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("превышен лимит времени сидирования: %w", err)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return false, fmt.Errorf("не удалось перемотать файл: %w", err)
	}

	current, err := client.StatObject(ctx, bucket, file.name, minioSDK.StatObjectOptions{})
	if err == nil && objectChecksum(current.UserMetadata) == checksum && current.Size == info.Size() {
		return false, nil
	}
	if err != nil && !isMissingObject(err) {
		return false, fmt.Errorf("не удалось проверить существующий объект: %w", err)
	}

	contentType := mime.TypeByExtension(path.Ext(file.name))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err = client.PutObject(ctx, bucket, file.name, source, info.Size(), minioSDK.PutObjectOptions{
		ContentType: contentType,
		UserMetadata: map[string]string{
			"sha256":     checksum,
			"seed-level": file.level,
		},
	})
	if err != nil {
		return false, fmt.Errorf("не удалось загрузить объект: %w", err)
	}
	return true, nil
}

func objectChecksum(metadata map[string]string) string {
	for key, value := range metadata {
		key = strings.ToLower(strings.TrimPrefix(strings.ToLower(key), "x-amz-meta-"))
		if key == "sha256" {
			return value
		}
	}
	return ""
}

func isMissingObject(err error) bool {
	switch minioSDK.ToErrorResponse(err).Code {
	case "NoSuchKey", "NoSuchObject", "NotFound":
		return true
	default:
		return false
	}
}
