package secretref

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 優先順位は ssm > env > file > literal(#354)。ここが崩れると、
// 「SSM に置き換えたのに古い env の値で動く」のような気づきにくい事故になる。
func TestResolvePriority(t *testing.T) {
	dir := t.TempDir()
	passFile := filepath.Join(dir, "p")
	if err := os.WriteFile(passFile, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SASHIKI_TEST_PASS", "from-env")

	orig := getParameter
	t.Cleanup(func() { getParameter = orig })
	getParameter = func(context.Context, string) (string, error) { return "from-ssm", nil }

	full := Ref{SSM: "/p", Env: "SASHIKI_TEST_PASS", File: passFile, Literal: "from-literal"}
	for _, tc := range []struct {
		name string
		ref  Ref
		want string
	}{
		{"全部あれば ssm", full, "from-ssm"},
		{"ssm 無しなら env", Ref{Env: full.Env, File: full.File, Literal: full.Literal}, "from-env"},
		{"ssm/env 無しなら file", Ref{File: full.File, Literal: full.Literal}, "from-file"},
		{"literal だけ", Ref{Literal: full.Literal}, "from-literal"},
	} {
		got, err := tc.ref.Resolve(context.Background(), "app_pass")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// ファイルの末尾改行は値に含めない。含めると認証が通らず、値を並べて比べられない
// ので原因が遠い。
func TestFileTrimsTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	for _, content := range []string{"secret", "secret\n", "secret\r\n", "secret\n\n"} {
		f := filepath.Join(dir, "p")
		if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := Ref{File: f}.Resolve(context.Background(), "app_pass")
		if err != nil {
			t.Fatal(err)
		}
		if got != "secret" {
			t.Errorf("content %q: got %q, want %q", content, got, "secret")
		}
	}
}

// 参照先が空なら **literal に落ちない**。明示した参照が壊れているときに既定値で
// 動き出すと、間違ったパスワードのまま起動して原因が分からなくなる。
func TestBrokenRefDoesNotFallBack(t *testing.T) {
	t.Setenv("SASHIKI_TEST_EMPTY", "")
	if _, err := (Ref{Env: "SASHIKI_TEST_EMPTY", Literal: "fallback"}).Resolve(context.Background(), "app_pass"); err == nil {
		t.Error("空の env で literal に落ちた")
	}
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := (Ref{File: missing, Literal: "fallback"}).Resolve(context.Background(), "app_pass"); err == nil {
		t.Error("読めない file で literal に落ちた")
	}
}

// 未設定のエラーは **選択肢を並べる**。「app_pass が未設定」だけだと、
// 平文で書くしかないと読めてしまう。
func TestUnsetListsAllOptions(t *testing.T) {
	msg := Unset("engine.mysql.app_pass").Error()
	for _, want := range []string{
		"engine.mysql.app_pass_ssm",
		"engine.mysql.app_pass_env",
		"engine.mysql.app_pass_file",
		"engine.mysql.app_pass:",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("エラーに %q が出ていない:\n%s", want, msg)
		}
	}
}

// 値そのものはエラーに出さない。エラーはログや issue に貼られる。
func TestErrorsDoNotLeakValues(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "p")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Ref{File: f}).Resolve(context.Background(), "app_pass"); err == nil {
		t.Fatal("空ファイルがエラーにならない")
	}
	t.Setenv("SASHIKI_TEST_SECRET", "s3cret")
	got, err := (Ref{Env: "SASHIKI_TEST_SECRET"}).Resolve(context.Background(), "app_pass")
	if err != nil || got != "s3cret" {
		t.Fatalf("resolve: %q %v", got, err)
	}
	// 名前は出すが値は出さない、が守れているかを空のときで見る
	t.Setenv("SASHIKI_TEST_SECRET", "")
	_, err = (Ref{Env: "SASHIKI_TEST_SECRET"}).Resolve(context.Background(), "app_pass")
	if err == nil {
		t.Fatal("空 env がエラーにならない")
	}
	if !strings.Contains(err.Error(), "SASHIKI_TEST_SECRET") {
		t.Errorf("どの変数か分からないエラー: %v", err)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("エラーに値が漏れている: %v", err)
	}
}
