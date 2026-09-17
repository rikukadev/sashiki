package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hook は sashikid の環境を継承するが、API トークンは見せない(#295)。
func TestRunStripsSecretsFromInheritedEnv(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "on-create",
		"#!/bin/sh\necho \"token=${SASHIKI_API_TOKEN-unset} custom=${MY_TOKEN-unset} path=${PATH:+set}\"\n")
	t.Setenv("SASHIKI_API_TOKEN", "sashiki_secret")
	t.Setenv("MY_TOKEN", "also_secret")

	r := NewRunner(dir, t.TempDir(), time.Minute)
	r.StripEnv = append(r.StripEnv, "MY_TOKEN")
	res, err := r.Run(context.Background(), OnCreate, Env{Branch: "pr-1"})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(res.LogPath)
	got := strings.TrimSpace(string(out))
	if got != "token=unset custom=unset path=set" {
		t.Errorf("hook env = %q; secrets must be stripped, PATH kept", got)
	}
}

// LogRetention より古い hook ログは次の実行で消える。
func TestRunPrunesOldLogs(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "on-create", "#!/bin/sh\nexit 0\n")
	logDir := t.TempDir()
	old := filepath.Join(logDir, "pr-0-on-create-20200101T000000Z.log")
	if err := os.WriteFile(old, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(logDir, "notes.txt") // .log 以外は触らない
	_ = os.WriteFile(keep, []byte("x"), 0o644)
	_ = os.Chtimes(keep, past, past)

	r := NewRunner(dir, logDir, time.Minute)
	r.LogRetention = 24 * time.Hour
	if _, err := r.Run(context.Background(), OnCreate, Env{Branch: "pr-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("log older than retention should be removed")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("non-.log files must be left alone")
	}
}
