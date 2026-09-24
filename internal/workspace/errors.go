// error モデル(仕様 11-1): 失敗を「調べられて次の一手が分かる」形にする。
package workspace

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/rikukadev/sashiki/internal/hooks"
	"github.com/rikukadev/sashiki/internal/state"
	"github.com/rikukadev/sashiki/internal/storage"
)

// エラーコード(仕様 11-1)。
const (
	CodeCloneFailed   = "clone_failed"
	CodeEngineFailed  = "engine_failed"
	CodeHookFailed    = "hook_failed"
	CodeSnapshotError = "snapshot_error"
	CodeStorageError  = "storage_error"
)

// classify は err と失敗した段階から診断情報を返す。
func classify(stage string, err error) (code string, recoverable bool, suggestions []string) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "hook"):
		return CodeHookFailed, true, []string{
			"フックのログを確認: /var/log/sashiki/hooks/<branch>-*.log",
			"フックを修正して `sashiki retry <branch>` で再実行",
		}
	case stage == "clone":
		return CodeCloneFailed, true, []string{
			"zpool の空き容量を確認: `sashiki capacity`",
			"baseline が存在するか確認: `sashiki baseline list`",
			"`sashiki retry <branch>` で再実行",
		}
	case stage == "engine-start", stage == "engine-ready":
		return CodeEngineFailed, true, []string{
			"mysqld のエラーログを確認: /var/log/sashiki/<branch>.err",
			"`sashiki retry <branch>` で再実行",
		}
	case stage == "snapshot":
		return CodeSnapshotError, false, []string{
			"datadir を掴んだままのプロセスが無いか確認",
		}
	case stage == "rollback", stage == "stash", stage == "quota", stage == "stop", stage == "engine-stop", stage == "storage":
		// reset / recreate の途中段階(#292)。どれも作業をやり直せば整う:
		// rollback は冪等、stash(rename 退避)は失敗しても旧が残る、stop は
		// retry 時に Kill する。
		return CodeStorageError, true, []string{
			"storage のログを確認(zfs: journalctl -u sashikid)",
			"`sashiki retry <branch>` で再実行",
		}
	default:
		return CodeStorageError, false, []string{"ログを確認して手動対応が必要"}
	}
}

// failOp は error 状態と診断を記録し、元エラーを返す。
func (m *Manager) failOp(name, op, stage string, err error) error {
	code, recoverable, suggestions := classify(stage, err)
	_ = m.db.SetError(name, op, code, recoverable, err.Error(), suggestions)
	// 失敗ステージを構造化ログに残す(#284)。operation_id は ops の完了ログに付く。
	slog.Error("operation stage failed", "branch", name, "failed_operation", op, "stage", stage,
		"error_code", code, "recoverable", recoverable, "error", err.Error())
	return err
}

// Retry は failed_operation を再実行する(仕様 11-1)。自動 retry はしない。
func (m *Manager) Retry(ctx context.Context, name string) (Info, error) {
	b, err := m.db.GetBranch(name)
	if err != nil {
		return Info{}, err
	}
	if b.State != state.StateError {
		return Info{}, fmt.Errorf("branch %s is %s (not in error state)", name, b.State)
	}
	if !b.Recoverable {
		return Info{}, fmt.Errorf("branch %s error is not recoverable (%s); inspect and delete manually", name, b.ErrorCode)
	}
	switch b.FailedOp {
	case "reset":
		return m.Reset(ctx, name)
	case "recreate":
		return m.Recreate(ctx, name)
	case "wake":
		return m.Wake(ctx, name)
	case "create":
		// 失敗した create は残骸を掃除して origin から作り直す(recreate 相当)。
		// Reset / Recreate と同じく branch lock と promote-guard を通す(#322:
		// 以前は直呼びで、reaper の Delete と並走したり、promote 済みの実体を
		// destroy -r できた)。
		return m.retryCreate(ctx, name)
	default:
		return Info{}, fmt.Errorf("cannot retry operation %q", b.FailedOp)
	}
}

// retryCreate は create の失敗をやり直す。lock 下で状態を読み直し、promote 元なら拒否する。
func (m *Manager) retryCreate(ctx context.Context, name string) (Info, error) {
	unlock := m.lock(name)
	defer unlock()
	b, err := m.db.GetBranch(name)
	if err != nil {
		return Info{}, err
	}
	if b.State != state.StateError {
		return Info{}, fmt.Errorf("branch %s is %s (not in error state)", name, b.State)
	}
	if vol, verr := m.resolveVolume(ctx, b); verr == nil {
		if err := m.refuseIfBackingBaseline(name, vol, "retry"); err != nil {
			return Info{}, err
		}
	}
	return m.recreateFrom(ctx, b, storage.SnapshotRef(b.OriginSnapshot), hooks.OnCreate, "create")
}

// RunHookManually は指定 event の hook だけを再実行する(仕様 11-1: sashiki hooks run)。
func (m *Manager) RunHookManually(ctx context.Context, name string, event hooks.Event) error {
	b, err := m.db.GetBranch(name)
	if err != nil {
		return err
	}
	vol, err := m.resolveVolume(ctx, b)
	if err != nil {
		return err
	}
	return m.runHook(ctx, event, b, vol)
}
