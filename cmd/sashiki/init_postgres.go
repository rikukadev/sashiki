// sashiki init --engine postgres のステップ(#224)。MySQL 版(init.go の
// initSteps)と同じ「冪等なステップの列」だが、パッケージ・データセットの
// recordsize・systemd ユニット・config が postgres 向けになる。
package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"text/template"
)

//go:embed assets/postgres-sashiki@.service
var postgresUnit []byte

//go:embed assets/config.postgres.yaml.tmpl
var configPostgresTmpl string

func initStepsPostgres(opts initOpts) []initStep {
	steps := []initStep{}

	if !opts.skipPackages {
		steps = append(steps, initStep{
			name: "パッケージ導入 (zfsutils-linux, postgresql, postgresql-client)",
			done: func() bool { return cmdOK("zfs", "version") && findPgBinDir() != "" },
			run: func() error {
				return runCmd(envv("DEBIAN_FRONTEND=noninteractive"), "apt-get", "install", "-y", "-q",
					"zfsutils-linux", "postgresql", "postgresql-client")
			},
		})
	}

	steps = append(steps,
		initStep{
			name: "既定の postgresql サービスを停止・無効化",
			done: func() bool { return !cmdOK("systemctl", "is-enabled", "--quiet", "postgresql") },
			run: func() error {
				// ディストリ既定のクラスタが 5432 を握っているとプロキシと衝突する。
				_ = runCmd(nil, "systemctl", "stop", "postgresql")
				return runCmd(nil, "systemctl", "disable", "postgresql")
			},
		},
		// zpool と dataset を sudoers より先に確認する(#320)。
		initStep{
			name: fmt.Sprintf("zpool %s", opts.pool),
			done: func() bool { return cmdOK("zpool", "list", opts.pool) },
			run: func() error {
				if opts.device == "" {
					return fmt.Errorf("pool %q が存在しません。--device <dev> を指定してください (lsblk で確認)", opts.pool)
				}
				// 既存 pool があれば作り直さず import する(#246)。terraform で
				// インスタンスを差し替えるとデータ EBS は prevent_destroy で残るが、
				// 新しいインスタンスでは pool が未 import なので done の zpool list に
				// 引っかからない。この分岐が無いと zpool create が既存 pool を拒否して
				// init がそこで止まる(データは無事だが手作業になる)。
				if importExistingPool(opts.pool, opts.device) {
					fmt.Println("    既存の pool を import しました(インスタンス差し替え)")
				} else if err := runCmd(nil, "zpool", "create", "-o", "ashift=12", opts.pool, opts.device); err != nil {
					return err
				}
				return runCmd(nil, "zfs", "set", "compression=lz4", "atime=off", opts.pool)
			},
		},
		initStep{
			// PostgreSQL のページサイズは 8KB。mysql(16k)と違う。
			name: fmt.Sprintf("データセット %s/base (recordsize=8k)", opts.pool),
			done: func() bool { return cmdOK("zfs", "list", opts.pool+"/base") },
			run: func() error {
				return runCmd(nil, "zfs", "create", "-o", "recordsize=8k", "-o", "logbias=throughput", opts.pool+"/base")
			},
		},
		initStep{
			// クローンのプロパティは origin ではなく名前空間上の親から継承されるため、
			// branch_parent 側にも recordsize=8k が要る(ここが 128k のままだと
			// 全ブランチが 128k で動いてしまう)。
			name: fmt.Sprintf("データセット %s/branches (recordsize=8k)", opts.pool),
			done: func() bool { return cmdOK("zfs", "list", opts.pool+"/branches") },
			run: func() error {
				return runCmd(nil, "zfs", "create", "-o", "recordsize=8k", "-o", "logbias=throughput", opts.pool+"/branches")
			},
		},
		initStep{
			name: "sudoers: sashiki ユーザーを zfs/systemctl の限定操作に制限",
			done: func() bool { return fileEqual(sudoersPath, []byte(sudoersContent(opts.pool))) },
			run:  func() error { return installSudoers(opts.pool) },
		},
		initStep{
			name: "ディレクトリ作成 (/etc/sashiki, /var/lib/sashiki, /var/log/sashiki)",
			run: func() error {
				for _, d := range []string{"/etc/sashiki/hooks", "/var/lib/sashiki/branches", "/var/log/sashiki/hooks"} {
					if err := os.MkdirAll(d, 0o755); err != nil {
						return err
					}
				}
				// postgres がログを書けるように
				return runCmd(nil, "chown", "-R", "postgres:postgres", "/var/log/sashiki")
			},
		},
		initStep{
			name: "systemd ユニット postgres-sashiki@.service",
			done: func() bool {
				return fileEqual("/etc/systemd/system/postgres-sashiki@.service", postgresUnit)
			},
			run: func() error {
				if err := os.WriteFile("/etc/systemd/system/postgres-sashiki@.service", postgresUnit, 0o644); err != nil {
					return err
				}
				return runCmd(nil, "systemctl", "daemon-reload")
			},
		},
		initStep{
			name: "/etc/sashiki/config.yaml 生成 (engine: postgres)",
			done: func() bool { _, err := os.Stat("/etc/sashiki/config.yaml"); return err == nil },
			run: func() error {
				cfg, err := renderPostgresConfigApp(opts.pool, opts.appPass)
				if err != nil {
					return err
				}
				return writeConfigFile("/etc/sashiki/config.yaml", cfg)
			},
		},
		configPermStep("/etc/sashiki/config.yaml"),
	)
	return steps
}

