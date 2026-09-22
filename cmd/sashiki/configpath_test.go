package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rikukadev/sashiki/internal/config"
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

// --app-pass に YAML を壊す文字が入っても、読み戻した値が一致する(#310 review)。
func TestRenderConfigQuotesAppPass(t *testing.T) {
	for _, pass := range []string{`foo: bar`, `foo # bar`, `say "hi"`, `back\slash`, "plain"} {
		data, err := renderConfigApp("pool", pass)
		if err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(f, data, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(f)
		if err != nil {
			t.Fatalf("config with app_pass %q must stay loadable: %v", pass, err)
		}
		if got := cfg.AppPass(); got != pass {
			t.Errorf("app_pass round-trip: got %q, want %q", got, pass)
		}
	}
}

// 引用符やバックスラッシュを含むパスワードでも SQL が壊れない。
func TestCreateAppUserSQLEscapes(t *testing.T) {
	sql := createAppUserSQL("dev", "caching_sha2_password", `it's \ "x"`)
	if !strings.Contains(sql, `BY 'it\'s \\ "x"'`) {
		t.Errorf("password not escaped: %s", sql)
	}
	if !strings.Contains(sql, `CREATE USER IF NOT EXISTS 'dev'@'%'`) {
		t.Errorf("user not quoted: %s", sql)
	}
}

// runtimeDir は run_dir を使い、長すぎる/作れない場合は /tmp に落とす(#308)。
func TestRuntimeDir(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{RunDir: dir}
	if got := runtimePath(cfg, "x.sock"); got != filepath.Join(dir, "x.sock") {
		t.Errorf("runtimePath = %q, want under %q", got, dir)
	}
	long := config.Config{RunDir: filepath.Join(dir, strings.Repeat("d", 90))}
	if got := runtimeDir(long); got != os.TempDir() {
		t.Errorf("too-long run_dir should fall back to TempDir, got %q", got)
	}
	if got := runtimeDir(config.Config{}); got != os.TempDir() {
		t.Errorf("empty run_dir should fall back to TempDir, got %q", got)
	}
}
