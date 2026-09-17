package postgres

import (
	"strings"
	"testing"

	"github.com/rikukadev/sashiki/internal/engine"
)

// ConnCount は app ロールで繋ぎ、パスワードは argv でなく PGPASSWORD で渡す(#291)。
// 以前は `-U postgres -w` で、scram を要求する initdb 既定では常に認証失敗し、
// 「判定不能=使用中」の保護で idle 停止が一度も発火しなかった。
func TestConnCountUsesAppRoleAndEnvPassword(t *testing.T) {
	e := New(Config{BinDir: "/opt/pg/bin", AppUser: "dev", AppPass: "s3cret"})
	args := e.connCountArgs(engine.Instance{Branch: "pr-1", Port: 5433})
	joined := strings.Join(args, " ")
	if args[0] != "/opt/pg/bin/psql" {
		t.Errorf("psql path = %q", args[0])
	}
	if !strings.Contains(joined, "-U dev") {
		t.Errorf("should connect as the app role, got: %s", joined)
	}
	if strings.Contains(joined, "-U postgres") {
		t.Errorf("must not rely on a passwordless postgres role: %s", joined)
	}
	if strings.Contains(joined, "s3cret") {
		t.Errorf("password must not appear on the command line: %s", joined)
	}
	found := false
	for _, kv := range e.clientEnv() {
		if kv == "PGPASSWORD=s3cret" {
			found = true
		}
	}
	if !found {
		t.Error("PGPASSWORD should carry the app password")
	}
}

func TestConnCountDefaultsAppUser(t *testing.T) {
	e := New(Config{})
	if e.cfg.AppUser != "dev" {
		t.Errorf("default AppUser = %q, want dev", e.cfg.AppUser)
	}
}
