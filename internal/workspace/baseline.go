// baseline の set / GC(仕様 12-1, 12-2, 12-5)。baseline は main の immutable
// publication。current は pointer にすぎず、publish しても既存 branch の
// origin は変わらない。GC は lineage(どの branch が参照中か)を sashiki が把握し、
// current・参照中は残す。zfs promote は使わない(lineage が追えなくなる)。
package workspace

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/rikukadev/sashiki/internal/state"
	"github.com/rikukadev/sashiki/internal/storage"
)

// PromoteOptions は promote の宣言(#296)。
type PromoteOptions struct {
	// Masked は「このブランチのデータはマスク済み」の宣言。require_masked のとき
	// 無ければ promote を拒否する。refresh の sentinel と違い自動判定はできない
	// (branch 上で何をしたかは sashiki には分からない)ので、operator が明示する。
	Masked bool
	// SkipValidate は _validate での検証を飛ばす。require_validated のときは
	// 飛ばした時点で current にできない。
	SkipValidate bool
}

// PromoteBranch は既存ブランチの現在の datadir を新しい baseline に昇格する(#129)。
// branch でマイグレーション済みの状態をそのまま次の baseline にできる(git の
// branch→main 相当)。snapshot 不変条件のため branch mysqld を graceful stop して
// から snapshot し、完了後に再起動する。既存の他ブランチの origin は変えない。
//
// refresh と同じ publish ポリシーを通す(#296)。以前は無条件に validated=true で
// 登録して即 current にしていたため、require_masked / require_validated の抜け道に
// なっていた。
func (m *Manager) PromoteBranch(ctx context.Context, name string, opts PromoteOptions) (string, error) {
	unlock := m.lock(name)
	defer unlock()
	m.baselineMu.Lock()
	defer m.baselineMu.Unlock()

	pr, ok := m.st.(storage.BranchPromoter)
	if !ok {
		return "", fmt.Errorf("%w: このバックエンドは promote に未対応です", ErrPreconditionFailed)
	}
	rc := m.resolveRefreshConfig(RefreshConfig{})
	// 満たせないポリシーは snapshot を取る前(branch を止める前)に拒否する。
	if rc.RequireMasked && !opts.Masked {
		return "", fmt.Errorf("%w: promote には --masked の宣言が要ります(require_masked)。"+
			"branch %q のデータがマスク済みなら --masked を付けてください", ErrPreconditionFailed, name)
	}
	if rc.RequireValidated && opts.SkipValidate {
		return "", fmt.Errorf("%w: require_validated のため --skip-validate では current にできません", ErrPreconditionFailed)
	}
	b, err := m.db.GetBranch(name)
	if err != nil {
		return "", err
	}
	vol, err := m.resolveVolume(ctx, b)
	if err != nil {
		return "", err
	}
	ins := m.instance(b, vol)

	// 不変条件: snapshot は必ず正常終了状態でのみ取得する。graceful stop する。
	if err := m.eng.Stop(ctx, ins); err != nil {
		return "", fmt.Errorf("promote: branch %s の停止に失敗: %w", name, err)
	}
	// promote は snapshot のために一時停止しただけなので、成否・どの経路で return
	// しても branch は必ず使用可能な状態に戻す(best effort)。以前は登録失敗時に
	// mysqld を停止したまま抜けていた(#156)。
	restarted := false
	restartBranch := func() {
		if restarted {
			return
		}
		restarted = true
		if err := m.eng.Start(ctx, ins); err == nil {
			_ = m.eng.WaitReady(ctx, ins)
		}
	}
	defer restartBranch()

	// server_uuid の重複を避けるため auto.cnf を削除してから snapshot(#80 と同趣旨)。
	_ = os.Remove(filepath.Join(vol.Path, "data", "auto.cnf"))

	tag := newBaselineTag()
	snap, err := pr.PromoteBranch(ctx, vol, tag)
	if err != nil {
		return "", fmt.Errorf("promote snapshot: %w", err)
	}
	prov := state.BaselineProvenance{DataAsOf: tag, Masked: opts.Masked}
	if err := m.db.RegisterBaseline(string(snap), prov); err != nil {
		return "", fmt.Errorf("promote: baseline 登録に失敗: %w", err)
	}
	// snapshot は取れたので branch はここで戻す。検証は candidate の clone
	// (_validate)で行い、branch 自体は使わない。
	restartBranch()

	// refresh と同じ検証: crash recovery なしで起動できること + on-baseline-validate。
	// 落ちたら登録は残す(validated=false のまま)が current にはしない。
	if !opts.SkipValidate {
		if err := m.validateCandidate(ctx, snap, rc); err != nil {
			return "", fmt.Errorf("promote: validate: %w(%s は登録済みだが current にしていない)", err, snap)
		}
		prov.Validated = true
		if err := m.db.RegisterBaseline(string(snap), prov); err != nil {
			return "", fmt.Errorf("promote: validated 記録に失敗: %w", err)
		}
	}
	if err := m.db.SetCurrentBaseline(string(snap)); err != nil {
		return "", fmt.Errorf("promote: current baseline 設定に失敗: %w", err)
	}
	log.Printf("baseline promote: %s → %s (current, masked=%v validated=%v)", name, snap, prov.Masked, prov.Validated)
	return string(snap), nil
}

