package minio

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCollectAssetsAppliesLevelsAsOverlays(t *testing.T) {
	root := t.TempDir()
	writeAsset(t, root, "shared", "images/logo.svg", "shared logo")
	writeAsset(t, root, "shared", "images/background.svg", "background")
	writeAsset(t, root, "development", "images/logo.svg", "development logo")

	files, missing, err := collectAssets(root, []string{"shared", "development"})
	if err != nil {
		t.Fatalf("collectAssets returned error: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("expected no missing levels, got %v", missing)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 final object keys, got %d", len(files))
	}
	if files[0].name != "images/background.svg" || files[0].level != "shared" {
		t.Errorf("unexpected first object: %+v", files[0])
	}
	if files[1].name != "images/logo.svg" || files[1].level != "development" {
		t.Errorf("unexpected overlay object: %+v", files[1])
	}
}

func TestCollectAssetsSkipsMissingLevel(t *testing.T) {
	root := t.TempDir()
	writeAsset(t, root, "shared", "readme.txt", "content")

	files, missing, err := collectAssets(root, []string{"shared", "development"})
	if err != nil {
		t.Fatalf("collectAssets returned error: %v", err)
	}
	if len(files) != 1 || len(missing) != 1 || missing[0] != "development" {
		t.Fatalf("unexpected collection result: files=%v missing=%v", files, missing)
	}
}

func TestCollectAssetsRejectsInvalidLevel(t *testing.T) {
	if _, _, err := collectAssets(t.TempDir(), []string{"../outside"}); err == nil {
		t.Fatal("expected invalid level to be rejected")
	}
}

func writeAsset(t *testing.T, root, level, name, content string) {
	t.Helper()
	filePath := filepath.Join(root, level, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("create asset directory: %v", err)
	}
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatalf("write asset: %v", err)
	}
}
