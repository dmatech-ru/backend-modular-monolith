package config

import (
	"os"
	"path/filepath"
	"testing"
)

type testConfig struct {
	Name  string `yaml:"name" env:"CONFIG_TEST_NAME" env-default:"default"`
	Level string `yaml:"level" env:"CONFIG_TEST_LEVEL" env-default:"info"`
}

func TestLoadFromFileWithEnvOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(path, []byte("name: from-file\nlevel: debug\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_TEST_PATH", path)
	t.Setenv("CONFIG_TEST_LEVEL", "error")

	cfg, err := Load[testConfig]("CONFIG_TEST_PATH")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "from-file" || cfg.Level != "error" {
		t.Fatalf("unexpected config %+v", cfg)
	}
}

func TestLoadFallsBackToDefaults(t *testing.T) {
	t.Setenv("CONFIG_TEST_PATH", filepath.Join(t.TempDir(), "missing.yaml"))

	cfg, err := Load[testConfig]("CONFIG_TEST_PATH")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "default" || cfg.Level != "info" {
		t.Fatalf("unexpected config %+v", cfg)
	}
}

func TestLoadWithoutPath(t *testing.T) {
	t.Setenv("CONFIG_TEST_PATH", "")
	t.Setenv("CONFIG_TEST_NAME", "from-env")

	cfg, err := Load[testConfig]("CONFIG_TEST_PATH")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "from-env" {
		t.Fatalf("unexpected config %+v", cfg)
	}
}
