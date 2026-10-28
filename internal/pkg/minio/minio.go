// Package minio управляет подключением к MinIO/S3 и сидированием файловых
// ассетов модуля.
package minio

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config — параметры подключения к MinIO/S3.
type Config struct {
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	Secure          bool
	ConnectTimeout  time.Duration

	// Bucket проверяется сразу при подключении (BucketExists), чтобы поймать
	// ошибки конфигурации/прав на старте, а не при первом реальном запросе.
	Bucket string

	MaxIdleConns        int
	MaxIdleConnsPerHost int
	MaxConnsPerHost     int
}

// Connect создаёт клиент MinIO/S3 и проверяет его доступом к cfg.Bucket.
func Connect(ctx context.Context, cfg Config, log *slog.Logger) (*minio.Client, error) {
	if cfg.Endpoint == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" ||
		cfg.ConnectTimeout <= 0 || cfg.MaxIdleConns < 0 ||
		cfg.MaxIdleConnsPerHost <= 0 || cfg.MaxConnsPerHost < 0 || cfg.Bucket == "" {
		return nil, fmt.Errorf("minio: endpoint, credentials, bucket, connection limits, and positive connect timeout are required")
	}

	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("minio: default HTTP transport has unsupported type %T", http.DefaultTransport)
	}
	minioTransport := transport.Clone()
	minioTransport.MaxIdleConns = cfg.MaxIdleConns
	minioTransport.MaxIdleConnsPerHost = cfg.MaxIdleConnsPerHost
	minioTransport.MaxConnsPerHost = cfg.MaxConnsPerHost

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:     credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure:    cfg.Secure,
		Transport: minioTransport,
	})
	if err != nil {
		return nil, fmt.Errorf("minio: invalid client configuration: %w", err)
	}

	connectCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if _, err := client.BucketExists(connectCtx, cfg.Bucket); err != nil {
		log.Error("MinIO: не удалось подключиться", "error", err)
		return nil, fmt.Errorf("minio: connection check failed: %w", err)
	}

	log.Info("MinIO: клиент подключён", "endpoint", cfg.Endpoint)
	return client, nil
}
