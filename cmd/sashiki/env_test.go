package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// fakeAPI は sashikid の代わり。CLI が「どの URL に何を送るか」まで含めて
// 確かめたいので、call() を差し替えず本物の HTTP を通す。
func fakeAPI(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("SASHIKI_API_URL", srv.URL)
}

// 標準出力を捕まえる。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	_ = w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b)
}

const branchJSON = `{"name":"pr-42","state":"running","port":3306,"engine_port":3401,` +
	`"host":"sashiki.internal","user":"dev@pr-42","engine":"mysql"}`

func TestCmdEnv(t *testing.T) {
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/branches/pr-42" {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(w, branchJSON)
	})

	t.Run("dotenv で出す", func(t *testing.T) {
		var code int
		out := captureStdout(t, func() { code = cmdEnv([]string{"pr-42"}) })
		if code != exitOK {
			t.Fatalf("exit = %d", code)
		}
		want := "DB_HOST=sashiki.internal\nDB_PORT=3306\nDB_USER=dev@pr-42\n"
		if out != want {
			t.Errorf("out =\n%q\nwant\n%q", out, want)
		}
	})

	// 出すのは **接続に使える 3 つ組**(#260)。engine_port は調査用なので
	// 混ぜない — 混ざると「どちらを使えばいいか」が読み手に判断できない。
	t.Run("engine_port は出さない", func(t *testing.T) {
		out := captureStdout(t, func() { _ = cmdEnv([]string{"pr-42"}) })
		if strings.Contains(out, "3401") {
			t.Errorf("内部ポートが混ざっている:\n%s", out)
		}
	})

	// パスワードは払い出し対象ではない。ここで出すと「表示しただけ」の
	// つもりの操作が CI のログに秘密を残す。
	t.Run("パスワードは出さない", func(t *testing.T) {
		out := captureStdout(t, func() { _ = cmdEnv([]string{"pr-42"}) })
		if strings.Contains(strings.ToUpper(out), "PASSWORD") {
			t.Errorf("パスワードらしき行がある:\n%s", out)
		}
	})

	t.Run("--prefix で接頭辞を変えられる", func(t *testing.T) {
		out := captureStdout(t, func() { _ = cmdEnv([]string{"pr-42", "--prefix", "SASHIKI_"}) })
		if !strings.Contains(out, "SASHIKI_HOST=sashiki.internal") {
			t.Errorf("prefix が効いていない:\n%s", out)
		}
		if strings.Contains(out, "DB_") {
			t.Errorf("既定の接頭辞が残っている:\n%s", out)
		}
	})
}

func TestCmdEnvNotFound(t *testing.T) {
	fakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"code":"branch_not_found","message":"no such branch"}`)
	})
	if code := cmdEnv([]string{"nope"}); code == exitOK {
		t.Error("存在しないブランチで成功してはいけない")
	}
}

// --exist-ok は API のクエリに落ちること。落ちないと 2 回目の create が
// 409 になり、hook 側が `|| true`(実エラーも握り潰す)に追い込まれる。
func TestCreateExistOk(t *testing.T) {
	var got url.Values
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		w.WriteHeader(http.StatusOK) // exist_ok の既存ヒット
		fmt.Fprint(w, branchJSON)
	})

	t.Run("付ければクエリに乗る", func(t *testing.T) {
		got = nil
		_ = captureStdout(t, func() { _ = cmdCreate([]string{"pr-42", "--exist-ok"}) })
		if got.Get("exist_ok") != "true" {
			t.Errorf("exist_ok=%q, want true", got.Get("exist_ok"))
		}
	})

	t.Run("付けなければ乗らない", func(t *testing.T) {
		got = nil
		_ = captureStdout(t, func() { _ = cmdCreate([]string{"pr-42"}) })
		if got.Has("exist_ok") {
			t.Errorf("指定していないのに exist_ok が付いている: %v", got)
		}
	})

	// 位置引数として食われると、ブランチ名が 2 つあることになり usage で落ちる。
	t.Run("フラグが名前として扱われない", func(t *testing.T) {
		got = nil
		code := exitError
		_ = captureStdout(t, func() { code = cmdCreate([]string{"pr-42", "--exist-ok"}) })
		if code != exitOK {
			t.Errorf("exit = %d(--exist-ok が位置引数になっている可能性)", code)
		}
	})
}