// renderPostgresConfig は postgres 用 config を生成する。bin_dir は実際に
// インストールされている版から解決する(バージョンをハードコードしない)。
func renderPostgresConfig(pool string) ([]byte, error) {
	return renderPostgresConfigApp(pool, "dev")
}

// renderPostgresConfigApp は app_pass を指定して postgres 用 config を生成する(#297)。
func renderPostgresConfigApp(pool, appPass string) ([]byte, error) {
	t, err := template.New("config").Parse(configPostgresTmpl)
	if err != nil {
		return nil, err
	}
	binDir := findPgBinDir()
	if binDir == "" {
		// 見つからない場合も config は書く(あとで手で直せる)。
		binDir = "/usr/lib/postgresql/16/bin"
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, struct {
		Pool     string
		PgBinDir string
		AppPass  string
	}{Pool: pool, PgBinDir: binDir, AppPass: yamlQuote(appPass)}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// findPgBinDir は /usr/lib/postgresql/<ver>/bin のうち最も新しい版を返す。
// Debian/Ubuntu は版ごとにディレクトリを切るため、固定パスだと版差で壊れる。
func findPgBinDir() string {
	entries, err := os.ReadDir("/usr/lib/postgresql")
	if err != nil {
		return ""
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() && binExists(filepath.Join("/usr/lib/postgresql", e.Name(), "bin", "postgres")) {
			versions = append(versions, e.Name())
		}
	}
	if len(versions) == 0 {
		return ""
	}
	// "9" < "10" にしたいので数値として比較する(辞書順だと 9 が最大になる)。
	sort.Slice(versions, func(i, j int) bool { return pgVersionLess(versions[i], versions[j]) })
	return filepath.Join("/usr/lib/postgresql", versions[len(versions)-1], "bin")
}

// pgVersionLess は "16" / "9.6" のような版文字列を数値順で比較する。
func pgVersionLess(a, b string) bool {
	ai, aok := pgMajor(a)
	bi, bok := pgMajor(b)
	if aok && bok {
		return ai < bi
	}
	return a < b
}

func pgMajor(v string) (int, bool) {
	n := 0
	seen := false
	for _, c := range v {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
		seen = true
	}
	return n, seen
}
