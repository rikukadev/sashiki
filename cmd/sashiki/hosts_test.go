package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeHosts(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// hosts.yaml の読み込み(#386)。無いのはエラーではないが、壊れているのは
// エラー — 黙って単一ホストの挙動に落ちると意図と違うホストへ操作が飛ぶ。
func TestLoadHostsFrom(t *testing.T) {
	t.Run("ファイルが無ければ nil, nil", func(t *testing.T) {
		hf, err := loadHostsFrom(filepath.Join(t.TempDir(), "nope.yaml"))
		if err != nil || hf != nil {
			t.Errorf("got %v, %v", hf, err)
		}
	})

	t.Run("正常系", func(t *testing.T) {
		p := writeHosts(t, `
default: a
hosts:
  a: { url: "http://a:8080", token: t1 }
  b: { url: "http://b:8080" }
`)
		hf, err := loadHostsFrom(p)
		if err != nil {
			t.Fatal(err)
		}
		if hf.Default != "a" || len(hf.Hosts) != 2 {
			t.Errorf("got %+v", hf)
		}
	})

	t.Run("url が無いホストはエラー", func(t *testing.T) {
		p := writeHosts(t, "hosts:\n  a: { token: x }\n")
		if _, err := loadHostsFrom(p); err == nil {
			t.Error("error を期待したが nil")
		}
	})

	t.Run("default が hosts に無ければエラー", func(t *testing.T) {
		p := writeHosts(t, "default: nope\nhosts:\n  a: { url: \"http://a\" }\n")
		if _, err := loadHostsFrom(p); err == nil {
			t.Error("error を期待したが nil")
		}
	})

	t.Run("壊れた YAML はエラー(黙って無視しない)", func(t *testing.T) {
		p := writeHosts(t, "hosts: [a: b\n")
		if _, err := loadHostsFrom(p); err == nil {
			t.Error("error を期待したが nil")
		}
	})
}

// token の 3 口(#354 と同じ思想)。参照先が空なら平文へ落ちずエラー。
func TestResolveToken(t *testing.T) {
	t.Run("token_file 優先", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "tok")
		if err := os.WriteFile(f, []byte("  file-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		h := hostEntry{Token: "literal", TokenEnv: "SASHIKI_TEST_TOKEN", TokenFile: f}
		got, err := h.resolveToken()
		if err != nil || got != "file-token" {
			t.Errorf("got %q, %v", got, err)
		}
	})

	t.Run("token_env は環境変数の名前", func(t *testing.T) {
		t.Setenv("SASHIKI_TEST_TOKEN", "env-token")
		h := hostEntry{Token: "literal", TokenEnv: "SASHIKI_TEST_TOKEN"}
		got, err := h.resolveToken()
		if err != nil || got != "env-token" {
			t.Errorf("got %q, %v", got, err)
		}
	})

	t.Run("token_env の参照先が空ならエラー(平文へ落ちない)", func(t *testing.T) {
		t.Setenv("SASHIKI_TEST_TOKEN", "")
		h := hostEntry{Token: "literal", TokenEnv: "SASHIKI_TEST_TOKEN"}
		if _, err := h.resolveToken(); err == nil {
			t.Error("error を期待したが nil")
		}
	})

	t.Run("token_file が読めなければエラー", func(t *testing.T) {
		h := hostEntry{TokenFile: filepath.Join(t.TempDir(), "nope")}
		if _, err := h.resolveToken(); err == nil {
			t.Error("error を期待したが nil")
		}
	})

	t.Run("どれも無ければ空(loopback 無認証などに任せる)", func(t *testing.T) {
		got, err := (hostEntry{}).resolveToken()
		if err != nil || got != "" {
			t.Errorf("got %q, %v", got, err)
		}
	})
}

// --host はグローバルフラグとして各コマンドの前に取り除かれる(#386)。
func TestExtractHostFlag(t *testing.T) {
	rest, host, err := extractHostFlag([]string{"list", "--host", "dev", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if host != "dev" {
		t.Errorf("host = %q", host)
	}
	if strings.Join(rest, " ") != "list --json" {
		t.Errorf("rest = %v", rest)
	}

	if _, _, err := extractHostFlag([]string{"list", "--host"}); err == nil {
		t.Error("値なしの --host はエラー")
	}

	rest, host, err = extractHostFlag([]string{"list"})
	if err != nil || host != "" || len(rest) != 1 {
		t.Errorf("フラグ無しで変化しないこと: %v %q %v", rest, host, err)
	}
}