// SetBaseline は current pointer を指定 snapshot へ切り替える(仕様 12-2)。
// 直前の正常版へ即 rollback するのに使う。snapshot は登録済みである必要がある。
func (m *Manager) SetBaseline(ctx context.Context, snapshot string) error {
	m.baselineMu.Lock()
	defer m.baselineMu.Unlock()
	b, err := m.db.GetBaseline(snapshot)
	if err != nil {
		return fmt.Errorf("baseline %s is not registered: %w", snapshot, ErrBaselineNotFound)
	}
	// publish ポリシー(refresh と同じ)を set にも課す(抜け道を塞ぐ)。
	if m.baselinePolicy.RequireMasked && !b.Prov.Masked {
		return fmt.Errorf("cannot set baseline %s: not masked (require_masked): %w", snapshot, ErrPreconditionFailed)
	}
	if m.baselinePolicy.RequireValidated && !b.Prov.Validated {
		return fmt.Errorf("cannot set baseline %s: not validated (require_validated): %w", snapshot, ErrPreconditionFailed)
	}
	return m.db.SetCurrentBaseline(snapshot)
}

// GCConfig は baseline GC のポリシー。
type GCConfig struct {
	KeepLast  int           // 直近 N 個は残す(0 = 個数では残さない)
	Retention time.Duration // この期間より新しい baseline は残す(0 = 期間では残さない)
	DryRun    bool          // true なら削除せず「削除対象」を Deleted に列挙する
}

// GCResult は GC の結果。
type GCResult struct {
	Deleted []string
	Kept    []string
}

// GCBaselines は GC ポリシーに従って古い baseline snapshot を削除する。
// current・branch から参照中・直近 KeepLast は残す。
func (m *Manager) GCBaselines(ctx context.Context, cfg GCConfig) (GCResult, error) {
	// SetBaseline と直列化して「current にしようとしている baseline を GC が消す」
	// 競合を防ぐ(current の読み取りと削除を同一クリティカルセクションで行う)。
	m.baselineMu.Lock()
	defer m.baselineMu.Unlock()
	baselines, err := m.db.ListBaselines()
	if err != nil {
		return GCResult{}, err
	}
	refs, err := m.db.BaselineRefCounts()
	if err != nil {
		return GCResult{}, err
	}
	current := string(m.currentBaseline())

	// 新しい順(ListBaselines は DESC)。KeepLast 個は無条件で残す。
	sort.SliceStable(baselines, func(i, j int) bool {
		return baselines[i].CreatedAt.After(baselines[j].CreatedAt)
	})

	var res GCResult
	for i, b := range baselines {
		keep := false
		switch {
		case b.Snapshot == current:
			keep = true // current pointer
		case b.IsCurrent:
			keep = true
		case refs[b.Snapshot] > 0:
			keep = true // branch が参照中
		case cfg.KeepLast > 0 && i < cfg.KeepLast:
			keep = true // 直近 N
		case cfg.Retention > 0 && time.Since(b.CreatedAt) < cfg.Retention:
			keep = true // retention より新しい
		}
		if keep {
			res.Kept = append(res.Kept, b.Snapshot)
			continue
		}
		// dry-run: 実際には消さず、削除対象として列挙する。
		if cfg.DryRun {
			res.Deleted = append(res.Deleted, b.Snapshot)
			continue
		}
		// storage から snapshot を削除(DeleteBaselineSnapshot を持つバックエンドのみ)。
		if bd, ok := m.st.(baselineDeleter); ok {
			if err := bd.DeleteBaselineSnapshot(ctx, storage.SnapshotRef(b.Snapshot)); err != nil {
				log.Printf("baseline gc: delete %s: %v", b.Snapshot, err)
				res.Kept = append(res.Kept, b.Snapshot)
				continue
			}
		}
		if err := m.db.DeleteBaseline(b.Snapshot); err != nil {
			return res, err
		}
		res.Deleted = append(res.Deleted, b.Snapshot)
	}
	return res, nil
}

// baselineDeleter は baseline snapshot を削除できるバックエンド。
type baselineDeleter interface {
	DeleteBaselineSnapshot(ctx context.Context, snap storage.SnapshotRef) error
}

// DeleteBaseline は指定 candidate baseline を削除する(#84)。current・branch から
// 参照中は 412(ErrPreconditionFailed)で拒否する。
func (m *Manager) DeleteBaseline(ctx context.Context, snapshot string) error {
	m.baselineMu.Lock()
	defer m.baselineMu.Unlock()
	if _, err := m.db.GetBaseline(snapshot); err != nil {
		return fmt.Errorf("%w: %s", ErrBaselineNotFound, snapshot)
	}
	if string(m.currentBaseline()) == snapshot {
		return fmt.Errorf("%w: baseline %s is current", ErrPreconditionFailed, snapshot)
	}
	refs, err := m.db.BaselineRefCounts()
	if err != nil {
		return err
	}
	if refs[snapshot] > 0 {
		return fmt.Errorf("%w: baseline %s is referenced by %d branch(es)", ErrPreconditionFailed, snapshot, refs[snapshot])
	}
	if bd, ok := m.st.(baselineDeleter); ok {
		if err := bd.DeleteBaselineSnapshot(ctx, storage.SnapshotRef(snapshot)); err != nil {
			return err
		}
	}
	return m.db.DeleteBaseline(snapshot)
}

// ListBaselineRows は API 用に provenance 付きで baseline を返す。
func (m *Manager) ListBaselineRows() ([]state.BaselineRow, error) {
	return m.db.ListBaselines()
}
