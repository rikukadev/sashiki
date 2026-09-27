// Package baseline は base datadir 上の一時 mysqld の操作を提供する。
// sashiki baseline import(初回構築)と baseline refresh の組み込みローダー
// (更新)が同じ載せ替え手順を共有する:
//
//	一時 mysqld を起動(--skip-networking)→ SQL を適用 → graceful shutdown
//	→ pid 消滅確認
//
// 最重要不変条件「@init/@baseline snapshot は必ず mysqld の正常終了状態で
// のみ取得する」のうち「正常終了させる」部分をこのパッケージが担う。
// snapshot の取得そのものは呼び出し側の責務(取得前に quiesce 検証を行う)。
package baseline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Server は一時 mysqld 1 本の識別情報。
type Server struct {
	DataDir  string // 既存(または初期化する)datadir
	Socket   string // 専用 UNIX ソケット(--skip-networking なので接続はこれのみ)
	PidFile  string // 停止確認(pid 消滅待ち)に使う
	LogError string // mysqld の --log-error
	// UID/GID: mysqld を実行する uid/gid(root から mysql ユーザーへ降格)。
	// 両方 0 なら降格しない(呼び出し元プロセスのユーザーで実行)。
	UID, GID uint32
	// MysqldBin は使う mysqld のパス(空なら /usr/sbin/mysqld)。macOS では
	// Homebrew mysql@8.0 等を指定する(#127)。
	MysqldBin string
	// ExtraCnf を指定すると初期化/投入時の mysqld が --defaults-file でその 1
	// ファイルだけを読む。投入時の sql_mode / strict を実行時と揃えられる
	// (未指定なら従来どおり既定の my.cnf を読む、#127)。
	ExtraCnf string

	// 以下は postgres エンジンでのみ使う(#226)。MySQL 経路では無視される。
	// PgBinDir は initdb / psql / pg_ctl のあるディレクトリ。
	PgBinDir string
	// PgPort は一時クラスタのポート。TCP は開かないので socket 名にだけ使う。
	PgPort int
	// PgDB は SQL を流す既定のデータベース(source_db 相当)。
	PgDB string
}

// mysqld は使う mysqld バイナリを返す(既定 /usr/sbin/mysqld)。
func (s Server) mysqld() string {
	if s.MysqldBin != "" {
		return s.MysqldBin
	}
	return "/usr/sbin/mysqld"
}

// defaultsArgs は mysqld の先頭に置く defaults 引数(空スライス可)。
func (s Server) defaultsArgs() []string {
	if s.ExtraCnf != "" {
		return []string{"--defaults-file=" + s.ExtraCnf}
	}
	return nil
}

