// PostgreSQL の baseline import(#223)。MySQL 版(baseline.go の
// runBaselineImport)と同じ段取り —— 空の base データセットにクラスタを作り、
// ダンプを投入し、接続ロールを用意し、**正常終了してから** snapshot を取得する
// —— を postgres のツール(initdb / psql / pg_restore / pg_ctl)で行う。
//
// 衝突面を小さくするため baseline.go には手を入れず、ここに閉じている
// (baseline.go は MySQL 側の変更が頻繁なため)。
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/rikukadev/sashiki/internal/config"
	"github.com/rikukadev/sashiki/internal/localstore"
)

// baseline 構築用に一時起動するクラスタの設定。TCP を開かず(listen_addresses=”)
// unix socket だけで受けるので、稼働中のブランチとポートが衝突しない。
const (
	pgBaselinePort   = "5499" // socket 名 .s.PGSQL.5499 に使うだけで LISTEN しない
	pgBaselineSocket = "/tmp"
	// サーバ出力の逃がし先。データセット内に置くと snapshot に含まれて
	// 全ブランチへ複製されるので外に置く。
	pgBaselineLog = "/tmp/sashiki-baseline-pg.log"
)

func runPostgresBaselineImport(cfg config.Config, opts baselineImportOpts) error {
	// apfs / reflink はローカル CoW backend(zfs コマンドを使わない、#227)。
	if cfg.Storage.Backend == "apfs" || cfg.Storage.Backend == "reflink" {
		return runLocalPostgresBaselineImport(cfg, opts)
	}

	base := cfg.Storage.Zfs.BaseDataset
	snap := base + "@" + cfg.Storage.Zfs.BaselineSnapshot
	if err := exec.Command("zfs", "list", snap).Run(); err == nil {
		return fmt.Errorf("snapshot %s は既に存在します。取得し直しは baseline refresh で行ってください", snap)
	}

	mountOut, err := exec.Command("zfs", "get", "-H", "-o", "value", "mountpoint", base).Output()
	if err != nil {
		return fmt.Errorf("dataset %s が見つかりません。先に sashiki init を実行してください", base)
	}
	dataDir := filepath.Join(strings.TrimSpace(string(mountOut)), "data")
	if entries, err := os.ReadDir(dataDir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s が空ではありません。初期化済みの base に import はできません", dataDir)
	}

	runUser := cfg.EngineRunUser()
	if runUser == "" {
		runUser = "postgres"
	}
	uid, gid, err := lookupOSUser(runUser)
	if err != nil {
		return err
	}
	// postgres は datadir のパーミッションが 0700(または 0750)でないと起動を拒む。
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	if err := chownR(filepath.Dir(dataDir), uid, gid); err != nil {
		return err
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return err
	}

	binDir := cfg.Engine.Postgres.BinDir
	initdb := pgBin(binDir, "initdb")
	pgCtl := pgBin(binDir, "pg_ctl")
	psql := pgBin(binDir, "psql")
	pgRestore := pgBin(binDir, "pg_restore")

	// --- initdb ---
	// host 認証を scram-sha-256 にして、proxy → backend の TCP 接続でパスワードを
	// 検証させる(#222 の pgproxy は app_user のパスワードを持っている)。local は
	// trust にして、この import 中の psql をパスワード無しで通す。
	fmt.Println("→ initdb")
	initArgs := []string{"-D", dataDir, "--auth-host=scram-sha-256", "--auth-local=trust"}
	initArgs = append(initArgs, cfg.Engine.Postgres.InitdbArgs...)
	if err := runAsUser(uid, gid, initdb, initArgs...); err != nil {
		return fmt.Errorf("initdb: %w", err)
	}

	// --- 起動(TCP を開かない) ---
	fmt.Println("→ postgres 起動")
	startOpts := fmt.Sprintf("-p %s -c listen_addresses='' -c unix_socket_directories=%s",
		pgBaselinePort, pgBaselineSocket)
	// -l でサーバ出力をファイルへ逃がす。これが無いと postgres はデーモン化した
	// あとも親から継いだ stdout/stderr を握り続けるため、出力をパイプで捕まえる
	// 実行方法(CombinedOutput)だとパイプが閉じず永久にブロックする。
	if err := runAsUserNoPipe(uid, gid, pgCtl, "start", "-D", dataDir,
		"-o", startOpts, "-l", pgBaselineLog, "-w"); err != nil {
		return fmt.Errorf("pg_ctl start: %w (詳細は %s)", err, pgBaselineLog)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = runAsUser(uid, gid, pgCtl, "stop", "-D", dataDir, "-m", "immediate", "-w")
		}
	}()

	// --- ダンプ投入 ---
	db := opts.db
	if db == "" {
		db = "postgres"
	}
	if opts.db != "" {
		fmt.Printf("→ データベース %s 作成\n", opts.db)
		if err := pgSQL(uid, gid, psql, "postgres",
			fmt.Sprintf("CREATE DATABASE %s", quoteIdent(opts.db))); err != nil {
			return fmt.Errorf("create database %s: %w", opts.db, err)
		}
	}
	if opts.from != "" {
		from, cleanup, err := pgResolveDump(opts.from)
		if err != nil {
			return err
		}
		defer cleanup()
		if err := pgLoadDump(uid, gid, psql, pgRestore, from, db, opts.threads); err != nil {
			return err
		}
	}

	// --- 接続ロール ---
	appUser, appPass := cfg.AppUser(), cfg.AppPass()
	fmt.Printf("→ 接続ロール %s 作成\n", appUser)
	// パスワードは scram-sha-256 で保存される(PG14+ の既定 password_encryption)。
	// proxy は認証を終端したうえで、このロールとして backend に繋ぎ直す。
	roleSQL := fmt.Sprintf("CREATE ROLE %s LOGIN SUPERUSER PASSWORD %s",
		quoteIdent(appUser), quoteLiteral(appPass))
	if err := pgSQL(uid, gid, psql, "postgres", roleSQL); err != nil {
		return fmt.Errorf("create role %s: %w", appUser, err)
	}
	if opts.db != "" {
		if err := pgSQL(uid, gid, psql, "postgres",
			fmt.Sprintf("ALTER DATABASE %s OWNER TO %s", quoteIdent(opts.db), quoteIdent(appUser))); err != nil {
			return fmt.Errorf("alter database owner: %w", err)
		}
	}

	// --- 正常終了 → snapshot ---
	// snapshot は必ず正常終了状態でのみ取得する。ここで落ちるとブランチ起動時に
	// crash recovery が走り、@baseline が dirty な状態で固定されてしまう。
	fmt.Println("→ 正常終了")
	if err := runAsUser(uid, gid, pgCtl, "stop", "-D", dataDir, "-m", "fast", "-w"); err != nil {
		return fmt.Errorf("pg_ctl stop: %w", err)
	}
	stopped = true

	fmt.Printf("→ snapshot %s 取得\n", snap)
	if out, err := exec.Command("zfs", "snapshot", snap).CombinedOutput(); err != nil {
		return fmt.Errorf("snapshot: %w: %s", err, strings.TrimSpace(string(out)))
	}
	registerImportedBaseline(cfg.StateDB, snap, cfg.Storage.Zfs.BaselineSnapshot)
	fmt.Println("baseline import 完了。sashiki create <name> でブランチを作れます")
	return nil
}

