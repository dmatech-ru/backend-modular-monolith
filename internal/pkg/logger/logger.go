package logger

import (
	"context"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
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

// ColorHandler — slog.Handler для dev-окружения: цветной вывод вида
// "time [Module] [LEVEL] message key=value". Каждая запись пишется одним
// вызовом Write под общим мьютексом, поэтому строки из разных горутин
// не перемешиваются.
type ColorHandler struct {
	w          io.Writer
	mu         *sync.Mutex
	level      slog.Leveler
	moduleName string
	attrs      []byte // уже отформатированные атрибуты из WithAttrs
	groups     []string
}

func NewColorHandler(w io.Writer, moduleName string, level slog.Leveler) *ColorHandler {
	return &ColorHandler{w: w, mu: &sync.Mutex{}, level: level, moduleName: moduleName}
}

func (h *ColorHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *ColorHandler) Handle(_ context.Context, r slog.Record) error {
	buf := make([]byte, 0, 256)

	if !r.Time.IsZero() {
		buf = r.Time.AppendFormat(buf, "2006-01-02 15:04:05.000")
		buf = append(buf, ' ')
	}
	if h.moduleName != "" {
		buf = append(buf, colorBoldWhite+"["+h.moduleName+"]"+colorReset+" "...)
	} else {
		buf = append(buf, "[system] "...)
	}
	buf = append(buf, "["+levelString(r.Level)+"] "...)
	buf = append(buf, r.Message...)
	buf = append(buf, h.attrs...)

	prefix := h.groupPrefix()
	r.Attrs(func(a slog.Attr) bool {
		buf = appendAttr(buf, prefix, a)
		return true
	})
	buf = append(buf, '\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.w.Write(buf)
	return err
}

func (h *ColorHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	h2 := *h
	h2.attrs = slices.Clip(h.attrs)
	prefix := h.groupPrefix()
	for _, a := range attrs {
		h2.attrs = appendAttr(h2.attrs, prefix, a)
	}
	return &h2
}

func (h *ColorHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h2 := *h
	h2.groups = append(slices.Clip(h.groups), name)
	return &h2
}

func (h *ColorHandler) groupPrefix() string {
	if len(h.groups) == 0 {
		return ""
	}
	return strings.Join(h.groups, ".") + "."
}

func appendAttr(buf []byte, prefix string, a slog.Attr) []byte {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return buf
	}
	// Имя модуля уже выведено в заголовке строки.
	if prefix == "" && a.Key == "module" {
		return buf
	}
	if a.Value.Kind() == slog.KindGroup {
		groupPrefix := prefix
		if a.Key != "" {
			groupPrefix += a.Key + "."
		}
		for _, ga := range a.Value.Group() {
			buf = appendAttr(buf, groupPrefix, ga)
		}
		return buf
	}
	buf = append(buf, " "+colorCyan+prefix+a.Key+colorReset+"="...)
	return append(buf, a.Value.String()...)
}

func levelString(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return colorRed + level.String() + colorReset
	case level >= slog.LevelWarn:
		return colorYellow + level.String() + colorReset
	case level >= slog.LevelInfo:
		return colorGreen + level.String() + colorReset
	default:
		return colorBlue + level.String() + colorReset
	}
}

// NewModuleLogger создаёт независимый логгер модуля со своим уровнем фильтрации.
// При APP_ENV=production пишет JSON с атрибутом module, иначе — цветной текст.
func NewModuleLogger(moduleName, levelStr string) *slog.Logger {
	return newModuleLogger(os.Stdout, os.Getenv("APP_ENV") == "production", moduleName, levelStr)
}

func newModuleLogger(w io.Writer, production bool, moduleName, levelStr string) *slog.Logger {
	level := parseLevel(levelStr)

	if production {
		handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
		return slog.New(handler.WithAttrs([]slog.Attr{slog.String("module", moduleName)}))
	}
	return slog.New(NewColorHandler(w, moduleName, level))
}

func parseLevel(levelStr string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(levelStr)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
