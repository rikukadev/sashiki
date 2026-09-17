// sashiki baseline: ベースライン管理。
// import はローカル root 操作(sashikid 不要): base の mysqld を初期化 →
// ダンプ投入 → 接続ユーザー作成 → 正常終了 → @baseline snapshot。
// 「スナップショットは必ず正常終了状態でのみ取得する」不変条件はここで守られる。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rikukadev/sashiki/internal/config"
	"github.com/rikukadev/sashiki/internal/localstore"
	"github.com/rikukadev/sashiki/internal/state"
)

func cmdBaseline(args []string) int {
	if len(args) < 1 {
		return usageBaseline()
	}
	switch args[0] {
	case "import":
		return cmdBaselineImport(args[1:])
	case "list":
		return cmdBaselineList(args[1:])
	case "set":
		return cmdBaselineSet(args[1:])
	case "gc":
		return cmdBaselineGC(args[1:])
	case "refresh":
		return cmdBaselineRefresh(args[1:])
	case "build":
		return cmdBaselineBuild(args[1:])
	case "validate":
		return cmdBaselineStage(args[1:], "validate")
	case "publish":
		return cmdBaselineStage(args[1:], "publish")
	case "delete":
		return cmdBaselineStage(args[1:], "delete")
	case "export":
		return cmdBaselineExport(args[1:])
	case "import-stream":
		return cmdBaselineImportStream(args[1:])
	case "promote":
		return cmdBaselinePromote(args[1:])
	default:
		return usageBaseline()
	}
}

// cmdBaselinePromote は既存ブランチを新 baseline に昇格する(#129)。
// --masked は require_masked 用の宣言、--skip-validate は _validate を飛ばす(#296)。
func cmdBaselinePromote(args []string) int {
	body := map[string]any{}
	var pos []string
	for _, a := range args {
		switch a {
		case "--masked":
			body["masked"] = true
		case "--skip-validate":
			body["skip_validate"] = true
		default:
			if strings.HasPrefix(a, "--") {
				fmt.Fprintf(os.Stderr, "sashiki baseline promote: 不明なフラグ %s\n", a)
				return exitUsage
			}
			pos = append(pos, a)
		}
	}
	if len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: sashiki baseline promote <branch> [--masked] [--skip-validate]")
		return exitUsage
	}
	body["branch"] = pos[0]
	code, data, err := call("POST", "/v1/baseline/promote", body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	var r struct{ From, Current string }
	_ = json.Unmarshal(data, &r)
	fmt.Printf("promoted '%s' to current baseline: %s\n", pos[0], r.Current)
	return exitOK
}

