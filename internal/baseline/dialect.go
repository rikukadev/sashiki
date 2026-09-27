package baseline

import "fmt"

// Dialect は組み込みローダー(ApplyDir)が使う SQL の方言差を吸収する。
// 適用記録の置き場は MySQL では専用データベース、PostgreSQL では対象 DB 内の
// スキーマになる(PostgreSQL は接続中の DB を跨いだ問い合わせができないため)。
type Dialect struct {
	// CreateMeta は適用記録の置き場(DB / スキーマ)を作る。
	CreateMeta string
	// CreateMigrations は _migrations テーブルを作る。
	CreateMigrations string
	// CountMigration は適用済みかを数える SQL を返す(結果は "0" / "1")。
	CountMigration func(name string) string
	// InsertMigration は適用記録を書く SQL を返す。
	InsertMigration func(name string) string
	// ServerVersion はサーバの版を返す SQL(app ユーザーの認証プラグイン選択用)。
	ServerVersion string
	// AppUserSQL は app ユーザーを「無ければ作る / あればパスワード更新」する SQL(#355)。
	// nil なら MySQL 用。
	AppUserSQL func(user, pass, version string) string
}

// MySQLDialect は従来どおり sashiki_meta データベースに記録する。
func MySQLDialect() Dialect {
	return Dialect{
		CreateMeta: "CREATE DATABASE IF NOT EXISTS `" + metaDB + "`",
		CreateMigrations: "CREATE TABLE IF NOT EXISTS `" + metaDB +
			"`._migrations (name VARCHAR(255) PRIMARY KEY, applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)",
		CountMigration: func(name string) string {
			return fmt.Sprintf("SELECT COUNT(*) FROM `%s`._migrations WHERE name = '%s'", metaDB, name)
		},
		InsertMigration: func(name string) string {
			return fmt.Sprintf("INSERT INTO `%s`._migrations (name) VALUES ('%s')", metaDB, name)
		},
		ServerVersion: "SELECT @@version",
		AppUserSQL:    mysqlAppUserSQL,
	}
}

// PostgresDialect は対象 DB 内の sashiki_meta スキーマに記録する。
func PostgresDialect() Dialect {
	return Dialect{
		CreateMeta: "CREATE SCHEMA IF NOT EXISTS " + metaDB,
		CreateMigrations: "CREATE TABLE IF NOT EXISTS " + metaDB +
			"._migrations (name text PRIMARY KEY, applied_at timestamptz DEFAULT now())",
		CountMigration: func(name string) string {
			return fmt.Sprintf("SELECT COUNT(*) FROM %s._migrations WHERE name = '%s'", metaDB, name)
		},
		InsertMigration: func(name string) string {
			return fmt.Sprintf("INSERT INTO %s._migrations (name) VALUES ('%s')", metaDB, name)
		},
		ServerVersion: "SHOW server_version",
		AppUserSQL:    postgresAppUserSQL,
	}
}

// zero は Dialect が未設定(ゼロ値)かを返す。未設定なら MySQL 方言に倒す
// (既存の呼び出し・テストが Ops を手組みしていても壊さないため)。
func (d Dialect) zero() bool { return d.CreateMeta == "" }
