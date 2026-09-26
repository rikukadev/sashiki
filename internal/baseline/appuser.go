package baseline

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SyncAppUser は base を一時起動して app ユーザー(mysql の app_user / postgres の
// app_user ロール)を config の app_pass に合わせる(無ければ作り、あれば
// パスワードを更新する)。baseline は app ユーザーごと snapshot に入るので、
// app_pass を変えたら新しい baseline を取り直す必要があり、その経路がこれ(#355)。
// SQL ファイルの適用と同じ一時起動の中で行うため、ApplyDirSync から呼ぶ。
func SyncAppUser(ctx context.Context, s Server, ops Ops, user, pass string) error {
	_, err := applyAndSync(ctx, s, ops, "", "", user, pass)
	return err
}

// ApplyDirSync は ApplyDir に加えて app ユーザーを同期する。dir が空なら
// マイグレーションは適用せず同期だけ行う(`baseline refresh --app-user-only`)。
func ApplyDirSync(ctx context.Context, s Server, ops Ops, dir, db, user, pass string) ([]string, error) {
	return applyAndSync(ctx, s, ops, dir, db, user, pass)
}

// appUserSQL は方言ごとの「無ければ作る / あればパスワードを更新する」SQL。
// version は MySQL の認証プラグイン選択に使う(8.0+ は caching_sha2、5.7 と
// MariaDB は native。cmd/sashiki の pluginForVersion と同じ判定)。
func (d Dialect) appUserSQL(user, pass, version string) string {
	if d.AppUserSQL != nil {
		return d.AppUserSQL(user, pass, version)
	}
	return mysqlAppUserSQL(user, pass, version)
}

func mysqlAppUserSQL(user, pass, version string) string {
	plugin := "caching_sha2_password"
	if strings.Contains(version, "MariaDB") {
		plugin = "mysql_native_password"
	} else if dot := strings.IndexByte(version, '.'); dot > 0 {
		if major, err := strconv.Atoi(version[:dot]); err == nil && major < 8 {
			plugin = "mysql_native_password"
		}
	}
	u, p := MySQLLiteral(user), MySQLLiteral(pass)
	return fmt.Sprintf(
		"CREATE USER IF NOT EXISTS %s@'%%' IDENTIFIED WITH %s BY %s; "+
			"ALTER USER %s@'%%' IDENTIFIED WITH %s BY %s; "+
			"GRANT ALL PRIVILEGES ON *.* TO %s@'%%'; FLUSH PRIVILEGES;",
		u, plugin, p, u, plugin, p, u)
}

func postgresAppUserSQL(user, pass, _ string) string {
	// psql -c は複数文を受けるが、存在判定は DO ブロックで 1 文にする。
	return fmt.Sprintf(
		"DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN "+
			"ALTER ROLE %s WITH LOGIN SUPERUSER PASSWORD %s; ELSE "+
			"CREATE ROLE %s LOGIN SUPERUSER PASSWORD %s; END IF; END $$;",
		PGLiteral(user), PGIdent(user), PGLiteral(pass), PGIdent(user), PGLiteral(pass))
}

// MySQLLiteral は MySQL の文字列リテラル(\ と ' をエスケープ)。
func MySQLLiteral(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\x00", `\0`, "\n", `\n`, "\r", `\r`)
	return "'" + r.Replace(s) + "'"
}

// PGIdent / PGLiteral は PostgreSQL の識別子 / 文字列リテラル。
func PGIdent(s string) string   { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func PGLiteral(s string) string { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }

// applyAndSync は一時起動 → (dir があれば)マイグレーション適用 → (user があれば)
// app ユーザー同期 → 正常終了、を 1 回の起動で行う。
func applyAndSync(ctx context.Context, s Server, ops Ops, dir, db, user, pass string) ([]string, error) {
	files, err := migrationFiles(dir)
	if err != nil {
		return nil, err
	}
	sync := user != "" && pass != ""
	if len(files) == 0 && !sync {
		return nil, nil
	}
	dia := ops.Dialect
	if dia.zero() {
		dia = MySQLDialect()
	}
	if err := ops.Start(ctx, s); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	shutdown := func() error {
		if err := ops.Shutdown(ctx, s); err != nil {
			return err
		}
		return ops.WaitGone(ctx, s, 60*time.Second)
	}
	fail := func(cause error) ([]string, error) {
		_ = shutdown()
		return nil, cause
	}
	if err := ops.WaitReady(ctx, s, 60*time.Second); err != nil {
		return fail(err)
	}
	applied, err := applyMigrations(ctx, s, ops, dia, files, db)
	if err != nil {
		return fail(err)
	}
	if sync {
		version := ""
		if dia.ServerVersion != "" {
			version, _ = ops.Query(ctx, s, dia.ServerVersion)
		}
		if _, err := ops.Query(ctx, s, dia.appUserSQL(user, pass, strings.TrimSpace(version))); err != nil {
			return fail(fmt.Errorf("app user %s: %w", user, err))
		}
	}
	if err := shutdown(); err != nil {
		return applied, fmt.Errorf("graceful shutdown: %w", err)
	}
	return applied, nil
}
