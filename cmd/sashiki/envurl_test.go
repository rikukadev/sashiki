package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pgBranchJSON = `{"name":"pr-42","state":"running","port":5432,"engine_port":5401,` +
	`"host":"sashiki.internal","user":"dev@pr-42","engine":"postgres"}`

func TestCmdEnvURL(t *testing.T) {
	fakeAPI(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, branchJSON) })
	t.Setenv("SASHIKI_DB_PASSWORD", "s3cret")

	// 標準出力ではパスワードを出さない(cmdEnv の方針どおり)。SASHIKI_DB_PASSWORD が
	// あっても URL に入れない。
	t.Run("標準出力ではパスワード無しの URL", func(t *testing.T) {
		out := captureStdout(t, func() { _ = cmdEnv([]string{"pr-42", "--url-var", "DATABASE_URL", "--db", "app"}) })
		if !strings.Contains(out, "DATABASE_URL=mysql://dev%40pr-42@sashiki.internal:3306/app\n") {
			t.Errorf("URL が違う:\n%s", out)
		}
		if strings.Contains(out, "s3cret") {
			t.Errorf("パスワードが標準出力に出ている:\n%s", out)
		}
	})

	// ユーザー名の @ をエスケープしないと、ドライバがホストとの区切りと読み違える。
	t.Run("ユーザー名の @ は %40", func(t *testing.T) {
		out := captureStdout(t, func() { _ = cmdEnv([]string{"pr-42", "--url-var", "DATABASE_URL"}) })
		if strings.Contains(out, "dev@pr-42@") {
			t.Errorf("@ がエスケープされていない:\n%s", out)
		}
	})

	// DB_HOST と URL の host は同じ値にする。片方だけ上書きされると、同じ .env に
	// 繋がる値と繋がらない値が並ぶ。
	t.Run("SASHIKI_DB_HOST は DB_HOST と URL の両方に効く", func(t *testing.T) {
		t.Setenv("SASHIKI_DB_HOST", "127.0.0.1")
		out := captureStdout(t, func() { _ = cmdEnv([]string{"pr-42", "--url-var", "DATABASE_URL"}) })
		if !strings.Contains(out, "DB_HOST=127.0.0.1\n") {
			t.Errorf("DB_HOST が上書きされていない:\n%s", out)
		}
		if !strings.Contains(out, "@127.0.0.1:3306") {
			t.Errorf("URL の host が上書きされていない:\n%s", out)
		}
	})

	t.Run("--url-var が無ければ従来どおり 3 行", func(t *testing.T) {
		out := captureStdout(t, func() { _ = cmdEnv([]string{"pr-42"}) })
		if out != "DB_HOST=sashiki.internal\nDB_PORT=3306\nDB_USER=dev@pr-42\n" {
			t.Errorf("出力が変わっている:\n%q", out)
		}
	})
}

func TestCmdEnvURLPostgres(t *testing.T) {
	fakeAPI(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, pgBranchJSON) })
	t.Setenv("SASHIKI_DB_NAME", "")
	// DB 名の指定が無ければ、必ず存在する postgres に繋ぐ(connect と同じ)。
	out := captureStdout(t, func() { _ = cmdEnv([]string{"pr-42", "--url-var", "DATABASE_URL"}) })
	if !strings.Contains(out, "DATABASE_URL=postgres://dev%40pr-42@sashiki.internal:5432/postgres\n") {
		t.Errorf("postgres の URL が違う:\n%s", out)
	}
}

func TestCmdEnvWrite(t *testing.T) {
	fakeAPI(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, branchJSON) })
	t.Setenv("SASHIKI_DB_PASSWORD", "s3cret")
	t.Setenv("SASHIKI_DB_HOST", "127.0.0.1")

	t.Run("既存の行を残し、同じキーだけ置き換える", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), ".env.local")
		orig := "# 手で書いた設定\nAPI_KEY=abc\nDATABASE_URL=mysql://old\nDB_PORT=1\n"
		if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
			t.Fatal(err)
		}
		var code int
		out := captureStdout(t, func() {
			code = cmdEnv([]string{"pr-42", "--url-var", "DATABASE_URL", "--db", "app", "--write", path})
		})
		if code != exitOK {
			t.Fatalf("exit = %d", code)
		}
		got, _ := os.ReadFile(path)
		want := "# 手で書いた設定\nAPI_KEY=abc\n" +
			"DATABASE_URL=mysql://dev%40pr-42:s3cret@127.0.0.1:3306/app\nDB_PORT=3306\n" +
			"DB_HOST=127.0.0.1\nDB_USER=dev@pr-42\n"
		if string(got) != want {
			t.Errorf("ファイル =\n%s\nwant\n%s", got, want)
		}
		// 標準出力には値を出さない(パスワードを含むため)。
		if strings.Contains(out, "s3cret") || strings.Contains(out, "mysql://") {
			t.Errorf("標準出力に値が出ている:\n%s", out)
		}
		// 既存ファイルのパーミッションは変えない。
		if st, _ := os.Stat(path); st.Mode().Perm() != 0o644 {
			t.Errorf("mode = %v, want 0644", st.Mode().Perm())
		}
	})

	t.Run("新しく作るときは 0600", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), ".env.local")
		_ = captureStdout(t, func() { _ = cmdEnv([]string{"pr-42", "--url-var", "DATABASE_URL", "--write", path}) })
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, want 0600", st.Mode().Perm())
		}
		got, _ := os.ReadFile(path)
		if strings.HasPrefix(string(got), "\n") {
			t.Errorf("先頭に空行がある:\n%q", got)
		}
	})

	t.Run("パスワードを渡さなければ URL にも入らない", func(t *testing.T) {
		t.Setenv("SASHIKI_DB_PASSWORD", "")
		path := filepath.Join(t.TempDir(), ".env.local")
		_ = captureStdout(t, func() { _ = cmdEnv([]string{"pr-42", "--url-var", "DATABASE_URL", "--write", path}) })
		got, _ := os.ReadFile(path)
		if !strings.Contains(string(got), "DATABASE_URL=mysql://dev%40pr-42@127.0.0.1:3306\n") {
			t.Errorf("URL が違う:\n%s", got)
		}
	})
}

func TestCmdEnvUnknownFlag(t *testing.T) {
	if code := cmdEnv([]string{"pr-42", "--url"}); code != exitUsage {
		t.Errorf("exit = %d, want %d", code, exitUsage)
	}
}
