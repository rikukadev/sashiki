// Package hooks は on-create / on-reset / on-delete フックの実行(仕様 14-2)。
// 自社固有処理(マイグレーション適用など)をコアの外に追い出すための仕組み。
package hooks

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Event はフックの種類。
type Event string

// フックイベント。
const (
	OnCreate           Event = "on-create"
	OnRecreate         Event = "on-recreate"
	OnReset            Event = "on-reset"
	OnDelete           Event = "on-delete"
	OnBaselineValidate Event = "on-baseline-validate"
)

// Env はフックに環境変数で渡す情報。
type Env struct {
	Branch         string
	Port           int
	Socket         string
	DataDir        string
	EngineType     string
	AdminUser      string
	OriginSnapshot string
	StateDir       string
	// provenance(仕様 11-2 / #81): core は source の中身を解釈せず、
	// adapter が書いた値をそのまま hook へ素通しする。
	SourceJSON             string // branch の source(opaque JSON)。空なら環境変数自体を設定しない
	Owner                  string
	Purpose                string
	Profile                string
	BaselineSchemaRevision string // origin baseline の schema_revision。空なら環境変数自体を設定しない
}

// Result はフック実行結果。
type Result struct {
	Ran      bool // フックが存在して実行された
	ExitCode int
	LogPath  string
}

// Runner はフックを見つけて実行する。
type Runner struct {
	Dir     string        // /etc/sashiki/hooks
	LogDir  string        // /var/log/sashiki/hooks
	Timeout time.Duration // 既定 10 分
	// StripEnv は hook に渡さない環境変数名(#295)。hook は sashikid の環境を
	// 継承する(PATH や proxy 設定、migrate に要る変数を引き継ぐため)が、
	// API トークンのような sashikid 自身の秘密まで見せる理由は無い。
	StripEnv []string
	// LogRetention より古い hook ログは次の実行時に消す(既定 30 日、0 で無効)。
	LogRetention time.Duration
	// now はテストで固定するための時計。
	now func() time.Time
}

// NewRunner は Runner を作る。
func NewRunner(dir, logDir string, timeout time.Duration) *Runner {
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	return &Runner{
		Dir: dir, LogDir: logDir, Timeout: timeout, now: time.Now,
		StripEnv:     []string{"SASHIKI_API_TOKEN"},
		LogRetention: 30 * 24 * time.Hour,
	}
}

// BaseEnv は hook に渡す土台の環境(sashikid の環境から StripEnv を除いたもの)。
// baseline の refresh スクリプトなど、Runner.Run を通らずに外部コマンドを起動する
// 経路も同じものを使う(#321: refresh_script に API トークンが渡っていた)。
func (r *Runner) BaseEnv() []string { return r.inheritedEnv() }

// inheritedEnv は sashikid の環境から StripEnv を除いたものを返す。
func (r *Runner) inheritedEnv() []string {
	strip := map[string]bool{}
	for _, k := range r.StripEnv {
		if k != "" {
			strip[k] = true
		}
	}
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strip[k] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// pruneLogs は LogRetention より古い hook ログを消す(best-effort、#295)。
// ローテーションが無いと create のたびに増え続ける。
func (r *Runner) pruneLogs() {
	if r.LogRetention <= 0 {
		return
	}
	entries, err := os.ReadDir(r.LogDir)
	if err != nil {
		return
	}
	cutoff := r.now().Add(-r.LogRetention)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(r.LogDir, e.Name()))
		}
	}
}

// Find はイベント名に一致する実行可能ファイルを探す(拡張子は見ない)。
func (r *Runner) Find(event Event) (string, bool) {
	return r.find(event)
}

// find はイベント名に一致する実行可能ファイルを探す(拡張子は見ない)。
func (r *Runner) find(event Event) (string, bool) {
	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		base := name
		if ext := filepath.Ext(name); ext != "" {
			base = name[:len(name)-len(ext)]
		}
		if base != string(event) && name != string(event) {
			continue
		}
		p := filepath.Join(r.Dir, name)
		if info, err := os.Stat(p); err == nil && info.Mode()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

// Run はフックを実行する。フックが無ければ Ran=false で成功扱い。
// stdout/stderr は LogDir に保存する。
func (r *Runner) Run(ctx context.Context, event Event, env Env) (Result, error) {
	path, ok := r.find(event)
	if !ok {
		return Result{Ran: false, ExitCode: 0}, nil
	}

	logPath := filepath.Join(r.LogDir,
		fmt.Sprintf("%s-%s-%s.log", env.Branch, event, r.now().UTC().Format("20060102T150405Z")))
	if err := os.MkdirAll(r.LogDir, 0o755); err != nil {
		return Result{}, err
	}
	r.pruneLogs()
	logFile, err := os.Create(logPath)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = logFile.Close() }()

	cctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, path)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = append(r.inheritedEnv(),
		"SASHIKI_EVENT="+string(event),
		"SASHIKI_BRANCH="+env.Branch,
		fmt.Sprintf("SASHIKI_PORT=%d", env.Port),
		"SASHIKI_SOCKET="+env.Socket,
		"SASHIKI_DATADIR="+env.DataDir,
		"SASHIKI_ENGINE="+env.EngineType,
		"SASHIKI_ADMIN_USER="+env.AdminUser,
		"SASHIKI_ORIGIN_SNAPSHOT="+env.OriginSnapshot,
		"SASHIKI_STATE_DIR="+env.StateDir,
		"SASHIKI_OWNER="+env.Owner,
		"SASHIKI_PURPOSE="+env.Purpose,
		"SASHIKI_PROFILE="+env.Profile,
	)
	// source_json / schema_revision は「値が無い」と「空文字」を hook 側で
	// 区別できるように、値があるときだけ設定する(#81)。
	if env.SourceJSON != "" {
		cmd.Env = append(cmd.Env, "SASHIKI_SOURCE_JSON="+env.SourceJSON)
	}
	if env.BaselineSchemaRevision != "" {
		cmd.Env = append(cmd.Env, "SASHIKI_BASELINE_SCHEMA_REVISION="+env.BaselineSchemaRevision)
	}

	runErr := cmd.Run()
	res := Result{Ran: true, LogPath: logPath}
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			return res, fmt.Errorf("hook %s exited %d (log: %s)", event, res.ExitCode, logPath)
		}
		res.ExitCode = -1
		return res, fmt.Errorf("hook %s: %w (log: %s)", event, runErr, logPath)
	}
	return res, nil
}
