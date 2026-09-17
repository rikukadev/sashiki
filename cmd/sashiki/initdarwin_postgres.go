// macOS ネイティブの PostgreSQL セットアップ(#238)。MySQL 版(initdarwin.go)と
// 同じ段取りを postgres のツールで行う: base クラスタを initdb で作り、接続ロールと
// アプリ DB を用意し、**正常終了してから** clonefile で baseline を取得し、config を
// 生成して launchd に常駐登録する。
//
// 部品(process モード #227 / ローカル CoW の baseline import #227)は既にあるので、
// ここがやるのは「Homebrew の postgres を見つけて一括で組む」ところ。
package main

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed assets/config.darwin.postgres.yaml.tmpl
var configDarwinPostgresTmpl string

// init 中だけ使う一時クラスタのポート。TCP は開かない(socket 名にのみ使う)ので、
// 稼働中のブランチや baseline import(5499)と衝突しない値にしておく。
const pgInitPort = "5497"

func cmdInitDarwinPostgres(opts initOpts) int {
	binDir, err := resolvePostgresBinDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki init:", err)
		return exitError
	}
	root := opts.root
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "sashiki init:", err)
			return exitError
		}
		root = filepath.Join(home, "Library", "Application Support", "sashiki-pg")
	}
	sashikidBin, err := resolveSashikid()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki init:", err)
		return exitError
	}
	configPath := filepath.Join(root, "config.yaml")
	logDir := filepath.Join(root, "log")
	baseData := filepath.Join(root, "base", "data")
	baseline := filepath.Join(root, "base", "snap", "baseline")

	fmt.Printf("sashiki init (darwin/postgres): root=%s bin_dir=%s\n", root, binDir)
	if !opts.yes {
		fmt.Print("続行する? [y/N]: ")
		var ans string
		_, _ = fmt.Scanln(&ans)
		if len(ans) == 0 || (ans[0] != 'y' && ans[0] != 'Y') {
			fmt.Println("中止しました")
			return exitError
		}
	}

	pgBinOf := func(name string) string { return filepath.Join(binDir, name) }
	// 一時クラスタへは unix socket で繋ぐ(TCP は開かない)。
	env := []string{"PGHOST=" + os.TempDir(), "PGPORT=" + pgInitPort, "PGCONNECT_TIMEOUT=10"}

	steps := []initStep{
		{
			name: "ディレクトリ作成 (base / branches / run / log / hooks)",
			run: func() error {
				for _, d := range []string{
					filepath.Join(root, "base", "snap"),
					filepath.Join(root, "branches"),
					filepath.Join(root, "run"),
					logDir,
					filepath.Join(root, "hooks"),
				} {
					if err := os.MkdirAll(d, 0o755); err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			name: "base クラスタを初期化 (initdb)",
			done: func() bool { return dirNonEmpty(baseData) },
			run: func() error {
				// postgres は datadir が 0700 でないと起動を拒む。
				if err := os.MkdirAll(baseData, 0o700); err != nil {
					return err
				}
				// host を scram にすることで proxy → backend の TCP 接続でも
				// パスワード検証が働く(#222)。local は trust にして、この
				// セットアップ中の psql をパスワード無しで通す。
				return runOut(pgBinOf("initdb"), "-D", baseData,
					"--auth-host=scram-sha-256", "--auth-local=trust")
			},
		},
		{
			name: "baseline に接続ロール(dev)と app データベースを作成",
			done: func() bool { _, err := os.Stat(baseline); return err == nil },
			run: func() error {
				return withTempPostgres(binDir, baseData, logDir, func() error {
					if err := runOutEnv(env, pgBinOf("psql"), "-w", "-v", "ON_ERROR_STOP=1",
						"-d", "postgres", "-c",
						"CREATE ROLE "+quoteIdent("dev")+" LOGIN SUPERUSER PASSWORD "+quoteLiteral("dev")); err != nil {
						return err
					}
					return runOutEnv(env, pgBinOf("psql"), "-w", "-v", "ON_ERROR_STOP=1",
						"-d", "postgres", "-c",
						"CREATE DATABASE "+quoteIdent("app")+" OWNER "+quoteIdent("dev"))
				})
			},
		},
		{
			name: "baseline snapshot を取得 (clonefile)",
			done: func() bool { _, err := os.Stat(baseline); return err == nil },
			run: func() error {
				// 直前のステップで pg_ctl stop -m fast まで済んでいる(= 正常終了)。
				// snapshot は必ず正常終了状態でのみ取得する。
				return runOut("cp", "-Rc", baseData, baseline)
			},
		},
		{
			name: "config.yaml 生成 (engine: postgres)",
			done: func() bool { _, err := os.Stat(configPath); return err == nil },
			run: func() error {
				data, err := renderTmpl(configDarwinPostgresTmpl,
					map[string]string{"Root": root, "PgBinDir": binDir})
				if err != nil {
					return err
				}
				// app_pass を含むので所有者のみ(#295)。
				return os.WriteFile(configPath, data, 0o600)
			},
		},
		{
			name: "launchd に sashikid を登録 (dev.sashiki.sashikid-pg)",
			run: func() error {
				home, _ := os.UserHomeDir()
				agents := filepath.Join(home, "Library", "LaunchAgents")
				if err := os.MkdirAll(agents, 0o755); err != nil {
					return err
				}
				// mysql 版と別ラベルにして共存できるようにする。
				plistPath := filepath.Join(agents, "dev.sashiki.sashikid-pg.plist")
				data, err := renderTmpl(plistTmpl, map[string]string{
					"SashikidBin": sashikidBin, "Config": configPath, "Log": logDir,
				})
				if err != nil {
					return err
				}
				data = []byte(strings.ReplaceAll(string(data),
					"dev.sashiki.sashikid", "dev.sashiki.sashikid-pg"))
				if err := os.WriteFile(plistPath, data, 0o644); err != nil {
					return err
				}
				_ = exec.Command("launchctl", "unload", plistPath).Run()
				return runOut("launchctl", "load", "-w", plistPath)
			},
		},
	}

	for _, s := range steps {
		if s.done != nil && s.done() {
			fmt.Printf("  ✓ %s (済み・スキップ)\n", s.name)
			continue
		}
		fmt.Printf("  → %s\n", s.name)
		if err := s.run(); err != nil {
			fmt.Fprintf(os.Stderr, "sashiki init: %s: %v\n", s.name, err)
			return exitError
		}
	}

	// CLI(create/show/delete)は config ではなく API を見る。mysql 版と
	// ポートが違う構成もあり得るので SASHIKI_API_URL を明示して案内する。
	fmt.Printf(`
init 完了 (darwin/postgres)。sashikid は launchd で常駐しています。
  export SASHIKI_API_URL=http://127.0.0.1:8080

  ブランチ:  sashiki create pr-1
  接続:      PGPASSWORD=dev psql -h 127.0.0.1 -p 5432 -U 'dev@pr-1' -d app
             (未作成のブランチでも、接続した時点で作られる)
  停止:      launchctl unload ~/Library/LaunchAgents/dev.sashiki.sashikid-pg.plist

本番相当のデータを入れるなら(root 不要、ログインユーザーで実行):
  sashiki baseline import --from dump.sql --db app --config %s
`, configPath)
	return exitOK
}

// withTempPostgres は base クラスタを一時的に起動して fn を実行し、必ず
// 正常終了(pg_ctl stop -m fast)させる。snapshot の一貫性がこれに依存する。
func withTempPostgres(binDir, dataDir, logDir string, fn func() error) error {
	logPath := filepath.Join(logDir, "init-postgres.log")
	opts := fmt.Sprintf("-p %s -c listen_addresses='' -c unix_socket_directories=%s",
		pgInitPort, os.TempDir())
	// -l は必須。渡さないとデーモン化した postgres が親の stdout/stderr を握り
	// 続け、出力をパイプで捕まえる実行だと永久にブロックする(#223/#227)。
	start := exec.Command(filepath.Join(binDir, "pg_ctl"), "start", "-D", dataDir,
		"-o", opts, "-l", logPath, "-w", "-t", "60")
	start.Stdout, start.Stderr = os.Stderr, os.Stderr
	if err := start.Run(); err != nil {
		return fmt.Errorf("pg_ctl start: %w (詳細は %s)", err, logPath)
	}
	ferr := fn()
	stop := exec.Command(filepath.Join(binDir, "pg_ctl"), "stop", "-D", dataDir, "-m", "fast", "-w", "-t", "60")
	stop.Stdout, stop.Stderr = os.Stderr, os.Stderr
	if err := stop.Run(); err != nil {
		if ferr != nil {
			return ferr
		}
		return fmt.Errorf("pg_ctl stop: %w", err)
	}
	return ferr
}

// runOutEnv は runOut に環境変数を足したもの。
func runOutEnv(env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// resolvePostgresBinDir は Homebrew / PATH から postgres の bin ディレクトリを探す。
// Homebrew は版ごとに formula が分かれる(postgresql@17 等)ため、**数値順で最新**を
// 選ぶ(辞書順だと "9" > "16" になる)。
func resolvePostgresBinDir() (string, error) {
	var found []string
	for _, formula := range brewPostgresFormulae() {
		out, err := exec.Command("brew", "--prefix", formula).Output()
		if err != nil {
			continue
		}
		p := filepath.Join(trim(out), "bin")
		if binExists(filepath.Join(p, "initdb")) {
			found = append(found, p)
		}
	}
	if len(found) > 0 {
		return found[0], nil // brewPostgresFormulae が新しい順に返す
	}
	// PATH にあるならそれを使う(Postgres.app や手動導入)。
	if p, err := exec.LookPath("initdb"); err == nil {
		return filepath.Dir(p), nil
	}
	return "", fmt.Errorf("postgres が見つかりません。brew install postgresql@17 などで導入してください")
}

// brewPostgresFormulae は導入済みの postgresql formula を新しい版から順に返す。
func brewPostgresFormulae() []string {
	out, err := exec.Command("brew", "list", "--formula").Output()
	if err != nil {
		// brew が無い/失敗した場合は代表的な版を新しい順に当たる。
		return []string{"postgresql@18", "postgresql@17", "postgresql@16", "postgresql@15", "postgresql"}
	}
	var versioned []string
	plain := false
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		switch {
		case name == "postgresql":
			plain = true
		case strings.HasPrefix(name, "postgresql@"):
			versioned = append(versioned, name)
		}
	}
	sort.Slice(versioned, func(i, j int) bool {
		return pgVersionLess(strings.TrimPrefix(versioned[j], "postgresql@"),
			strings.TrimPrefix(versioned[i], "postgresql@")) // 降順
	})
	if plain {
		versioned = append(versioned, "postgresql")
	}
	return versioned
}
