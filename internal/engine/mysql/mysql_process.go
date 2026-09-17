// process モード: systemd の無い環境(macOS ネイティブ / コンテナ、#113)で
// mysqld を直接 spawn して管理する。pidfile ベースでライフサイクルを追う。
// snapshot の一貫性のため、Stop は SIGTERM(mysqld にとって graceful shutdown)。
package mysql

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rikukadev/sashiki/internal/engine"
)

func (e *Engine) pidPath(ins engine.Instance) string {
	return filepath.Join(ins.DataDir, "mysqld.pid")
}

func (e *Engine) socketPath(ins engine.Instance) string {
	// Unix ドメインソケットのパスは ~103 byte 制限がある。datadir が深い場所
	// (例: ~/Library/Application Support/... 配下)だと超えて mysqld が起動
	// できないため、短い固定ディレクトリに port ごとの名前で置く。クライアントは
	// TCP(127.0.0.1:port)で繋ぐのでソケットの位置は問わない。
	return filepath.Join("/tmp", fmt.Sprintf("sashiki-%d.sock", ins.Port))
}

// startArgs は process モードで mysqld に渡す引数を組む(純粋関数、テスト用)。
func (e *Engine) startArgs(ins engine.Instance) []string {
	// 先頭は defaults の扱い。ExtraCnf 指定時は **その 1 ファイルだけ**を読む
	// (--defaults-file)。--defaults-extra-file だと /etc/my.cnf や
	// /opt/homebrew/etc/my.cnf も追加で読み、意図しない設定が混ざる(#126)。
	// 無ければ何も読まない(--no-defaults)。どちらも「先頭必須」オプションなので
	// 必ず args[0] に置く。後続の datadir/port 等が値を上書きする。
	first := "--no-defaults"
	if e.cfg.ExtraCnf != "" {
		first = "--defaults-file=" + e.cfg.ExtraCnf
	}
	args := []string{
		first,
		"--datadir=" + ins.DataDir,
		fmt.Sprintf("--port=%d", ins.Port),
		"--socket=" + e.socketPath(ins),
		"--pid-file=" + e.pidPath(ins),
		"--log-error=" + filepath.Join(ins.DataDir, "error.log"),
		"--bind-address=127.0.0.1",
		"--mysqlx=OFF", // X protocol の追加ポート衝突を避ける
	}
	// --no-defaults だと mysqld 既定(128M)になり、config の buffer_pool_size が
	// 効かなかった(#299)。extra_cnf 側で指定したいときは buffer_pool_size を空に。
	if e.cfg.BufferPoolBytes > 0 {
		args = append(args, fmt.Sprintf("--innodb-buffer-pool-size=%d", e.cfg.BufferPoolBytes))
	}
	if os.Geteuid() == 0 {
		args = append(args, "--user="+e.cfg.RunUser)
	}
	// --daemonize: 親 mysqld が fork 後 exit(0) するので Run() で起動完了を待てる。
	args = append(args, "--daemonize")
	return args
}

// startProcess は mysqld を直接 spawn する(--daemonize で自己 daemon 化)。
func (e *Engine) startProcess(ctx context.Context, ins engine.Instance) error {
	_ = os.Remove(e.pidPath(ins)) // 古い pidfile が起動を妨げないよう掃除
	// ExtraCnf を「唯一の設定源」にするため、baseline 由来の PERSIST 残骸
	// (mysqld-auto.cnf)を除去する。PERSIST は option file より優先されるため、
	// 残っていると extra_cnf を上書きしてしまう(#126)。
	if e.cfg.ExtraCnf != "" {
		_ = os.Remove(filepath.Join(ins.DataDir, "mysqld-auto.cnf"))
	}
	if _, err := e.run(ctx, e.cfg.MysqldBin, e.startArgs(ins)...); err != nil {
		return fmt.Errorf("start mysqld (process mode): %w", err)
	}
	return nil
}

// stopProcess は SIGTERM で graceful shutdown する(clean → snapshot 一貫性)。
func (e *Engine) stopProcess(ctx context.Context, ins engine.Instance) error {
	pid, ok := e.readPid(ins)
	if !ok {
		return nil // 既に居ない
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	if e.waitGone(ins, e.cfg.ReadyTimeout) {
		_ = os.Remove(e.pidPath(ins))
		return nil
	}
	return fmt.Errorf("stop mysqld@%s (pid %d): did not shut down in %s", ins.Branch, pid, e.cfg.ReadyTimeout)
}

// killProcess は SIGKILL で即時停止する(rollback で捨てる dirty state 用)。
func (e *Engine) killProcess(ctx context.Context, ins engine.Instance) error {
	if pid, ok := e.readPid(ins); ok {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		e.waitGone(ins, 5*time.Second)
	}
	_ = os.Remove(e.pidPath(ins))
	// SIGKILL では mysqld が後始末しないため、socket とその lock ファイルが残る。
	// lock には旧 pid が書かれており、PID 1 が刈り取る前(ゾンビ中)に再 Start すると
	// 新 mysqld が「pid X が socket 使用中」と誤認して起動拒否する(MY-010259)。
	// 強制停止時点で lock は必ず stale なので消してよい。
	sock := e.socketPath(ins)
	_ = os.Remove(sock)
	_ = os.Remove(sock + ".lock")
	return nil
}

// isRunningProcess は pidfile の pid が生きているかで判定する。
func (e *Engine) isRunningProcess(ins engine.Instance) bool {
	pid, ok := e.readPid(ins)
	if !ok {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// readPid は pidfile から pid を読む。
func (e *Engine) readPid(ins engine.Instance) (int, bool) {
	b, err := os.ReadFile(e.pidPath(ins))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// waitGone はプロセスが消えるまで待つ(消えたら true)。
func (e *Engine) waitGone(ins engine.Instance, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !e.isRunningProcess(ins) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !e.isRunningProcess(ins)
}
