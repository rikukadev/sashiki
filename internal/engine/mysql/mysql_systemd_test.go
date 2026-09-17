package mysql

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/rikukadev/sashiki/internal/engine"
)

// 古い mysqld@.service も展開する MYSQLD_DEFAULTS に bind-address を入れ、
// init 再実行前の既存ホストでも次回 Start から直結ポートを閉じる(#288)。
func TestSystemdStartWritesLoopbackBindOption(t *testing.T) {
	e := New(Config{EnvDir: t.TempDir(), Mode: ModeSystemd, ExtraCnf: "/etc/mysql/sashiki.cnf"})
	e.run = func(context.Context, string, ...string) (string, error) { return "", nil }
	ins := engine.Instance{Branch: "pr-1", Port: 3401, DataDir: "/data/pr-1"}
	if err := e.Start(context.Background(), ins); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(e.envPath(ins.Branch))
	if err != nil {
		t.Fatal(err)
	}
	env := string(b)
	want := "MYSQLD_DEFAULTS=--defaults-file=/etc/mysql/sashiki.cnf --bind-address=127.0.0.1"
	if !strings.Contains(env, want) {
		t.Fatalf("env = %q, want %q", env, want)
	}
}
