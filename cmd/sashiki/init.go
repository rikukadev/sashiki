// sashiki init: ホストのセットアップを自動化する(仕様 12-5)。
// apt・AppArmor・zpool・データセット・systemd ユニット・config 生成を行う。
// 各ステップは冪等。
package main

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"text/template"
)

//go:embed assets/mysqld@.service
var mysqldUnit []byte

//go:embed assets/config.yaml.tmpl
var configTmpl string

type initOpts struct {
	pool         string
	device       string
	skipPackages bool
	poolExplicit bool // --pool が明示されたか(#320: 省略時は既存 config から拾う)
	yes          bool
	platform     string // linux(既定) | darwin
	root         string // darwin: storage.local.root(既定 ~/Library/Application Support/sashiki)
	engine       string // mysql(既定) | postgres(#224)
	// appPass は config に書く app_pass。Linux では未指定ならランダム生成する(#297)。
	// darwin は dev 用途なので既定 dev のまま。
	appPass string
}

// initStep は 1 ステップ。done が true を返したらスキップする。
type initStep struct {
	name string
	done func() bool
	run  func() error
}

func cmdInit(args []string) int {
	opts := initOpts{pool: "dbpool", platform: runtime.GOOS, engine: "mysql"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--pool":
			i++
			if i >= len(args) {
				return usage()
			}
			opts.pool = args[i]
			opts.poolExplicit = true
		case "--device":
			i++
			if i >= len(args) {
				return usage()
			}
			opts.device = args[i]
		case "--platform":
			i++
			if i >= len(args) {
				return usage()
			}
			opts.platform = args[i]
		case "--root":
			i++
			if i >= len(args) {
				return usage()
			}
			opts.root = args[i]
		case "--engine":
			i++
			if i >= len(args) {
				return usage()
			}
			opts.engine = args[i]
		case "--app-pass":
			i++
			if i >= len(args) {
				return usage()
			}
			opts.appPass = args[i]
		case "--skip-packages":
			opts.skipPackages = true
		case "--yes", "-y":
			opts.yes = true
		default:
			return usage()
		}
	}
	// --engine の検証は root 検査より前に行う(#325: 非 root だと「root で実行して
	// ください」が先に出て、引数エラーなのに終了コードが 1 になっていた)。
	switch opts.engine {
	case "", "mysql", "postgres":
	default:
		fmt.Fprintf(os.Stderr, "sashiki init: --engine %q は未対応です (mysql | postgres)\n", opts.engine)
		return exitUsage
	}
	if opts.platform == "darwin" {
		return cmdInitDarwin(opts)
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "sashiki init: root で実行してください (sudo sashiki init ...)")
		return exitError
	}
	// --pool 省略時は既存 config の pool を使う(#320)。既定の dbpool のまま
	// 既存ホストで再実行すると、AppArmor と sudoers が dbpool 用に書き換わって
	// 動いているブランチ(例: Terraform の tank)の mysqld が拒否される。
	if !opts.poolExplicit {
		if p := existingPoolFromConfig("/etc/sashiki/config.yaml"); p != "" && p != opts.pool {
			fmt.Printf("sashiki init: --pool 省略のため既存 config の pool %q を使います\n", p)
			opts.pool = p
		}
	} else if p := existingPoolFromConfig("/etc/sashiki/config.yaml"); p != "" && p != opts.pool {
		fmt.Fprintf(os.Stderr, "sashiki init: 警告 --pool %s は既存 config の pool %s と違います。"+
			"既存ホストの再実行なら --pool %s(pool 名は zpool list で確認)\n", opts.pool, p, p)
	}
	// app_pass の既定 "dev" は、proxy を 0.0.0.0:3306 で開き lazy create が既定 ON の
	// Linux 構成では「到達できる誰でもブランチを作れる」状態になる(#297)。
	// 明示が無ければランダムに作り、config に書いて 1 回だけ表示する。
	generatedPass := false
	if opts.appPass == "" {
		pass, err := randomPassword()
		if err != nil {
			fmt.Fprintln(os.Stderr, "sashiki init: app_pass 生成:", err)
			return exitError
		}
		opts.appPass = pass
		generatedPass = true
	}

	var steps []initStep
	switch opts.engine {
	case "", "mysql":
		opts.engine = "mysql"
		steps = initSteps(opts)
	case "postgres":
		steps = initStepsPostgres(opts)
	default:
		fmt.Fprintf(os.Stderr, "sashiki init: --engine %q は未対応です (mysql | postgres)\n", opts.engine)
		return exitUsage
	}
	// config が既にあれば app_pass は上書きしない(冪等)。表示も出さない。
	_, statErr := os.Stat("/etc/sashiki/config.yaml")
	configExisted := statErr == nil
	fmt.Printf("sashiki init: engine=%s pool=%s device=%s\n", opts.engine, opts.pool, orDash(opts.device))
	if !opts.yes {
		fmt.Print("続行する? [y/N]: ")
		var ans string
		_, _ = fmt.Scanln(&ans)
		if !strings.HasPrefix(strings.ToLower(ans), "y") {
			fmt.Println("中止しました")
			return exitError
		}
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
	// baseline は mysql / postgres とも `sashiki baseline import` で作れる
	// (postgres は #223 で対応)。手順を engine に依らず同じ形で案内する。
	dump := "prod-dump.sql"
	if opts.engine == "postgres" {
		dump = "prod-dump.sql   # pg_dump のカスタム形式 / ディレクトリも可"
	}
	fmt.Printf(`
init 完了。次のステップ:
  1. ベースデータを投入して baseline を作る(ZFS 操作のため root):
       sudo sashiki baseline import --from %s
  2. sashikid を起動: sudo systemctl enable --now sashikid
  3. ブランチを作る: sashiki create pr-1
`, dump)
	if generatedPass && !configExisted {
		fmt.Printf(`
app パスワード(接続ユーザー dev の -p に使う。/etc/sashiki/config.yaml の app_pass):
  %s
固定したいときは init の前に sashiki init --app-pass <値> で指定する(既存 config は書き換えない)。
`, opts.appPass)
	}
	return exitOK
}

