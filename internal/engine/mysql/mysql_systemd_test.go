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

// buffer_pool_size は MYSQLD_DEFAULTS 経由で systemd の mysqld に届く(#299)。
func TestSystemdStartPassesBufferPool(t *testing.T) {
	e := New(Config{EnvDir: t.TempDir(), Mode: ModeSystemd, BufferPoolBytes: 512 * 1024 * 1024})
	e.run = func(context.Context, string, ...string) (string, error) { return "", nil }
	ins := engine.Instance{Branch: "pr-1", Port: 3401, DataDir: "/data/pr-1"}
	if err := e.Start(context.Background(), ins); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(e.envPath(ins.Branch))
	if !strings.Contains(string(b), "--innodb-buffer-pool-size=536870912") {
		t.Fatalf("env should carry the buffer pool size, got %q", b)
	}
}

// unit に固定値が残っていると、それが MYSQLD_DEFAULTS より後ろに来て勝つ。
func TestUnitDoesNotHardcodeBufferPool(t *testing.T) {
	for _, p := range []string{"../../../deploy/systemd/mysqld@.service", "../../../cmd/sashiki/assets/mysqld@.service"} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "--innodb-buffer-pool-size") {
				t.Errorf("%s must not hardcode innodb-buffer-pool-size: %q", p, line)
			}
		}
	}
}
