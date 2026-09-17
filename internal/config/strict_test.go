package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseSize(t *testing.T) {
	ok := map[string]int64{
		"": 0, "1024": 1024, "512k": 512 << 10, "256M": 256 << 20, "256MB": 256 << 20,
		"256MiB": 256 << 20, "1G": 1 << 30, "20GiB": 20 << 30, "2 gb": 2 << 30, "1T": 1 << 40,
	}
	for in, want := range ok {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"abc", "256X", "M", "-1G", "80%"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) should fail", bad)
		}
	}
}

// 未知キーは黙って無視せずエラーにする。SPEC の古い書き方(baselines: / トップレベル
// profiles:)を書いても効かないまま動いていた(#300)。
func TestLoadRejectsUnknownKeys(t *testing.T) {
	for name, body := range map[string]string{
		"top-level typo":      "storage:\n  backend: ebs-zfs\nbaselines:\n  keep_last: 3\n",
		"profiles at top":     "profiles:\n  ci: { idle_stop_after: 5m }\n",
		"nested unknown":      "branches:\n  max_running: 10\n",
		"mysqld binary alias": "engine:\n  mysql:\n    binary: /usr/sbin/mysqld\n",
	} {
		_, err := Load(writeCfg(t, body))
		if err == nil {
			t.Errorf("%s: unknown key should be rejected", name)
			continue
		}
		if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "unknown") {
			t.Errorf("%s: error should name the unknown field, got %v", name, err)
		}
	}
	// 旧名 alias(zfs / proxy_user)は引き続き受ける。
	if _, err := Load(writeCfg(t, "storage:\n  backend: zfs\n  zfs:\n    pool: p\nengine:\n  mysql:\n    proxy_user: dev\n")); err != nil {
		t.Errorf("legacy aliases must still load: %v", err)
	}
}

// サイズ文字列の誤りは起動時に落とす(黙って 0 = 無制限にしない)。
func TestLoadRejectsBadSizes(t *testing.T) {
	if _, err := Load(writeCfg(t, "engine:\n  mysql:\n    buffer_pool_size: 256XB\n")); err == nil {
		t.Error("invalid buffer_pool_size should be rejected")
	}
	if _, err := Load(writeCfg(t, "storage:\n  default_storage_quota: 20GiB\n")); err != nil {
		t.Errorf("20GiB should be accepted: %v", err)
	}
}

func TestLoadValidatesWatermarksAndTLSPair(t *testing.T) {
	for name, body := range map[string]string{
		"percent-like":        "storage:\n  high_watermark: 80\n",
		"high above critical": "storage:\n  high_watermark: 0.95\n  critical_watermark: 0.9\n",
		"tls cert only":       "proxy:\n  tls_cert: /etc/ssl/c.pem\n",
		"tls key only":        "proxy:\n  tls_key: /etc/ssl/k.pem\n",
	} {
		if _, err := Load(writeCfg(t, body)); err == nil {
			t.Errorf("%s: should be rejected", name)
		}
	}
	if _, err := Load(writeCfg(t, "storage:\n  high_watermark: 0.8\n  critical_watermark: 0.9\nproxy:\n  tls_cert: c\n  tls_key: k\n")); err != nil {
		t.Errorf("valid watermarks + tls pair should load: %v", err)
	}
}

// init が生成するテンプレートは strict でも読める(未知キーを含まない)。
func TestTemplatesLoadStrictly(t *testing.T) {
	// cmd/sashiki の init テストが renderConfig 経由で Load するので、ここでは
	// SPEC 21 章のサンプルを実際に読めることを確かめる(ドキュメントと実装のずれ防止)。
	b, err := os.ReadFile("../../docs/SPEC.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	start := strings.Index(doc, "## 21. 設定ファイル")
	if start < 0 {
		t.Fatal("SPEC 21 章が見つからない")
	}
	rest := doc[start:]
	open := strings.Index(rest, "```yaml\n")
	if open < 0 {
		t.Fatal("SPEC 21 章に yaml ブロックが無い")
	}
	rest = rest[open+len("```yaml\n"):]
	end := strings.Index(rest, "```")
	if _, err := Load(writeCfg(t, rest[:end])); err != nil {
		t.Errorf("SPEC 21 章のサンプル config が読めない(実装とずれている): %v", err)
	}
}
