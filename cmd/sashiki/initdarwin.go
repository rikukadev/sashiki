// sashiki init --platform darwin: macOS ネイティブ(VM レス、#119)の一括セットアップ。
// Homebrew mysql を検出し、storage.local.root(APFS)を用意して base datadir を
// 初期化・baseline を clonefile で取得、config を生成、launchd で sashikid を常駐させる。
// root 権限は不要(ログインユーザーで動く)。
package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"
)

//go:embed assets/config.darwin.yaml.tmpl
var configDarwinTmpl string

//go:embed assets/sashikid.plist.tmpl
var plistTmpl string

func cmdInitDarwin(opts initOpts) int {
	// engine で経路を分ける(#238)。postgres は initdb / pg_ctl を使う。
	switch opts.engine {
	case "postgres":
		return cmdInitDarwinPostgres(opts)
	case "", "mysql":
	default:
		fmt.Fprintf(os.Stderr, "sashiki init: --engine %q は未対応です (mysql | postgres)\n", opts.engine)
		return exitError
	}
	mysqldBin, err := resolveMysqld()
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
		root = filepath.Join(home, "Library", "Application Support", "sashiki")
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

	fmt.Printf("sashiki init (darwin): root=%s mysqld=%s\n", root, mysqldBin)
	if !opts.yes {
		fmt.Print("続行する? [y/N]: ")
		var ans string
		_, _ = fmt.Scanln(&ans)
		if len(ans) == 0 || (ans[0] != 'y' && ans[0] != 'Y') {
			fmt.Println("中止しました")
			return exitError
		}
	}
	// APFS 上か確認し、Time Machine の対象から外す(#302)。
	if err := prepareDarwinRoot(root); err != nil {
		fmt.Fprintln(os.Stderr, "sashiki init:", err)
		return exitError
	}

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
			name: "base datadir を初期化 (mysqld --initialize-insecure)",
			done: func() bool { return dirNonEmpty(baseData) },
			run: func() error {
				if err := os.MkdirAll(baseData, 0o755); err != nil {
					return err
				}
				return runOut(mysqldBin, "--no-defaults", "--initialize-insecure", "--datadir="+baseData)
			},
		},
		{
			name: "baseline に app ユーザー(dev)を作成",
			done: func() bool { _, err := os.Stat(baseline); return err == nil },
			run:  func() error { return provisionDevUser(mysqldBin, baseData) },
		},
		{
			name: "baseline snapshot を取得 (clonefile)",
			done: func() bool { _, err := os.Stat(baseline); return err == nil },
			run: func() error {
				// server_uuid の重複を避けるため auto.cnf を消してから clone する
				// (全ブランチが同一 UUID になるのを防ぐ。#80 と同趣旨)。
				if err := os.Remove(filepath.Join(baseData, "auto.cnf")); err != nil && !os.IsNotExist(err) {
					return err
				}
				return runOut("cp", "-Rc", baseData, baseline)
			},
		},
		{
			name: "config.yaml 生成",
			done: func() bool { _, err := os.Stat(configPath); return err == nil },
			run: func() error {
				data, err := renderTmpl(configDarwinTmpl, map[string]string{"Root": root, "MysqldBin": mysqldBin})
				if err != nil {
					return err
				}
				// app_pass を含むので所有者のみ(#295)。
				return os.WriteFile(configPath, data, 0o600)
			},
		},
		{
			name: "launchd に sashikid を登録 (dev.sashiki.sashikid)",
			run: func() error {
				home, _ := os.UserHomeDir()
				agents := filepath.Join(home, "Library", "LaunchAgents")
				if err := os.MkdirAll(agents, 0o755); err != nil {
					return err
				}
				plistPath := filepath.Join(agents, "dev.sashiki.sashikid.plist")
				data, err := renderTmpl(plistTmpl, map[string]string{
					"SashikidBin": sashikidBin, "Config": configPath, "Log": logDir,
				})
				if err != nil {
					return err
				}
				if err := os.WriteFile(plistPath, data, 0o644); err != nil {
					return err
				}
				// 再ロードで冪等に(unload は未ロードでも無害)。
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

	fmt.Printf(`
init 完了 (darwin)。sashikid は launchd で常駐しています。
  接続確認:  sashiki doctor
  ブランチ:  sashiki create pr-1
  停止:      launchctl unload ~/Library/LaunchAgents/dev.sashiki.sashikid.plist

本番相当のデータを入れるなら(root 不要、--config も不要):
  sashiki baseline import --from dump.sql
  (init が作った空の baseline は残り、新しい tag で取って current を切り替える)
`)
	return exitOK
}

// resolveMysqld は Homebrew / PATH から mysqld を探す。app_user は
// caching_sha2_password で作るので 8.0 / 8.4 / 9.x いずれでも動く(#version-compat)。
// 既定の探索順として広く入っている mysql@8.0 を最初に見るだけで、要件ではない。
func resolveMysqld() (string, error) {
	// mysql@8.0 を最優先。
	if out, err := exec.Command("brew", "--prefix", "mysql@8.0").Output(); err == nil {
		p := filepath.Join(trim(out), "bin", "mysqld")
		if binExists(p) {
			return p, nil
		}
	}
	for _, p := range []string{
		"/opt/homebrew/opt/mysql@8.0/bin/mysqld",
		"/usr/local/opt/mysql@8.0/bin/mysqld",
	} {
		if binExists(p) {
			return p, nil
		}
	}
	// 素の mysql(8.4 / 9.x 以降)。app_user は caching_sha2_password で作るので
	// native_password が無くても動く(#197 / #209)。以前ここで出していた
	// 「9.x は通らない」警告は古い(#293)。
	for _, p := range []string{
		"/opt/homebrew/opt/mysql/bin/mysqld",
		"/usr/local/opt/mysql/bin/mysqld",
	} {
		if binExists(p) {
			return p, nil
		}
	}
	if p, err := exec.LookPath("mysqld"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("mysqld が見つかりません。`brew install mysql@8.0` を実行してください")
}

// resolveSashikid は sashikid バイナリを探す(sashiki と同じディレクトリ優先)。
func resolveSashikid() (string, error) {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "sashikid")
		if binExists(p) {
			return p, nil
		}
	}
	if p, err := exec.LookPath("sashikid"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("sashikid が見つかりません(sashiki と同じ場所に置いてください)")
}

// provisionDevUser は base datadir を一時起動して app ユーザー(dev, native_password)
// と app データベースを作り、正常終了する。方式A プロキシは dev/dev で backend に
// 繋ぐため、baseline に dev ユーザーが必要(#119)。socket のみ(--skip-networking)で
// 一時起動するのでポート競合しない。
func provisionDevUser(mysqldBin, baseData string) error {
	sock := "/tmp/sashiki-init.sock"
	pid := filepath.Join(baseData, "init.pid")
	_ = os.Remove(sock)
	_ = os.Remove(pid)
	args := []string{
		"--no-defaults",
		"--datadir=" + baseData,
		"--socket=" + sock,
		"--pid-file=" + pid,
		"--skip-networking", // 一時起動は socket のみ
		"--log-error=" + filepath.Join(baseData, "init.err.log"),
		"--daemonize",
	}
	if out, err := exec.Command(mysqldBin, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("一時 mysqld 起動: %w: %s", err, trim(out))
	}
	defer func() {
		if b, err := os.ReadFile(pid); err == nil {
			if p, e := strconv.Atoi(strings.TrimSpace(string(b))); e == nil {
				_ = syscall.Kill(p, syscall.SIGTERM)
				for i := 0; i < 100; i++ {
					if syscall.Kill(p, 0) != nil {
						break
					}
					time.Sleep(100 * time.Millisecond)
				}
			}
		}
		_ = os.Remove(sock)
	}()

	client := filepath.Join(filepath.Dir(mysqldBin), "mysql")
	var lastErr error
	for i := 0; i < 100; i++ {
		// backend の版でプラグインを選ぶ(8.0+ は caching_sha2、5.7 等は native)。
		// server 未 ready の間は既定 caching_sha2 が返り CREATE も失敗するので retry で回る。
		plugin := authPluginFor(client, sock)
		sql := fmt.Sprintf(
			"CREATE USER IF NOT EXISTS 'dev'@'%%' IDENTIFIED WITH %s BY 'dev';"+
				"GRANT ALL PRIVILEGES ON *.* TO 'dev'@'%%' WITH GRANT OPTION;"+
				"CREATE DATABASE IF NOT EXISTS app;FLUSH PRIVILEGES;", plugin)
		out, err := exec.Command(client, "--socket="+sock, "-uroot", "-e", sql).CombinedOutput()
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("%w: %s", err, trim(out))
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("dev ユーザー作成に失敗: %v", lastErr)
}

func renderTmpl(tmpl string, data any) ([]byte, error) {
	t, err := template.New("t").Parse(tmpl)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func dirNonEmpty(path string) bool {
	ents, err := os.ReadDir(path)
	return err == nil && len(ents) > 0
}

func trim(b []byte) string { return string(bytes.TrimSpace(b)) }

// runOut はコマンドを実行し、失敗時に出力付きエラーを返す。
func runOut(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, trim(out))
	}
	return nil
}