// cmdBaselineBuild は build 段階を実行し(既定 --wait)、candidate の snapshot を表示する(#84)。
func cmdBaselineBuild(args []string) int {
	_, noWait, timeout, interval := extractWaitFlags(args)
	code, data, err := call("POST", "/v1/baseline/build", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusAccepted {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	var b struct {
		OperationID string `json:"operation_id"`
		Snapshot    string `json:"snapshot"`
	}
	_ = json.Unmarshal(data, &b)
	done, exit := awaitMutation(data, noWait, timeout, interval)
	if !done {
		if !noWait {
			return exit
		}
		fmt.Printf("baseline build started: %s (operation %s)\n", b.Snapshot, b.OperationID)
		return exitOK
	}
	fmt.Printf("baseline built: %s\n  次: sashiki baseline validate %s / publish %s\n", b.Snapshot, b.Snapshot, b.Snapshot)
	return exitOK
}

// cmdBaselineStage は validate / publish / delete を実行する(#84)。
func cmdBaselineStage(args []string, stage string) int {
	rest, noWait, timeout, interval := extractWaitFlags(args)
	if len(rest) != 1 {
		fmt.Fprintf(os.Stderr, "usage: sashiki baseline %s <snapshot>\n", stage)
		return exitUsage
	}
	snap := rest[0]
	code, data, err := call("POST", "/v1/baseline/"+stage, map[string]any{"snapshot": snap})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	switch code {
	case http.StatusOK: // publish / delete は同期
		fmt.Printf("baseline %s: %s\n", stage, snap)
		return exitOK
	case http.StatusAccepted: // validate は非同期
		done, exit := awaitMutation(data, noWait, timeout, interval)
		if !done {
			return exit
		}
		fmt.Printf("baseline %s: %s\n", stage, snap)
		return exitOK
	default:
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
}

func usageBaseline() int {
	fmt.Fprint(os.Stderr, `Usage:
  sashiki baseline import --from <dump.sql|dir|s3://...|-> [--db <name>] [--import-cnf <my.cnf>] [--threads N] [--config <path>]   ベース構築 + @baseline 取得 (zfs は root)
      --from がディレクトリなら *.sql を並列投入(名前に schema を含むファイルを先に投入。既定 --threads = CPU 数)
      --from に s3:// URL や -(標準入力)も渡せる(ホストに落とさず投入、#242)
  sashiki baseline export --to <path|s3://...|-> [--snapshot <tag>]        baseline を zfs send で書き出す (#243)
  sashiki baseline import-stream --from <path|s3://...|-> [--force]        書き出した baseline を zfs recv して current にする (#243)
  sashiki baseline list [--json]                                snapshot 一覧 (sashikid 経由)
  sashiki baseline refresh                                      refresh_script / source_dir で更新
  sashiki baseline promote <branch> [--masked] [--skip-validate] migrate 済み branch を新 baseline に昇格 (#129)。
                                                                 require_masked なら --masked の宣言が要る (#296)
  sashiki baseline set|delete <snapshot> / build / validate / publish / gc
`)
	return exitUsage
}

// --- import ---

type baselineImportOpts struct {
	from       string
	configPath string
	db         string // 投入先 DB(#170)。USE を含まない単体 DB ダンプ向け
	importCnf  string // import 中だけ使う my.cnf(buffer pool 等を緩める、#194)
	threads    int    // --from がディレクトリのときの並列投入数(#194)
}

func cmdBaselineImport(args []string) int {
	opts := baselineImportOpts{configPath: "/etc/sashiki/config.yaml", threads: runtime.NumCPU()}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--from":
			i++
			if i >= len(args) {
				return usageBaseline()
			}
			opts.from = args[i]
		case "--config":
			i++
			if i >= len(args) {
				return usageBaseline()
			}
			opts.configPath = args[i]
		case "--db":
			i++
			if i >= len(args) {
				return usageBaseline()
			}
			opts.db = args[i]
		case "--import-cnf":
			i++
			if i >= len(args) {
				return usageBaseline()
			}
			opts.importCnf = args[i]
		case "--threads":
			i++
			if i >= len(args) {
				return usageBaseline()
			}
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 {
				fmt.Fprintln(os.Stderr, "sashiki baseline import: --threads は 1 以上の整数")
				return exitError
			}
			opts.threads = n
		default:
			return usageBaseline()
		}
	}
	// s3:// / -(標準入力)はローカルに実体が無いので存在チェックしない(#242)。
	if opts.from != "" && !isS3(opts.from) && !isStdio(opts.from) {
		if _, err := os.Stat(opts.from); err != nil {
			fmt.Fprintf(os.Stderr, "sashiki baseline import: --from %s: %v\n", opts.from, err)
			return exitError
		}
	}
	if opts.importCnf != "" {
		if _, err := os.Stat(opts.importCnf); err != nil {
			fmt.Fprintf(os.Stderr, "sashiki baseline import: --import-cnf %s: %v\n", opts.importCnf, err)
			return exitError
		}
	}
	cfg, err := config.Load(opts.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sashiki baseline import: config: %v\n", err)
		return exitError
	}
	// root が要るのは zfs(systemd)経路だけ。apfs/reflink のローカル CoW は
	// ログインユーザー(macOS ネイティブ)や root コンテナで動くので要求しない(#138)。
	local := cfg.Storage.Backend == "apfs" || cfg.Storage.Backend == "reflink"
	if !local && os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "sashiki baseline import: root で実行してください")
		return exitError
	}
	if err := runBaselineImport(cfg, opts); err != nil {
		fmt.Fprintln(os.Stderr, "sashiki baseline import:", err)
		return exitError
	}
	return exitOK
}

