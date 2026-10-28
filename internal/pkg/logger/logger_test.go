package logger

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
)

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func TestColorHandlerFormat(t *testing.T) {
	var buf bytes.Buffer
	log := newModuleLogger(&buf, false, "Identity", "info")

	log.With("module", "ignored", "req", 42).WithGroup("db").Info("hello", "error", errors.New("boom"))
	log.Debug("hidden")

	line := ansi.ReplaceAllString(buf.String(), "")
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("expected exactly one line, got %q", line)
	}
	for _, want := range []string{"[Identity] [INFO] hello", " req=42", " db.error=boom"} {
		if !strings.Contains(line, want) {
			t.Errorf("output %q does not contain %q", line, want)
		}
	}
	if strings.Contains(line, "module=") {
		t.Errorf("module attribute must not be duplicated: %q", line)
	}
}

func TestColorHandlerConcurrentWritesDoNotInterleave(t *testing.T) {
	var buf bytes.Buffer
	log := newModuleLogger(&buf, false, "M", "debug")

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { log.Info("message", "k", "v") })
	}
	wg.Wait()

	for _, line := range strings.Split(strings.TrimSpace(ansi.ReplaceAllString(buf.String(), "")), "\n") {
		if !strings.HasSuffix(line, "[M] [INFO] message k=v") {
			t.Fatalf("malformed line %q", line)
		}
	}
}

func TestProductionLoggerIsJSON(t *testing.T) {
	var buf bytes.Buffer
	newModuleLogger(&buf, true, "Gateway", "warn").Warn("careful", "n", 1)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("invalid json %q: %v", buf.String(), err)
	}
	if rec["module"] != "Gateway" || rec["msg"] != "careful" {
		t.Fatalf("unexpected record %v", rec)
	}
}
