// Package postgres は PostgreSQL の engine.Engine 実装。
// systemd テンプレートユニット(postgres-sashiki@<branch>)でブランチごとの
// postgres を起動する。接続は直接ポート(sashiki show <name> で確認)のほか、
// listen.proxy を設定すれば internal/pgproxy の固定エンドポイント経由でも
// できる(`<user>@<branch>` でルーティング + lazy create、#222)。
//
// リモート接続する場合は engine.postgres.listen_addresses を "*" 等に広げ、
// かつ base の pg_hba.conf にクライアント側ネットワークの host 行が必要
// (pg_hba はブランチにクローンされるので base に入れておく)。
//
// storage 側の注意: Postgres のページサイズは 8KB のため、base データセットと
// branch_parent(クローンは名前空間上の親からプロパティを継承する)は
// recordsize=8k で作るのが望ましい(zfs backend の設定で変更可能)。
package postgres

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rikukadev/sashiki/internal/engine"
)

// Config は postgres エンジンの設定。
type Config struct {
	EnvDir          string // /etc/sashiki
	UnitTemplate    string // 既定 "postgres-sashiki" → postgres-sashiki@<branch>.service
	BinDir          string // 既定 /usr/lib/postgresql/16/bin
	ListenAddresses string // 既定 127.0.0.1。リモート接続を許すなら "*" 等
	ReadyTimeout    time.Duration
	Sudo            bool
	// RootHelper は sashiki-root-helper のパス(#276)。設定時は systemctl を
	// `sudo -n <helper> systemctl ...` で呼ぶ(Sudo より優先)。
	RootHelper string

	// Mode は起動方式。"systemd"(既定)は systemd テンプレートユニット、
	// "process" は pg_ctl で直接起動する(systemd の無い macOS ネイティブ /
	// コンテナ向け、#227)。
	Mode string
	// RunUser は root で動かすときに降格する OS ユーザー(既定 "postgres")。
	// postgres は root では起動を拒むため。root でなければ無視する。
	RunUser string
	// SharedBuffers は process モードで postgres に渡す shared_buffers。
	SharedBuffers string
	// LogDir は process モードのサーバログの置き場(既定 os.TempDir())。
	// datadir 配下に置くと snapshot に入ってしまうので外に出す。
	LogDir string
	// AppUser / AppPass は ConnCount が pg_stat_activity を読むときの接続ロール
	// (baseline import が作る app ロール、既定 dev)。initdb は
	// --auth-host=scram-sha-256 なので、パスワード無しの postgres ロールでは
	// 127.0.0.1 に繋げず、idle 停止が一度も発火しなかった(#291)。
	AppUser string
	AppPass string
	// StopTimeout は process モードの graceful stop を待つ時間(既定 10 分、#302)。
	StopTimeout time.Duration
}

const (
	ModeSystemd = "systemd"
	ModeProcess = "process"
)

// Engine は engine.Engine の PostgreSQL + systemd 実装。
type Engine struct {
	cfg Config
	run func(ctx context.Context, name string, args ...string) (string, error)
}

// New は PostgreSQL エンジンを作る。
func New(cfg Config) *Engine {
	if cfg.UnitTemplate == "" {
		cfg.UnitTemplate = "postgres-sashiki"
	}
	if cfg.BinDir == "" {
		cfg.BinDir = "/usr/lib/postgresql/16/bin"
	}
	if cfg.ListenAddresses == "" {
		cfg.ListenAddresses = "127.0.0.1"
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 30 * time.Second
	}
	if cfg.AppUser == "" {
		cfg.AppUser = "dev"
	}
	if cfg.StopTimeout == 0 {
		cfg.StopTimeout = 10 * time.Minute
	}
	e := &Engine{cfg: cfg}
	e.run = e.execCmd
	return e
}