func initSteps(opts initOpts) []initStep {
	steps := []initStep{}

	if !opts.skipPackages {
		steps = append(steps, initStep{
			name: "パッケージ導入 (zfsutils-linux, mysql-server-8.0, mysql-client-8.0)",
			done: func() bool { return cmdOK("zfs", "version") && binExists("/usr/sbin/mysqld") },
			run: func() error {
				return runCmd(envv("DEBIAN_FRONTEND=noninteractive"), "apt-get", "install", "-y", "-q",
					"zfsutils-linux", "mysql-server-8.0", "mysql-client-8.0")
			},
		})
	}

	steps = append(steps,
		initStep{
			name: "既定の mysql サービスを停止・無効化",
			done: func() bool { return !cmdOK("systemctl", "is-enabled", "--quiet", "mysql") },
			run: func() error {
				_ = runCmd(nil, "systemctl", "stop", "mysql")
				return runCmd(nil, "systemctl", "disable", "mysql")
			},
		},
		// zpool と dataset を AppArmor / sudoers より先に確認する(#320)。pool 名を
		// 間違えたとき、プロファイルと sudoers を書き換える前に「pool が無い」で
		// 止まるように。
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
			name: fmt.Sprintf("データセット %s/base (recordsize=16k)", opts.pool),
			done: func() bool { return cmdOK("zfs", "list", opts.pool+"/base") },
			run: func() error {
				return runCmd(nil, "zfs", "create", "-o", "recordsize=16k", "-o", "logbias=throughput", opts.pool+"/base")
			},
		},
		initStep{
			name: fmt.Sprintf("データセット %s/branches", opts.pool),
			done: func() bool { return cmdOK("zfs", "list", opts.pool+"/branches") },
			run:  func() error { return runCmd(nil, "zfs", "create", opts.pool+"/branches") },
		},
		initStep{
			name: "AppArmor: mysqld を datadir に閉じ込める完全プロファイルを生成(enforce)",
			done: func() bool {
				// 内容が最新の生成結果と一致する場合のみスキップ(pool 変更や
				// プロファイル更新時は再生成・再ロードする)
				return fileEqual(apparmorProfilePath, []byte(apparmorProfile(opts.pool)))
			},
			run: func() error {
				// Ubuntu 24.04 の /etc/apparmor.d/usr.sbin.mysqld は空の
				// プレースホルダで local override は no-op のため、sashiki 自前の
				// 完全プロファイルを配布して enforce でロードする(#79)。
				return installApparmorProfile(opts.pool)
			},
		},
		rootHelperConfigStep(opts.pool),
		sudoersStep(opts.pool),
		initStep{
			name: "ディレクトリ作成 (/etc/sashiki, /var/lib/sashiki, /var/log/sashiki)",
			run: func() error {
				for _, d := range []string{"/etc/sashiki/hooks", "/var/lib/sashiki/branches", "/var/log/sashiki/hooks"} {
					if err := os.MkdirAll(d, 0o755); err != nil {
						return err
					}
				}
				// mysqld がエラーログを書けるように
				return runCmd(nil, "chown", "-R", "mysql:mysql", "/var/log/sashiki")
			},
		},
		initStep{
			name: "systemd ユニット mysqld@.service",
			done: func() bool { return fileEqual("/etc/systemd/system/mysqld@.service", mysqldUnit) },
			run: func() error {
				if err := os.WriteFile("/etc/systemd/system/mysqld@.service", mysqldUnit, 0o644); err != nil {
					return err
				}
				return runCmd(nil, "systemctl", "daemon-reload")
			},
		},
		initStep{
			name: "/etc/sashiki/config.yaml 生成",
			done: func() bool { _, err := os.Stat("/etc/sashiki/config.yaml"); return err == nil },
			run: func() error {
				cfg, err := renderConfigApp(opts.pool, opts.appPass)
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

// useRootHelper は sudoers を helper 1 行にしてよいか。config が無い(これから生成
// する。テンプレートは root_helper 付き)か、既存 config に root_helper があるとき。
// 既存 config に無いホストは旧方式のまま(config を書き換えないと sashikid の
// `sudo -n zfs` が拒否される)。UPGRADING の手順で root_helper を足して init を再実行する。
func useRootHelper(configPath string) bool {
	if _, err := os.Stat(configPath); err != nil {
		return true
	}
	return existingRootHelperFromConfig(configPath) != ""
}

// existingRootHelperFromConfig は既存 config の root_helper(無ければ空)。
func existingRootHelperFromConfig(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var c struct {
		RootHelper string `yaml:"root_helper"`
	}
	if yaml.Unmarshal(data, &c) != nil {
		return ""
	}
	return c.RootHelper
}

// rootHelperConfigStep は helper の設定(root 0600)を書く。sudoers より先。
func rootHelperConfigStep(pool string) initStep {
	want := []byte(rootHelperConfigContent(pool))
	return initStep{
		name: "root-helper 設定 (/etc/sashiki/root-helper.yaml)",
		done: func() bool { return fileEqual(rootHelperConfigPath, want) },
		run: func() error {
			if err := os.MkdirAll("/etc/sashiki", 0o755); err != nil {
				return err
			}
			if !binExists(rootHelperBin) {
				fmt.Fprintf(os.Stderr, "sashiki init: 警告: %s が見つかりません(deb / tar.gz に同梱。ソースから入れた場合は go build ./cmd/sashiki-root-helper を置く)\n", rootHelperBin)
			}
			return os.WriteFile(rootHelperConfigPath, want, 0o600)
		},
	}
}

const rootHelperConfigPath = "/etc/sashiki/root-helper.yaml"

// sudoersStep は sudoers を生成する。root-helper 方式か旧方式かは config で決まる。
func sudoersStep(pool string) initStep {
	return initStep{
		name: "sudoers: sashiki ユーザーの root 操作を sashiki-root-helper に限定",
		done: func() bool {
			// 内容が最新の生成結果と一致する場合のみスキップ(旧形式の
			// 緩い sudoers は上書きして絞り直す #78)
			return fileEqual(sudoersPath, []byte(sudoersFor(pool, useRootHelper("/etc/sashiki/config.yaml"))))
		},
		run: func() error {
			useHelper := useRootHelper("/etc/sashiki/config.yaml")
			if !useHelper {
				fmt.Println("  (既存 config に root_helper が無いため旧方式の sudoers を維持。docs/UPGRADING.md の手順で移行できる)")
			}
			// visudo -cf 検証後に本置きする(#78)。
			return installSudoers(pool, useHelper)
		},
	}
}

// existingPoolFromConfig は既存 config の storage.ebs-zfs.pool(旧名 zfs.pool)を
// 返す。無ければ空。config.Load は未知キーで落ちる(#300)ので、アップグレード前の
// config でも読めるよう必要なキーだけ寛容に読む。
func existingPoolFromConfig(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var c struct {
		Storage struct {
			Zfs struct {
				Pool string `yaml:"pool"`
			} `yaml:"ebs-zfs"`
			LegacyZfs struct {
				Pool string `yaml:"pool"`
			} `yaml:"zfs"`
		} `yaml:"storage"`
	}
	if err := yaml.Unmarshal(data, &c); err != nil {
		return ""
	}
	if c.Storage.Zfs.Pool != "" {
		return c.Storage.Zfs.Pool
	}
	return c.Storage.LegacyZfs.Pool
}

func renderConfig(pool string) ([]byte, error) {
	return renderConfigApp(pool, "dev")
}

// renderConfigApp は app_pass を指定して config を生成する(#297)。
func renderConfigApp(pool, appPass string) ([]byte, error) {
	t, err := template.New("config").Parse(configTmpl)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, struct {
		Pool    string
		AppPass string
	}{Pool: pool, AppPass: yamlQuote(appPass)}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeConfigFile は app_pass を含む config を書き、権限を揃える(#295)。
func writeConfigFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return fixConfigPerm(path)
}

// configPerm は config に付けたい権限。sashikid は User=sashiki で動くので
// root:sashiki 0640。sashiki グループが無い環境(ソースから入れて root 直起動)
// では root:root 0600 — other に読ませる理由は無い(#310 review)。
func configPerm() (mode os.FileMode, gid int) {
	g, err := user.LookupGroup("sashiki")
	if err != nil {
		return 0o600, 0
	}
	n, err := strconv.Atoi(g.Gid)
	if err != nil {
		return 0o600, 0
	}
	return 0o640, n
}

// configPermOK は path が configPerm の状態か。
func configPermOK(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	mode, gid := configPerm()
	if st.Mode().Perm() != mode {
		return false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	return int(sys.Uid) == 0 && int(sys.Gid) == gid
}

// fixConfigPerm は既存 config の内容には触れず権限だけ揃える。init を再実行した
// 既存環境(生成ステップは「済み」でスキップされる)にも #295 を効かせるため。
func fixConfigPerm(path string) error {
	mode, gid := configPerm()
	if err := os.Chown(path, 0, gid); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// configPermStep は既存 config の権限を揃える冪等ステップ。
func configPermStep(path string) initStep {
	return initStep{
		name: path + " の権限 (root:sashiki 0640 / 無ければ 0600)",
		done: func() bool { return configPermOK(path) },
		run:  func() error { return fixConfigPerm(path) },
	}
}

// yamlQuote は文字列を YAML の二重引用符リテラルにする。--app-pass に `: ` や
// ` #` や引用符が入っても生成 config が壊れず、読まれる値が表示と一致する
// (#310 review)。
func yamlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// randomPassword は app_pass 用の乱数(hex 32 文字)を返す。YAML でクォート不要な
// 文字だけにする。
func randomPassword() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// --- helpers ---

func cmdOK(name string, args ...string) bool {
	return exec.Command(name, args...).Run() == nil
}

func binExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode()&0o111 != 0
}

func fileEqual(path string, want []byte) bool {
	got, err := os.ReadFile(path)
	return err == nil && bytes.Equal(got, want)
}

func runCmd(env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func envv(kv ...string) []string { return kv }

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// importExistingPool はデバイス上に既存の zpool があれば import して true を返す。
// インスタンスを作り直してもデータ EBS は残る運用(terraform の prevent_destroy)で、
// init が zpool create に進んで止まらないようにするため(#246)。
//
// -f が要るのは「最後に別システムで使われた」pool を取り込むため。インスタンスを
// 差し替えると hostid が変わり、force 無しでは import が拒否される。EBS は 1 台の
// インスタンスにしか attach されないので、他ホストと同時にマウントする事故は起きない。
func importExistingPool(pool, device string) bool {
	// まずデバイスを直接探す。見つからなければ既定の探索パスに任せる
	// (by-id とパーティションの対応が環境で違うため 2 段構え)。
	if runCmd(nil, "zpool", "import", "-f", "-d", device, pool) == nil {
		return true
	}
	return runCmd(nil, "zpool", "import", "-f", pool) == nil
}