// runLocalBaselineImport は apfs / reflink backend 用の baseline import。
// zfs コマンドを使わず、<root>/base/data に mysqld で投入して正常終了し、
// storage backend の SnapshotBase で <root>/base/snap/<baseline> を作る(#113/#140)。
// loadDump は dump を mysqld(socket)へ投入する(#170)。db 指定時は先に
// CREATE DATABASE し、その DB を default に選んで投入する(USE を含まない単体 DB
// ダンプ向け)。無指定ならダンプ内の CREATE/USE に従う(従来動作)。
func loadDump(mysqlBin, sock, from, db string, threads int) error {
	if db != "" {
		mk := exec.Command(mysqlBin, "-uroot", "-S", sock, "-e",
			fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`", db))
		if out, err := mk.CombinedOutput(); err != nil {
			return fmt.Errorf("create database %s: %w: %s", db, err, strings.TrimSpace(string(out)))
		}
	}
	// s3:// / -(標準入力)はホストに落とさずそのまま流し込む(#242)。
	// mysql が受けるのはプレーン SQL なので、ストリームのまま投入できる。
	if isS3(from) || isStdio(from) {
		src, err := openSource(from)
		if err != nil {
			return err
		}
		defer func() { _ = src.Close() }()
		fmt.Printf("→ ダンプ投入 (%s, ストリーム)\n", from)
		if err := loadReader(mysqlBin, sock, src, db); err != nil {
			return err
		}
		return src.Close() // S3 側の失敗をここで拾う
	}
	fi, err := os.Stat(from)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return loadDumpDir(mysqlBin, sock, from, db, threads)
	}
	fmt.Printf("→ ダンプ投入 (%s)\n", from)
	return loadOneFile(mysqlBin, sock, from, db)
}

// loadReader は任意の io.Reader を mysql の標準入力へ流し込む(#242)。
// loadOneFile と同じセッション設定(バルクロード用フラグ)を使う。
func loadReader(mysqlBin, sock string, r io.Reader, db string) error {
	args := []string{
		"-uroot", "-S", sock,
		"--init-command=SET unique_checks=0, foreign_key_checks=0, sql_log_bin=0",
	}
	if db != "" {
		args = append(args, db)
	}
	load := exec.Command(mysqlBin, args...)
	load.Stdin = r
	if out, err := load.CombinedOutput(); err != nil {
		return fmt.Errorf("load stream: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// loadOneFile は 1 本の .sql を mysqld へ投入する。バルク投入の高速化(#194):
// unique / FK チェックと binlog 書き込みをこの投入セッションだけ無効化する
// (baseline は使い捨てブランチの元でダンプ自体が整合しているので再チェックは
// 不要。セッション限定なので import 後の実行時は制約が有効)。--init-command は
// stdin を読む前に 1 度だけ実行される。
func loadOneFile(mysqlBin, sock, path, db string) error {
	dump, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dump.Close() }()
	args := []string{
		"-uroot", "-S", sock,
		"--init-command=SET unique_checks=0, foreign_key_checks=0, sql_log_bin=0",
	}
	if db != "" {
		args = append(args, db) // 既定 DB を選択(USE 無しダンプがこの DB に入る)
	}
	load := exec.Command(mysqlBin, args...)
	load.Stdin = dump
	if out, err := load.CombinedOutput(); err != nil {
		return fmt.Errorf("load %s: %w: %s", filepath.Base(path), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// loadDumpDir はディレクトリ内の *.sql を並列投入する(#194)。mydumper 出力や
// テーブル単位に分割したダンプを想定。名前に "schema" を含むファイル(mydumper の
// *-schema.sql / *-schema-create.sql 等)を先に順次投入して全テーブルを作ってから、
// 残りのデータファイルを threads 本の接続で並列投入する。FK チェックは投入中
// 無効なので、データファイル間の投入順序は問わない。
func loadDumpDir(mysqlBin, sock, dir, db string, threads int) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var schema, data []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".sql") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if strings.Contains(strings.ToLower(e.Name()), "schema") {
			schema = append(schema, p)
		} else {
			data = append(data, p)
		}
	}
	if len(schema) == 0 && len(data) == 0 {
		return fmt.Errorf("%s に *.sql がありません", dir)
	}
	sort.Strings(schema)
	sort.Strings(data)
	// スキーマ(テーブル定義)を先に、依存順が読めないので順次。
	for _, p := range schema {
		fmt.Printf("→ スキーマ投入 (%s)\n", filepath.Base(p))
		if err := loadOneFile(mysqlBin, sock, p, db); err != nil {
			return err
		}
	}
	if threads < 1 {
		threads = 1
	}
	fmt.Printf("→ データ投入 (%d ファイルを %d 並列)\n", len(data), threads)
	sem := make(chan struct{}, threads)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for _, p := range data {
		p := p
		mu.Lock()
		stop := firstErr != nil
		mu.Unlock()
		if stop {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := loadOneFile(mysqlBin, sock, p, db); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// authPluginFor は backend の版に応じて app_user の認証プラグインを選ぶ。
// caching_sha2_password は MySQL 8.0 で追加されたため 5.7 以前には無い。8.0 以降
// (8.1〜8.3 / 8.4 / 9.x を含む)は caching_sha2、5.7 等(major<8)は
// mysql_native_password を使う。8.4 は native 既定 OFF・9.x は native 廃止なので、
// この分岐で全域(5.7〜9.x)を一本化する(#version-compat)。判定不能時は既定の
// caching_sha2(sashiki の既定 mysqld は 8.0+)。
func authPluginFor(mysqlBin, sock string) string {
	out, err := exec.Command(mysqlBin, "-uroot", "-S", sock, "-N", "-B", "-e", "SELECT @@version").Output()
	if err != nil {
		return "caching_sha2_password" // 判定不能時の既定(sashiki の既定 mysqld は 8.0+)
	}
	return pluginForVersion(strings.TrimSpace(string(out)))
}

// pluginForVersion は @@version 文字列から app_user の認証プラグインを選ぶ(純関数)。
//   - MariaDB(10.x/11.x など)は caching_sha2 を持たない(native / ed25519)ので native。
//     ※ MariaDB は best-effort。他の挙動は未検証で正式サポート対象ではない。
//   - MySQL 5.7 等(major<8)も caching_sha2 が 8.0 追加のため無く native。
//   - MySQL 8.0+(8.1〜8.3 / 8.4 / 9.x)は caching_sha2(8.4 native 既定 OFF・9.x native 廃止)。
//
// major は 10 でも MariaDB 名を先に見るので、MariaDB を MySQL 8+ と誤判定しない(#version-compat)。
func pluginForVersion(v string) string {
	if strings.Contains(v, "MariaDB") {
		return "mysql_native_password"
	}
	if major, ok := majorVersion(v); ok && major < 8 {
		return "mysql_native_password"
	}
	return "caching_sha2_password"
}

// majorVersion は "8.0.46" / "5.7.44-log" / "9.6.0" 等から先頭の major を取る。
func majorVersion(v string) (int, bool) {
	dot := strings.IndexByte(v, '.')
	if dot <= 0 {
		return 0, false
	}
	n, err := strconv.Atoi(v[:dot])
	if err != nil {
		return 0, false
	}
	return n, true
}

// mysqldBinOr は設定の mysqld パス(未設定なら /usr/sbin/mysqld)を返す。
func mysqldBinOr(cfg config.Config) string {
	if b := cfg.Engine.Mysql.MysqldBin; b != "" {
		return b
	}
	return "/usr/sbin/mysqld"
}

func runLocalBaselineImport(cfg config.Config, opts baselineImportOpts) error {
	root := cfg.Storage.Local.Root
	if root == "" {
		return fmt.Errorf("storage.local.root が未設定です(apfs/reflink には必須)")
	}
	baselineTag := cfg.Storage.Local.BaselineSnapshot
	if baselineTag == "" {
		baselineTag = "baseline"
	}
	dataDir := filepath.Join(root, "base", "data")
	snapPath := filepath.Join(root, "base", "snap", baselineTag)

	if _, err := os.Stat(snapPath); err == nil {
		return fmt.Errorf("baseline %s は既に存在します。取得し直しは baseline refresh で対応", snapPath)
	}
	if entries, err := os.ReadDir(dataDir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s が空ではありません。初期化済みの base に import はできません", dataDir)
	}

	// root(コンテナ)なら mysqld を mysql ユーザーに落として動かし、datadir も
	// chown する。非 root(macOS ネイティブのログインユーザー)ではそのまま自分で
	// 起動する — mysql ユーザーが居ない/setuid できないため(#138)。
	asRoot := os.Geteuid() == 0
	var mysqlUID, mysqlGID uint32
	if asRoot {
		var err error
		mysqlUID, mysqlGID, err = lookupMysqlUser()
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return err
	}
	if asRoot {
		if err := chownR(filepath.Join(root, "base"), mysqlUID, mysqlGID); err != nil {
			return err
		}
	}
	runMysqld := func(args ...string) error {
		// import 専用 cnf があれば先頭に置く(buffer pool 等を投入中だけ緩める、#194)。
		if opts.importCnf != "" {
			args = append([]string{"--defaults-file=" + opts.importCnf}, args...)
		}
		if asRoot {
			return runAsUser(mysqlUID, mysqlGID, mysqldBinOr(cfg), args...)
		}
		out, err := exec.Command(mysqldBinOr(cfg), args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w: %s", mysqldBinOr(cfg), err, strings.TrimSpace(string(out)))
		}
		return nil
	}

	mysqldBin := mysqldBinOr(cfg)
	// client は mysqld の隣から解決する(PATH 側の別メジャーを引かない、#149)。
	mysqlBin := mysqlClientBin(mysqldBin, "mysql")
	mysqladminBin := mysqlClientBin(mysqldBin, "mysqladmin")
	sock := "/tmp/sashiki-baseline.sock"
	logErr := filepath.Join(cfg.Storage.Local.Root, "baseline.err")

	fmt.Println("→ mysqld 初期化")
	if err := runMysqld("--initialize-insecure", "--datadir="+dataDir, "--log-error="+logErr); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	fmt.Println("→ mysqld 起動")
	if err := runMysqld("--datadir="+dataDir, "--port=0", "--skip-networking",
		"--socket="+sock, "--pid-file=/tmp/sashiki-baseline.pid", "--log-error="+logErr, "--daemonize"); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = exec.Command(mysqladminBin, "-uroot", "-S", sock, "shutdown").Run()
		}
	}()
	if err := waitSocket(sock, 60*time.Second, mysqladminBin); err != nil {
		return err
	}
	if opts.from != "" {
		if err := loadDump(mysqlBin, sock, opts.from, opts.db, opts.threads); err != nil {
			return err
		}
	}
	fmt.Printf("→ 接続ユーザー %s 作成\n", cfg.Engine.Mysql.ProxyUser)
	plugin := authPluginFor(mysqlBin, sock)
	createUser := fmt.Sprintf(
		"CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED WITH %s BY '%s'; GRANT ALL PRIVILEGES ON *.* TO '%s'@'%%'; FLUSH PRIVILEGES;",
		cfg.Engine.Mysql.ProxyUser, plugin, cfg.Engine.Mysql.ProxyPass, cfg.Engine.Mysql.ProxyUser)
	if out, err := exec.Command(mysqlBin, "-uroot", "-S", sock, "-e", createUser).CombinedOutput(); err != nil {
		return fmt.Errorf("create user: %w: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Println("→ 正常終了")
	if out, err := exec.Command(mysqladminBin, "-uroot", "-S", sock, "shutdown").CombinedOutput(); err != nil {
		return fmt.Errorf("shutdown: %w: %s", err, strings.TrimSpace(string(out)))
	}
	stopped = true
	if err := waitGone("/tmp/sashiki-baseline.pid", 30*time.Second); err != nil {
		return err
	}
	fmt.Println("→ auto.cnf 削除 (server_uuid 重複対策)")
	if err := os.Remove(filepath.Join(dataDir, "auto.cnf")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove auto.cnf: %w", err)
	}

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

func runBaselineImport(cfg config.Config, opts baselineImportOpts) error {
	// postgres は initdb / psql / pg_restore を使う別経路(baseline_postgres.go、#223)。
	if cfg.Engine.Type == "postgres" {
		return runPostgresBaselineImport(cfg, opts)
	}
	// apfs / reflink はローカル CoW backend(zfs コマンドを使わない)。
	if cfg.Storage.Backend == "apfs" || cfg.Storage.Backend == "reflink" {
		return runLocalBaselineImport(cfg, opts)
	}

	base := cfg.Storage.Zfs.BaseDataset
	snap := base + "@" + cfg.Storage.Zfs.BaselineSnapshot

	// 既存 @baseline があれば失敗(上書きは派生ブランチを壊すため refresh フローで扱う)
	if err := exec.Command("zfs", "list", snap).Run(); err == nil {
		return fmt.Errorf("snapshot %s は既に存在します。取得し直しは baseline refresh (issue #10) で対応予定", snap)
	}

	mountOut, err := exec.Command("zfs", "get", "-H", "-o", "value", "mountpoint", base).Output()
	if err != nil {
		return fmt.Errorf("dataset %s が見つかりません。先に sashiki init を実行してください", base)
	}
	dataDir := filepath.Join(strings.TrimSpace(string(mountOut)), "data")

	if entries, err := os.ReadDir(dataDir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s が空ではありません。初期化済みの base に import はできません", dataDir)
	}

	mysqlUID, mysqlGID, err := lookupMysqlUser()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return err
	}
	if err := chownR(filepath.Dir(dataDir), mysqlUID, mysqlGID); err != nil {
		return err
	}

	sock := "/tmp/sashiki-baseline.sock"
	logErr := filepath.Join(cfg.Hooks.LogDir, "..", "baseline.err")

	// mysqld のパスと defaults を config から取る(#127)。固定 /usr/sbin/mysqld を
	// やめ、engine.mysql.mysqld_bin / extra_cnf を使うことで投入時の sql_mode /
	// strict を実行時と揃える。mysqladmin は mysqld と同じ場所から解決する。
	mysqldBin := cfg.Engine.Mysql.MysqldBin
	if mysqldBin == "" {
		mysqldBin = "/usr/sbin/mysqld"
	}
	// import 中の mysqld が読む defaults。--import-cnf(#194)があれば投入中だけ
	// それを使い(buffer pool 等を緩める)、無ければ従来どおり extra_cnf(#127、
	// 投入時の sql_mode / strict を実行時と揃える)。両者は排他(mysqld の
	// --defaults-file は 1 つだけ。import-cnf 側に必要な設定を含める前提)。
	var defaults []string
	switch {
	case opts.importCnf != "":
		defaults = []string{"--defaults-file=" + opts.importCnf}
	case cfg.Engine.Mysql.ExtraCnf != "":
		defaults = []string{"--defaults-file=" + cfg.Engine.Mysql.ExtraCnf}
	}
	// client は mysqld の隣から解決する(PATH 側の別メジャーを引かない、#149)。
	mysqlBin := mysqlClientBin(mysqldBin, "mysql")
	mysqladminBin := mysqlClientBin(mysqldBin, "mysqladmin")

	fmt.Println("→ mysqld 初期化")
	initArgs := append(defaults, "--initialize-insecure", "--datadir="+dataDir, "--log-error="+logErr)
	if err := runAsUser(mysqlUID, mysqlGID, mysqldBin, initArgs...); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	fmt.Println("→ mysqld 起動")
	startArgs := append(defaults, "--datadir="+dataDir, "--port=0", "--skip-networking",
		"--socket="+sock, "--pid-file=/tmp/sashiki-baseline.pid",
		"--log-error="+logErr, "--daemonize")
	if err := runAsUser(mysqlUID, mysqlGID, mysqldBin, startArgs...); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = exec.Command(mysqladminBin, "-uroot", "-S", sock, "shutdown").Run()
		}
	}()
	if err := waitSocket(sock, 60*time.Second, mysqladminBin); err != nil {
		return err
	}

	if opts.from != "" {
		if err := loadDump(mysqlBin, sock, opts.from, opts.db, opts.threads); err != nil {
			return err
		}
	}

	fmt.Printf("→ 接続ユーザー %s 作成\n", cfg.Engine.Mysql.ProxyUser)
	// backend の版で認証プラグインを選ぶ: 8.0+ は caching_sha2(8.4 native 既定 OFF /
	// 9.x native 廃止)、5.7 等は caching_sha2 が無いので mysql_native_password。
	// proxy は client 認証を自前検証し、backend へは選んだプラグインで接続し直す
	// (caching_sha2 の平文 TCP cold cache は RSA full-auth、native は AuthSwitch)。
	plugin := authPluginFor(mysqlBin, sock)
	createUser := fmt.Sprintf(
		"CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED WITH %s BY '%s'; GRANT ALL PRIVILEGES ON *.* TO '%s'@'%%'; FLUSH PRIVILEGES;",
		cfg.Engine.Mysql.ProxyUser, plugin, cfg.Engine.Mysql.ProxyPass, cfg.Engine.Mysql.ProxyUser)
	userCmd := exec.Command(mysqlBin, "-uroot", "-S", sock, "-e", createUser)
	if out, err := userCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create user: %w: %s", err, strings.TrimSpace(string(out)))
	}

	fmt.Println("→ 正常終了")
	if out, err := exec.Command(mysqladminBin, "-uroot", "-S", sock, "shutdown").CombinedOutput(); err != nil {
		return fmt.Errorf("shutdown: %w: %s", err, strings.TrimSpace(string(out)))
	}
	stopped = true
	if err := waitGone("/tmp/sashiki-baseline.pid", 30*time.Second); err != nil {
		return err
	}

	// server_uuid の重複対策(#80 / 仕様 12-3): datadir をクローンすると
	// auto.cnf の server_uuid まで複製され、全ブランチが同一 UUID になる。
	// 正常終了後・snapshot 取得前に削除しておけば、各ブランチの初回起動時に
	// mysqld が固有の UUID を再生成する。
	fmt.Println("→ auto.cnf 削除 (server_uuid 重複対策)")
	if err := os.Remove(filepath.Join(dataDir, "auto.cnf")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove auto.cnf: %w", err)
	}

	fmt.Printf("→ snapshot %s 取得\n", snap)
	if out, err := exec.Command("zfs", "snapshot", snap).CombinedOutput(); err != nil {
		return fmt.Errorf("snapshot: %w: %s", err, strings.TrimSpace(string(out)))
	}
	registerImportedBaseline(cfg.StateDB, snap, cfg.Storage.Zfs.BaselineSnapshot)
	fmt.Println("baseline import 完了。sashiki create <name> でブランチを作れます")
	return nil
}

// mysqlClientBin は mysqld と同じディレクトリの client(mysql / mysqladmin)を
// 返す。MysqldBin が絶対パス(Homebrew 等 PATH 外)のとき PATH 側の別メジャーの
// client を引くと native_password 認証や ready 判定が失敗するため、隣を優先する
// (#119 / #149)。見つからなければ素の名前(PATH 解決)にフォールバック。
func mysqlClientBin(mysqldBin, name string) string {
	if strings.Contains(mysqldBin, "/") {
		if p := filepath.Join(filepath.Dir(mysqldBin), name); binExists(p) {
			return p
		}
	}
	return name
}

// registerImportedBaseline は import した baseline を state.db に current として
// 登録する(仕様 12-1)。import 本体は成功しているので登録失敗は致命ではないが、
// 黙って握りつぶすと baseline list / GC の台帳から漏れるため warning を出す(#150)。
func registerImportedBaseline(stateDB, snap, dataAsOf string) {
	db, err := state.Open(stateDB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "警告: baseline を state.db に登録できません(baseline list / GC の台帳から漏れます): open %s: %v\n", stateDB, err)
		return
	}
	defer func() { _ = db.Close() }()
	if err := db.RegisterBaseline(snap, state.BaselineProvenance{DataAsOf: dataAsOf}); err != nil {
		fmt.Fprintf(os.Stderr, "警告: baseline の登録に失敗しました(台帳から漏れます): %v\n", err)
		return
	}
	if err := db.SetCurrentBaseline(snap); err != nil {
		fmt.Fprintf(os.Stderr, "警告: current baseline の設定に失敗しました(backend 既定にフォールバックします): %v\n", err)
	}
}

func lookupMysqlUser() (uint32, uint32, error) {
	u, err := user.Lookup("mysql")
	if err != nil {
		return 0, 0, fmt.Errorf("mysql ユーザーが見つかりません(mysql-server はインストール済み?): %w", err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return uint32(uid), uint32(gid), nil
}

func runAsUser(uid, gid uint32, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid}}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func waitSocket(sock string, timeout time.Duration, mysqladminBin string) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if exec.Command(mysqladminBin, "-uroot", "-S", sock, "ping").Run() == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("mysqld が %s 以内に ready になりませんでした", timeout)
}

func waitGone(pidFile string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(pidFile); os.IsNotExist(err) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("mysqld の停止を確認できませんでした (%s が残っています)", pidFile)
}

func chownR(root string, uid, gid uint32) error {
	return filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(path, int(uid), int(gid))
	})
}

// --- list (sashikid 経由) ---

func cmdBaselineList(args []string) int {
	_, _, jsonOut, err := parseFlags(args)
	if err != nil {
		return usageBaseline()
	}
	code, data, err := call("GET", "/v1/baseline", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	if jsonOut {
		fmt.Println(string(data))
		return exitOK
	}
	var resp struct {
		Current   string   `json:"current"`
		Snapshots []string `json:"snapshots"`
	}
	_ = json.Unmarshal(data, &resp)
	fmt.Println("current:", resp.Current)
	for _, s := range resp.Snapshots {
		marker := "  "
		if s == resp.Current {
			marker = "* "
		}
		fmt.Println(marker + s)
	}
	return exitOK
}

func cmdBaselineSet(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: sashiki baseline set <snapshot>")
		return exitUsage
	}
	code, data, err := call("POST", "/v1/baseline/set", map[string]any{"snapshot": args[0]})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	fmt.Printf("current baseline set to %s\n", args[0])
	return exitOK
}

func cmdBaselineGC(args []string) int {
	q := url.Values{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--keep-last":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "sashiki: --keep-last requires a value")
				return exitUsage
			}
			i++
			q.Set("keep_last", args[i])
		case "--dry-run":
			q.Set("dry_run", "true")
		}
	}
	path := "/v1/baseline/gc"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	code, data, err := call("POST", path, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	fmt.Println(string(data))
	return exitOK
}

func cmdBaselineRefresh(args []string) int {
	code, data, err := call("POST", "/v1/baseline/refresh", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusAccepted {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	fmt.Println(string(data))
	return exitOK
}