func (e *Engine) execCmd(ctx context.Context, name string, args ...string) (string, error) {
	var cmd *exec.Cmd
	switch {
	case e.cfg.RootHelper != "":
		// root-helper 経由(#276)。name は systemctl で、helper が unit を検証する。
		cmd = exec.CommandContext(ctx, "sudo", append([]string{"-n", e.cfg.RootHelper, name}, args...)...)
	case e.cfg.Sudo:
		cmd = exec.CommandContext(ctx, "sudo", append([]string{"-n", name}, args...)...)
	default:
		cmd = exec.CommandContext(ctx, name, args...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (e *Engine) unit(branch string) string {
	return fmt.Sprintf("%s@%s", e.cfg.UnitTemplate, branch)
}

// Start はインスタンスを起動する(mode により systemd / pg_ctl 直起動)。
func (e *Engine) Start(ctx context.Context, ins engine.Instance) error {
	if e.cfg.Mode == ModeProcess {
		return e.startProcess(ctx, ins)
	}
	// env_dir を確保する(systemd の RuntimeDirectory が無い root 直起動でも動くように、#177)。
	if err := os.MkdirAll(e.cfg.EnvDir, 0o755); err != nil {
		return fmt.Errorf("ensure env_dir %s: %w", e.cfg.EnvDir, err)
	}
	// SHARED_BUFFERS も渡す。以前は process モードだけが shared_buffers を効かせ、
	// systemd unit は postgres 既定のままだった(#299)。
	env := fmt.Sprintf("PORT=%d\nDATADIR=%s\nPGBIN=%s\nLISTEN_ADDRESSES=%s\nSHARED_BUFFERS=%s\n",
		ins.Port, ins.DataDir, e.cfg.BinDir, e.cfg.ListenAddresses, pgSize(e.cfg.SharedBuffers))
	if err := os.WriteFile(filepath.Join(e.cfg.EnvDir, ins.Branch+".env"), []byte(env), 0o644); err != nil {
		return fmt.Errorf("write env: %w", err)
	}
	_, err := e.run(ctx, "systemctl", "start", e.unit(ins.Branch))
	return err
}

// Stop は正常終了(graceful)させる。snapshot の一貫性はこれに依存する。
func (e *Engine) Stop(ctx context.Context, ins engine.Instance) error {
	if e.cfg.Mode == ModeProcess {
		return e.stopProcess(ctx, ins)
	}
	if _, err := e.run(ctx, "systemctl", "stop", e.unit(ins.Branch)); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(e.cfg.EnvDir, ins.Branch+".env"))
	return nil
}

// Kill は即時停止(immediate)。dirty state を捨てる rollback 用。
func (e *Engine) Kill(ctx context.Context, ins engine.Instance) error {
	if e.cfg.Mode == ModeProcess {
		return e.killProcess(ctx, ins)
	}
	_, _ = e.run(ctx, "systemctl", "kill", "-s", "SIGKILL", e.unit(ins.Branch))
	_, _ = e.run(ctx, "systemctl", "stop", e.unit(ins.Branch))
	_ = os.Remove(filepath.Join(e.cfg.EnvDir, ins.Branch+".env"))
	return nil
}

// WaitReady は pg_isready が通るまで待つ。
func (e *Engine) WaitReady(ctx context.Context, ins engine.Instance) error {
	deadline := time.Now().Add(e.cfg.ReadyTimeout)
	bin := filepath.Join(e.cfg.BinDir, "pg_isready")
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, bin, "-h", "127.0.0.1", "-p", fmt.Sprintf("%d", ins.Port))
		if err := cmd.Run(); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout: postgres@%s (port %d) did not become ready in %s",
		ins.Branch, ins.Port, e.cfg.ReadyTimeout)
}

// IsRunning は起動中かどうか(mode により systemctl / pidfile)。
func (e *Engine) IsRunning(ctx context.Context, ins engine.Instance) (bool, error) {
	if e.cfg.Mode == ModeProcess {
		return e.isRunningProcess(ins), nil
	}
	out, _ := e.run(ctx, "systemctl", "is-active", e.unit(ins.Branch))
	return out == "active", nil
}

// ConnCount は現在のクライアント接続数を返す(engine.ConnCounter, #41)。
// pg_stat_activity から client backend を数え、自分(このポーラ)の接続は除く。
// app ロール(dev)で 127.0.0.1 に繋ぐ。baseline import が initdb を
// --auth-host=scram-sha-256 で作り、app ロールにパスワードを持たせるので、
// 追加の pg_hba 設定なしで通る(#291)。-w でプロンプトを禁じ、失敗時は
// ポーラ側で「判定不能=使用中」として保護する。
func (e *Engine) ConnCount(ctx context.Context, ins engine.Instance) (int, error) {
	args := e.connCountArgs(ins)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = e.clientEnv()
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("psql pg_stat_activity (port %d): %w", ins.Port, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("parse count %q: %w", strings.TrimSpace(string(out)), err)
	}
	return n, nil
}

// connCountArgs は ConnCount が実行する psql のコマンドライン。
func (e *Engine) connCountArgs(ins engine.Instance) []string {
	return []string{
		filepath.Join(e.cfg.BinDir, "psql"),
		"-h", "127.0.0.1", "-p", strconv.Itoa(ins.Port),
		"-U", e.cfg.AppUser, "-d", "postgres", "-w", "-tAc",
		"SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()",
	}
}

// clientEnv は psql に渡す環境。パスワードは argv でなく PGPASSWORD で渡す。
func (e *Engine) clientEnv() []string {
	return append(os.Environ(), "PGPASSWORD="+e.cfg.AppPass)
}
