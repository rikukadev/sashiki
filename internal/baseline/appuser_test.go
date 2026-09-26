package baseline

import (
	"context"
	"strings"
	"testing"
)

// app ユーザーの同期は「無ければ作る / あれば ALTER でパスワード更新」を 1 回の起動で行う(#355)。
func TestSyncAppUserAltersPassword(t *testing.T) {
	m := &mockOps{}
	ops := m.ops()
	ops.Dialect = MySQLDialect()
	if err := SyncAppUser(context.Background(), Server{}, ops, "dev", "n3w'p"); err != nil {
		t.Fatal(err)
	}
	if !m.started || !m.shutdown || !m.waitGone {
		t.Errorf("should start and gracefully stop the base: started=%v shutdown=%v gone=%v", m.started, m.shutdown, m.waitGone)
	}
	joined := strings.Join(m.queries, "\n")
	for _, want := range []string{"SELECT @@version", "CREATE USER IF NOT EXISTS 'dev'@'%'", "ALTER USER 'dev'@'%' IDENTIFIED WITH", `BY 'n3w\'p'`} {
		if !strings.Contains(joined, want) {
			t.Errorf("queries should contain %q:\n%s", want, joined)
		}
	}
}

// user / pass が空なら何も起動しない。
func TestSyncAppUserNoop(t *testing.T) {
	m := &mockOps{}
	if err := SyncAppUser(context.Background(), Server{}, m.ops(), "", ""); err != nil {
		t.Fatal(err)
	}
	if m.started {
		t.Error("nothing to sync: base must not be started")
	}
}

// ApplyDirSync はマイグレーション適用と同じ起動の中で同期する(起動は 1 回)。
func TestApplyDirSyncSingleStart(t *testing.T) {
	dir := t.TempDir()
	writeSQL(t, dir, "0001_a.sql", "SELECT 1;")
	m := &mockOps{}
	ops := m.ops()
	ops.Dialect = MySQLDialect()
	applied, err := ApplyDirSync(context.Background(), Server{}, ops, dir, "app", "dev", "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || len(m.appliedFiles) != 1 {
		t.Errorf("applied = %v", applied)
	}
	if !strings.Contains(strings.Join(m.queries, "\n"), "ALTER USER 'dev'@'%'") {
		t.Error("app user should be synced in the same run")
	}
}

func TestAppUserSQLDialects(t *testing.T) {
	if got := mysqlAppUserSQL("dev", "p", "5.7.44-log"); !strings.Contains(got, "mysql_native_password") {
		t.Errorf("5.7 should use native: %s", got)
	}
	if got := mysqlAppUserSQL("dev", "p", "26.7.0"); !strings.Contains(got, "caching_sha2_password") {
		t.Errorf("26.7 should use caching_sha2: %s", got)
	}
	if got := postgresAppUserSQL("de\"v", "p'w", ""); !strings.Contains(got, `ALTER ROLE "de""v" WITH LOGIN SUPERUSER PASSWORD 'p''w'`) || !strings.Contains(got, `CREATE ROLE "de""v"`) {
		t.Errorf("postgres SQL should quote identifier and literal: %s", got)
	}
}
