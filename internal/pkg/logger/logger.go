package logger

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// ANSI цветовые коды для терминала
const (
	colorReset     = "\033[0m"
	colorRed       = "\033[31m"   // Error
	colorGreen     = "\033[32m"   // Info
	colorYellow    = "\033[33m"   // Warn
	colorBlue      = "\033[34m"   // Debug
	colorCyan      = "\033[36m"   // Attributes
	colorBoldWhite = "\033[1;37m" // Имя модуля
)

// ColorHandler — кастомный хендлер для честного раскрашивания консоли
type ColorHandler struct {
	slog.Handler
	moduleName string // 🪄 Храним имя модуля прямо в хендлере
}

func (h *ColorHandler) Handle(ctx context.Context, r slog.Record) error {
	var levelStr string

	switch r.Level {
	case slog.LevelDebug:
		levelStr = colorBlue + "DEBUG" + colorReset
	case slog.LevelInfo:
		levelStr = colorGreen + "INFO" + colorReset
	case slog.LevelWarn:
		levelStr = colorYellow + "WARN" + colorReset
	case slog.LevelError:
		levelStr = colorRed + "ERROR" + colorReset
	default:
		levelStr = r.Level.String()
	}

	timeStr := r.Time.Format("2006-01-02 15:04:05.000")

	// Выводим строго по твоему шаблону: [ModuleName] [LEVEL] Message
	if h.moduleName != "" {
		fmt.Printf("%s %s[%s]%s [%s] %s", timeStr, colorBoldWhite, h.moduleName, colorReset, levelStr, r.Message)
	} else {
		fmt.Printf("%s [system] [%s] %s", timeStr, levelStr, r.Message)
	}

	// Выводим остальные доп-параметры (исключая module)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "module" {
			return true
		}
		fmt.Printf(" %s%s%s=%v", colorCyan, a.Key, colorReset, a.Value.Any())
		return true
	})

	fmt.Println()
	return nil
}

// 🪄 НОВЫЙ ИЗОЛИРОВАННЫЙ КОНСТРУКТОР ДЛЯ МОДУЛЕЙ
// Создает абсолютно независимый логгер со своим личным уровнем фильтрации!
func NewModuleLogger(moduleName, levelStr string) *slog.Logger {
	level := parseLevel(levelStr)
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if os.Getenv("APP_ENV") == "production" {
		// На проде — JSON, но тоже с пришитым тегом модуля и своим уровнем
		handler = slog.NewJSONHandler(os.Stdout, opts).WithAttrs([]slog.Attr{slog.String("module", moduleName)})
	} else {
		// В деве — наш красивый ColorHandler с изоляцией
		handler = &ColorHandler{
			Handler:    slog.NewTextHandler(os.Stdout, opts),
			moduleName: moduleName,
		}
	}

	return slog.New(handler)
}

func parseLevel(levelStr string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(levelStr)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