// pgLoadDump は --from を投入する。プレーン SQL は psql、pg_dump のカスタム/
// ディレクトリ形式は pg_restore で流す(形式は中身を見て判定する)。
func pgLoadDump(uid, gid uint32, psql, pgRestore, from, db string, threads int) error {
	fi, err := os.Stat(from)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		// pg_dump -Fd(ディレクトリ形式)。-j で並列復元できる。
		fmt.Printf("→ ダンプ投入 (%s, ディレクトリ形式, %d 並列)\n", from, threads)
		args := []string{"-w", "-d", db, "--no-owner", "-j", strconv.Itoa(max(threads, 1)), from}
		return runAsUserWithEnv(uid, gid, pgEnv(), pgRestore, args...)
	}
	if isPgCustomDump(from) {
		fmt.Printf("→ ダンプ投入 (%s, カスタム形式)\n", from)
		return runAsUserWithEnv(uid, gid, pgEnv(), pgRestore, "-w", "-d", db, "--no-owner", from)
	}
	fmt.Printf("→ ダンプ投入 (%s, プレーン SQL)\n", from)
	// ON_ERROR_STOP が無いと途中のエラーを黙って飛ばして「成功」してしまう。
	return runAsUserWithEnv(uid, gid, pgEnv(), psql,
		"-w", "-v", "ON_ERROR_STOP=1", "-d", db, "-f", from)
}

// isPgCustomDump は pg_dump のカスタム形式(先頭が "PGDMP")かを判定する。
func isPgCustomDump(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 5)
	if _, err := f.Read(head); err != nil {
		return false
	}
	return string(head) == "PGDMP"
}

// pgSQL は一時クラスタへ SQL を 1 本流す(local trust なのでパスワード不要)。
func pgSQL(uid, gid uint32, psql, db, sql string) error {
	return runAsUserWithEnv(uid, gid, pgEnv(), psql,
		"-w", "-v", "ON_ERROR_STOP=1", "-d", db, "-c", sql)
}

