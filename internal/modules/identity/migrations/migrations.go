package migrations

import "embed"

// FS содержит SQL-миграции сервиса (0001_name.sql, 0002_name.sql, ...), вшитые в бинарь.
//
//go:embed *.sql
var FS embed.FS
