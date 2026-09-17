package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 既存 baseline があれば失敗せず新しい tag で取り直す(#293)。
func TestLocalBaselineTag(t *testing.T) {
	snapDir := t.TempDir()
	if tag, replacing := localBaselineTag(snapDir, "baseline"); tag != "baseline" || replacing {
		t.Errorf("first import should use the preferred tag, got %q replacing=%v", tag, replacing)
	}
	if err := os.MkdirAll(filepath.Join(snapDir, "baseline"), 0o755); err != nil {
		t.Fatal(err)
	}
	tag, replacing := localBaselineTag(snapDir, "baseline")
	if !replacing || tag == "baseline" || len(tag) <= len("baseline-") {
		t.Errorf("existing baseline should yield a fresh tag, got %q replacing=%v", tag, replacing)
	}
}

// 初期化済みの base/data は空にしてから import する。DB が動いていれば拒否。
func TestPrepareLocalBaseData(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dataDir, "ibdata1"), []byte("x"), 0o644)
	if err := prepareLocalBaseData(dataDir, true); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Error("initialized base/data should be cleared for re-import")
	}

	_ = os.MkdirAll(dataDir, 0o755)
	_ = os.WriteFile(filepath.Join(dataDir, "mysqld.pid"), []byte("1"), 0o644)
	if err := prepareLocalBaseData(dataDir, true); err == nil {
		t.Error("a running mysqld on base/data must refuse the import")
	}
}

// SASHIKI_CONFIG が最優先(#293)。
func TestDefaultConfigPathEnvOverride(t *testing.T) {
	t.Setenv("SASHIKI_CONFIG", "/tmp/x.yaml")
	if got := defaultConfigPath(); got != "/tmp/x.yaml" {
		t.Errorf("defaultConfigPath = %q", got)
	}
}