// pgEnv は一時クラスタへ unix socket 経由で繋ぐための環境変数。接続タイムアウトも
// 入れて、繋がらないときに CI で無限に待たないようにする(psql/pg_restore には
// -w も渡してパスワードプロンプトで固まるのを防いでいる)。
func pgEnv() []string {
	return []string{
		"PGHOST=" + pgBaselineSocket,
		"PGPORT=" + pgBaselinePort,
		"PGCONNECT_TIMEOUT=10",
	}
}

// pgBin は bin_dir 配下のツールを返す。bin_dir 未設定なら PATH に任せる。
func pgBin(binDir, name string) string {
	if binDir != "" {
		if p := filepath.Join(binDir, name); binExists(p) {
			return p
		}
	}
	return name
}

// lookupOSUser は OS ユーザーの uid/gid を引く。
func lookupOSUser(name string) (uint32, uint32, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, fmt.Errorf("%s ユーザーが見つかりません(postgresql はインストール済み?): %w", name, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return uint32(uid), uint32(gid), nil
}

// quoteIdent / quoteLiteral は SQL の識別子 / 文字列リテラルを安全に埋め込む。
// 設定由来の値(ロール名・DB 名・パスワード)をそのまま連結しないため。
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func quoteLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}

// runAsUserNoPipe は出力をパイプで捕まえずに実行する。pg_ctl start のように
// 「子がデーモン化して親の stdout/stderr を握ったまま生き続ける」場合、
// CombinedOutput はパイプが閉じるまで待ち続けて永久にブロックするため。
func runAsUserNoPipe(uid, gid uint32, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid}}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runAsUserWithEnv は runAsUser に環境変数を足したもの(postgres のツールは
// PGHOST / PGPORT で接続先を受け取る)。エラー時に出力を添えるのも同じ。
func runAsUserWithEnv(uid, gid uint32, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid}}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runLocalPostgresBaselineImport は apfs / reflink(ローカル CoW backend)向けの
// postgres baseline import(#227)。zfs を使わず、storage.local.root 配下に
// クラスタを作って localstore の SnapshotBase で baseline を取得する。
// macOS ネイティブでは非 root(ログインユーザー)で動くので降格しない。
func runLocalPostgresBaselineImport(cfg config.Config, opts baselineImportOpts) error {
	root := cfg.Storage.Local.Root
	if root == "" {
		return fmt.Errorf("storage.local.root が未設定です(apfs/reflink には必須)")
	}
	preferredTag := cfg.Storage.Local.BaselineSnapshot
	if preferredTag == "" {
		preferredTag = "baseline"
	}
	dataDir := filepath.Join(root, "base", "data")
	baselineTag, replacing := localBaselineTag(filepath.Join(root, "base", "snap"), preferredTag)
	snapPath := filepath.Join(root, "base", "snap", baselineTag)
	if err := prepareLocalBaseData(dataDir, replacing); err != nil {
		return err
	}

	// root(コンテナ)なら run_user へ降格する。非 root(macOS ネイティブの
	// ログインユーザー)ではそのまま自分で起動する — postgres ユーザーが
	// 居ない/setuid できないため(mysql 側 #138 と同じ理由)。
	asRoot := os.Geteuid() == 0
	var uid, gid uint32
	if asRoot {
		runUser := cfg.EngineRunUser()
		if runUser == "" {
			runUser = "postgres"
		}
		var err error
		if uid, gid, err = lookupOSUser(runUser); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	if asRoot {
		if err := chownR(filepath.Join(root, "base"), uid, gid); err != nil {
			return err
		}
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return err
	}

	binDir := cfg.Engine.Postgres.BinDir
	initdb := pgBin(binDir, "initdb")
	pgCtl := pgBin(binDir, "pg_ctl")
	psql := pgBin(binDir, "psql")
	pgRestore := pgBin(binDir, "pg_restore")

	// 非 root では降格しない実行関数に差し替える。
	run := func(name string, args ...string) error {
		if asRoot {
			return runAsUser(uid, gid, name, args...)
		}
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	runEnv := func(name string, args ...string) error {
		if asRoot {
			return runAsUserWithEnv(uid, gid, pgEnv(), name, args...)
		}
		cmd := exec.Command(name, args...)
		cmd.Env = append(os.Environ(), pgEnv()...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
		}
		return nil
	}

	fmt.Println("→ initdb")
	initArgs := []string{"-D", dataDir, "--auth-host=scram-sha-256", "--auth-local=trust"}
	initArgs = append(initArgs, cfg.Engine.Postgres.InitdbArgs...)
	if err := run(initdb, initArgs...); err != nil {
		return fmt.Errorf("initdb: %w", err)
	}

	fmt.Println("→ postgres 起動")
	startOpts := fmt.Sprintf("-p %s -c listen_addresses='' -c unix_socket_directories=%s",
		pgBaselinePort, pgBaselineSocket)
	// -l を渡さないとデーモンが親のパイプを握って CombinedOutput がブロックする。
	startArgs := []string{"start", "-D", dataDir, "-o", startOpts, "-l", pgBaselineLog, "-w"}
	if asRoot {
		if err := runAsUserNoPipe(uid, gid, pgCtl, startArgs...); err != nil {
			return fmt.Errorf("pg_ctl start: %w (詳細は %s)", err, pgBaselineLog)
		}
	} else {
		cmd := exec.Command(pgCtl, startArgs...)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("pg_ctl start: %w (詳細は %s)", err, pgBaselineLog)
		}
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = run(pgCtl, "stop", "-D", dataDir, "-m", "immediate", "-w")
		}
	}()

	db := opts.db
	if db == "" {
		db = "postgres"
	}
	if opts.db != "" {
		fmt.Printf("→ データベース %s 作成\n", opts.db)
		if err := runEnv(psql, "-w", "-v", "ON_ERROR_STOP=1", "-d", "postgres",
			"-c", "CREATE DATABASE "+quoteIdent(opts.db)); err != nil {
			return fmt.Errorf("create database %s: %w", opts.db, err)
		}
	}
	if opts.from != "" {
		fromPath, cleanup, err := pgResolveDump(opts.from)
		if err != nil {
			return err
		}
		defer cleanup()
		fi, err := os.Stat(fromPath)
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			fmt.Printf("→ ダンプ投入 (%s, ディレクトリ形式, %d 並列)\n", fromPath, max(opts.threads, 1))
			err = runEnv(pgRestore, "-w", "-d", db, "--no-owner",
				"-j", strconv.Itoa(max(opts.threads, 1)), fromPath)
		case isPgCustomDump(fromPath):
			fmt.Printf("→ ダンプ投入 (%s, カスタム形式)\n", fromPath)
			err = runEnv(pgRestore, "-w", "-d", db, "--no-owner", fromPath)
		default:
			fmt.Printf("→ ダンプ投入 (%s, プレーン SQL)\n", fromPath)
			err = runEnv(psql, "-w", "-v", "ON_ERROR_STOP=1", "-d", db, "-f", fromPath)
		}
		if err != nil {
			return err
		}
	}

	appUser, appPass := cfg.AppUser(), cfg.AppPass()
	fmt.Printf("→ 接続ロール %s 作成\n", appUser)
	roleSQL := fmt.Sprintf("CREATE ROLE %s LOGIN SUPERUSER PASSWORD %s",
		quoteIdent(appUser), quoteLiteral(appPass))
	if err := runEnv(psql, "-w", "-v", "ON_ERROR_STOP=1", "-d", "postgres", "-c", roleSQL); err != nil {
		return fmt.Errorf("create role %s: %w", appUser, err)
	}
	if opts.db != "" {
		if err := runEnv(psql, "-w", "-v", "ON_ERROR_STOP=1", "-d", "postgres", "-c",
			fmt.Sprintf("ALTER DATABASE %s OWNER TO %s", quoteIdent(opts.db), quoteIdent(appUser))); err != nil {
			return fmt.Errorf("alter database owner: %w", err)
		}
	}

	// snapshot は必ず正常終了状態でのみ取得する。
	fmt.Println("→ 正常終了")
	if err := run(pgCtl, "stop", "-D", dataDir, "-m", "fast", "-w"); err != nil {
		return fmt.Errorf("pg_ctl stop: %w", err)
	}
	stopped = true

	fmt.Printf("→ snapshot %s 取得\n", snapPath)
	st, err := localstore.New(cfg.Storage.Backend, root, baselineTag)
	if err != nil {
		return err
	}
	snap, err := st.SnapshotBase(context.Background(), baselineTag)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	registerImportedBaseline(cfg.StateDB, string(snap), baselineTag)
	fmt.Println("baseline import 完了。sashiki create <name> でブランチを作れます")
	return nil
}

// pgResolveDump は --from が s3:// / -(標準入力)のときの取り扱いを決める(#242)。
// プレーン SQL ならストリームのまま流したいが、pg_restore のカスタム形式は
// シーク可能なファイルを要求するため、その場合だけ一時ファイルへ落とす。
// 判定には先頭 5 byte("PGDMP")を覗く必要があるので、いったん取得してから見る。
//
// 戻り値はローカルパス(ストリームでも一時ファイル経由)と後片付け関数。
func pgResolveDump(from string) (string, func(), error) {
	if !isS3(from) && !isStdio(from) {
		return from, func() {}, nil
	}
	// psql はストリームでも動くが、カスタム形式かどうかは中身を見ないと分からず、
	// 判定のために先頭を読むと巻き戻せない。ここは素直に一時ファイルへ落とし、
	// 形式判定と pg_restore の要件(シーク)を両立させる。
	fmt.Printf("→ %s を取得中(形式判定と pg_restore のためローカルへ)\n", from)
	return materialize(from, "sashiki-pg-dump-*.dump")
}