// client は mysql / mysqladmin クライアントのパスを返す。MysqldBin が絶対パスなら
// 同じディレクトリのものを使う(8.0 サーバを 9.x クライアントで叩く事故を避ける)。
func (s Server) client(name string) string {
	if strings.Contains(s.MysqldBin, "/") {
		p := filepath.Join(filepath.Dir(s.MysqldBin), name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return name
}

// Ops は mysqld / mysql / mysqladmin の実行一式。テストでは各関数を
// 差し替えてモックする(実コマンドは RealOps)。
type Ops struct {
	// Initialize は datadir を初期化する(mysqld --initialize-insecure)。
	Initialize func(ctx context.Context, s Server) error
	// Start は一時 mysqld をデーモンとして起動する。
	Start func(ctx context.Context, s Server) error
	// WaitReady はソケット越しに ping が通るまで待つ。
	WaitReady func(ctx context.Context, s Server, timeout time.Duration) error
	// Query は SQL を1文実行して出力(タブ区切り・ヘッダなし)を返す。
	Query func(ctx context.Context, s Server, sql string) (string, error)
	// ApplyFile は SQL ファイルを標準入力から流し込む(db が空なら DB 未選択で実行)。
	ApplyFile func(ctx context.Context, s Server, db, path string) error
	// Shutdown は graceful shutdown を要求する(mysqladmin shutdown)。
	Shutdown func(ctx context.Context, s Server) error
	// WaitGone は pid ファイルの消滅を待つ(= mysqld の停止確認)。
	WaitGone func(ctx context.Context, s Server, timeout time.Duration) error
	// Dialect は適用記録の SQL 方言(未設定なら MySQL 方言)。
	Dialect Dialect
}

// RealOps は実コマンドを実行する Ops(MySQL)。
func RealOps() Ops {
	return Ops{
		Dialect: MySQLDialect(),
		Initialize: func(ctx context.Context, s Server) error {
			args := append(s.defaultsArgs(), "--initialize-insecure",
				"--datadir="+s.DataDir, "--log-error="+s.LogError)
			return runAs(ctx, s, s.mysqld(), args...)
		},
		Start: func(ctx context.Context, s Server) error {
			args := append(s.defaultsArgs(),
				"--datadir="+s.DataDir, "--port=0", "--skip-networking",
				"--socket="+s.Socket, "--pid-file="+s.PidFile,
				"--log-error="+s.LogError, "--daemonize")
			return runAs(ctx, s, s.mysqld(), args...)
		},
		WaitReady: func(ctx context.Context, s Server, timeout time.Duration) error {
			deadline := time.Now().Add(timeout)
			for time.Now().Before(deadline) {
				if err := ctx.Err(); err != nil {
					return err
				}
				if exec.CommandContext(ctx, s.client("mysqladmin"), "-uroot", "-S", s.Socket, "ping").Run() == nil {
					return nil
				}
				time.Sleep(500 * time.Millisecond)
			}
			return fmt.Errorf("mysqld が %s 以内に ready になりませんでした", timeout)
		},
		Query: func(ctx context.Context, s Server, sql string) (string, error) {
			cmd := exec.CommandContext(ctx, s.client("mysql"), "-uroot", "-S", s.Socket, "-N", "-B", "-e", sql)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
			}
			return strings.TrimSpace(string(out)), nil
		},
		ApplyFile: func(ctx context.Context, s Server, db, path string) error {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			// migration/seed の投入も import と同じくバルクロード扱いにする(#194 publish 経路)。
			// baseline build 中の一時 mysqld なので制約チェック/バイナリログは不要で、
			// 大きな migration では体感が変わる。セッション限定なので runtime には残らない。
			args := []string{"-uroot", "-S", s.Socket,
				"--init-command=SET unique_checks=0, foreign_key_checks=0, sql_log_bin=0"}
			if db != "" {
				args = append(args, db)
			}
			cmd := exec.CommandContext(ctx, s.client("mysql"), args...)
			cmd.Stdin = f
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		},
		Shutdown: func(ctx context.Context, s Server) error {
			if out, err := exec.CommandContext(ctx, s.client("mysqladmin"), "-uroot", "-S", s.Socket, "shutdown").CombinedOutput(); err != nil {
				return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		},
		WaitGone: func(ctx context.Context, s Server, timeout time.Duration) error {
			deadline := time.Now().Add(timeout)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(s.PidFile); os.IsNotExist(err) {
					return nil
				}
				time.Sleep(200 * time.Millisecond)
			}
			return fmt.Errorf("mysqld の停止を確認できませんでした (%s が残っています)", s.PidFile)
		},
	}
}

// runAs は Server の UID/GID(0,0 以外なら降格)でコマンドを実行する。
func runAs(ctx context.Context, s Server, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if s.UID != 0 || s.GID != 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: s.UID, Gid: s.GID}}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// LookupOSUser は任意の OS ユーザーの uid/gid を返す(root からの降格用)。
func LookupOSUser(name string) (uint32, uint32, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, fmt.Errorf("%s ユーザーが見つかりません: %w", name, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return uint32(uid), uint32(gid), nil
}

// LookupMysqlUser は mysql ユーザーの uid/gid を返す(root からの降格用)。
func LookupMysqlUser() (uint32, uint32, error) {
	u, err := user.Lookup("mysql")
	if err != nil {
		return 0, 0, fmt.Errorf("mysql ユーザーが見つかりません(mysql-server はインストール済み?): %w", err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return uint32(uid), uint32(gid), nil
}

// migNameRe は _migrations に記録するファイル名の制約(SQL 埋め込み対策)。
var migNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// metaDB は適用記録を置く sashiki 専用データベース。アプリのスキーマから独立
// させることで、任意のダンプ/スキーマ構成でも冪等化できる。
const metaDB = "sashiki_meta"

// ApplyDir は既存の base datadir に対して dir/*.sql を名前順に適用する
// 組み込みローダー(baseline refresh の source_dir モード)。適用記録は
// sashiki_meta._migrations(name PK)で冪等化する。db が非空なら各ファイルを
// その DB を選択した状態で実行する(USE を含まないマイグレーション向け)。
// 成否にかかわらず mysqld は必ず graceful shutdown + pid 消滅確認まで行う
// (@baseline snapshot は正常終了状態でのみ取得する、の「正常終了」部分)。
// 適用対象が 1 件も無い場合は mysqld を起動せずに成功する。
func ApplyDir(ctx context.Context, s Server, ops Ops, dir, db string) ([]string, error) {
	return applyAndSync(ctx, s, ops, dir, db, "", "")
}

// migrationFiles は dir の *.sql を名前順に返す(dir が空なら無し)。
func migrationFiles(dir string) ([]string, error) {
	if dir == "" {
		return nil, nil
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	for _, f := range files {
		if !migNameRe.MatchString(filepath.Base(f)) {
			return nil, fmt.Errorf("マイグレーション名に使えない文字が含まれています: %s", filepath.Base(f))
		}
	}
	return files, nil
}

// applyMigrations は起動済みの base に未適用のファイルを順に適用する(適用記録付き)。
func applyMigrations(ctx context.Context, s Server, ops Ops, dia Dialect, files []string, db string) ([]string, error) {
	if len(files) == 0 {
		return nil, nil
	}
	if _, err := ops.Query(ctx, s, dia.CreateMeta); err != nil {
		return nil, err
	}
	if _, err := ops.Query(ctx, s, dia.CreateMigrations); err != nil {
		return nil, err
	}
	var applied []string
	for _, f := range files {
		name := filepath.Base(f)
		got, err := ops.Query(ctx, s, dia.CountMigration(name))
		if err != nil {
			return nil, err
		}
		if got == "1" {
			continue
		}
		if err := ops.ApplyFile(ctx, s, db, f); err != nil {
			return nil, fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := ops.Query(ctx, s, dia.InsertMigration(name)); err != nil {
			return nil, err
		}
		applied = append(applied, name)
	}
	return applied, nil
}
