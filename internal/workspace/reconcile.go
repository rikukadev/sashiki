// reconciliation / doctor / orphan GC(仕様 20-1, 20-2)。
// sashikid 再起動後、state.db / ZFS dataset / mysqld プロセス / systemd unit は
// 食い違い得る。起動時に必ず突き合わせる。
package workspace

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/rikukadev/sashiki/internal/engine"
	"github.com/rikukadev/sashiki/internal/state"
	"github.com/rikukadev/sashiki/internal/storage"
)

// ReconcileReport は突合結果。
type ReconcileReport struct {
	Demoted     []string // running → sleeping(プロセス死亡・volume 健全)
	Errored     []string // dataset 消失など
	Orphans     []string // dataset だけあって state.db に無い branch 名
	Interrupted []string // creating/resetting/deleting のまま再起動で中断された残骸を回収
}

// Reconcile は state.db を ZFS / engine と突き合わせて整合させる(起動時に呼ぶ)。
// 中断された遷移の回収・error 付与・sleeping への降格まで書き込む。
func (m *Manager) Reconcile(ctx context.Context) (ReconcileReport, error) {
	return m.reconcile(ctx, true)
}

// inspect は Reconcile と同じ突き合わせを書き込み無しで行う(doctor / gc 用、#303)。
// 以前は doctor / gc --orphans も Reconcile を呼んでいて、実行中の create / reset を
// 「再起動で中断された」とみなして error にし、deleting の Delete を再開していた。
// 起動時と違い、稼働中は creating / resetting / deleting が正当に存在する。
func (m *Manager) inspect(ctx context.Context) (ReconcileReport, error) {
	return m.reconcile(ctx, false)
}

func (m *Manager) reconcile(ctx context.Context, mutate bool) (ReconcileReport, error) {
	var rep ReconcileReport
	branches, err := m.db.ListBranches()
	if err != nil {
		return rep, err
	}
	known := map[string]bool{}
	for _, b := range branches {
		known[b.Name] = true

		// 遷移中状態(creating/resetting/deleting)で残っている = 再起動で中断された
		// 残骸。sashikid は単一プロセスなので起動時に正当な mid-operation は無い(#53
		// は operation テーブルの回収。ここは branch state を回収する)。
		if !mutate {
			switch b.State {
			case state.StateDeleting, state.StateCreating, state.StateResetting:
				continue // 稼働中の正当な遷移。触らない
			}
		}
		switch b.State {
		case state.StateDeleting:
			// 削除の途中で落ちた → 削除を完了させる(volume 欠損でも Delete が行を掃除する)。
			if err := m.Delete(ctx, b.Name); err != nil {
				log.Printf("reconcile: resume delete %s: %v", b.Name, err)
				rep.Errored = append(rep.Errored, b.Name)
			} else {
				rep.Interrupted = append(rep.Interrupted, b.Name)
			}
			continue
		case state.StateCreating, state.StateResetting:
			// error(recoverable)にして `sashiki retry` で再駆動できるようにする。
			// FailedOp は元操作にあわせる(retry がこの値で分岐する)。
			op := "create"
			if b.State == state.StateResetting {
				op = "reset"
			}
			_ = m.db.SetError(b.Name, op, "interrupted", true,
				op+" が再起動で中断されました",
				[]string{"`sashiki retry " + b.Name + "` で再実行", "または `sashiki delete " + b.Name + "`"})
			rep.Interrupted = append(rep.Interrupted, b.Name)
			continue
		}

		vol, verr := m.resolveVolume(ctx, b)
		if verr != nil {
			// dataset が無い → recoverable=false の error(手動で消された等)
			if mutate {
				_ = m.db.SetError(b.Name, "reconcile", "dataset_missing", false,
					"dataset not found for branch", []string{"`sashiki delete " + b.Name + "` で行を掃除"})
			}
			rep.Errored = append(rep.Errored, b.Name)
			continue
		}
		// running なのにプロセスが死んでいる → sleeping(volume は健全)
		if b.State == state.StateRunning {
			if running, _ := m.eng.IsRunning(ctx, m.instance(b, vol)); !running {
				if mutate {
					_ = m.db.SetState(b.Name, state.StateSleeping, "")
				}
				rep.Demoted = append(rep.Demoted, b.Name)
			}
		}
	}
	// dataset だけあって state.db に無い → orphan
	if vl, ok := m.st.(storage.VolumeLister); ok {
		vols, verr := vl.ListBranchVolumes(ctx)
		if verr == nil {
			for _, name := range vols {
				// 予約名(_validate 等)は orphan 扱いしない(state.db 非登録でも正当)。
				if IsReserved(name) {
					continue
				}
				if !known[name] {
					rep.Orphans = append(rep.Orphans, name)
				}
			}
		}
	}
	if mutate && len(rep.Demoted)+len(rep.Errored)+len(rep.Orphans)+len(rep.Interrupted) > 0 {
		log.Printf("reconcile: demoted=%v errored=%v orphans=%v interrupted=%v",
			rep.Demoted, rep.Errored, rep.Orphans, rep.Interrupted)
	}
	return rep, nil
}

// GCOrphans は state.db に無い dataset(orphan)を削除する(sashiki gc --orphans)。
func (m *Manager) GCOrphans(ctx context.Context) ([]string, error) {
	rep, err := m.inspect(ctx)
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, name := range rep.Orphans {
		vol, verr := m.resolveVolume(ctx, state.Branch{Name: name})
		if verr != nil {
			continue
		}
		if job, derr := m.st.DeleteAsync(ctx, vol); derr == nil {
			_, _ = m.st.Poll(ctx, job)
			deleted = append(deleted, name)
		}
	}
	return deleted, nil
}

// DoctorCheck は 1 項目の診断結果。Status は "ok" | "warn" | "error"。
type DoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

const (
	checkOK    = "ok"
	checkWarn  = "warn"
	checkError = "error"
)

// DoctorReport は sashiki doctor の診断結果。
type DoctorReport struct {
	PoolHealthy       bool // pool 容量が読めるか(CapacityReporter があるか)
	PoolStatusHealthy bool // zpool status -x が healthy か(#88)
	PoolStatusDetail  string
	PoolUsedRatio     float64
	CurrentBaseline   string
	BaselineMasked    bool // current baseline が masked か(#88)
	BaselineValidated bool // current baseline が validated か(#88)
	StateDBWritable   bool // state.db が書き込み可能か(#88)
	BranchCount       int
	PortConflicts     []string
	ExposedListeners  []string
	Orphans           []string
	MemHeadroomOK     bool
	Issues            []string
	Checks            []DoctorCheck // 整形表示用(#88)
}

// Doctor は運用者向けの健全性レポートを返す(仕様 20-2)。
func (m *Manager) Doctor(ctx context.Context) (DoctorReport, error) {
	var d DoctorReport
	add := func(name, status, detail string) {
		d.Checks = append(d.Checks, DoctorCheck{Name: name, Status: status, Detail: detail})
		if status != checkOK {
			d.Issues = append(d.Issues, fmt.Sprintf("%s: %s", name, detail))
		}
	}

	branches, err := m.db.ListBranches()
	if err != nil {
		return d, err
	}
	d.BranchCount = len(branches)

	// state.db 書き込み可否
	if err := m.db.Writable(); err != nil {
		add("state.db writable", checkError, err.Error())
	} else {
		d.StateDBWritable = true
		add("state.db writable", checkOK, "")
	}

	// current baseline とその provenance(masked / validated)
	d.CurrentBaseline = string(m.currentBaseline())
	if d.CurrentBaseline == "" {
		add("current baseline", checkError, "未設定(baseline import/refresh が必要)")
	} else {
		add("current baseline", checkOK, d.CurrentBaseline)
		if row, err := m.db.GetBaseline(d.CurrentBaseline); err == nil {
			d.BaselineMasked = row.Prov.Masked
			d.BaselineValidated = row.Prov.Validated
			if d.BaselineMasked {
				add("baseline masked", checkOK, "")
			} else {
				add("baseline masked", checkWarn, "current baseline は masked 済みとして記録されていない")
			}
			if d.BaselineValidated {
				add("baseline validated", checkOK, "")
			} else {
				add("baseline validated", checkWarn, "current baseline は validate 未通過")
			}
		}
	}

	// port 重複
	seen := map[int]string{}
	for _, b := range branches {
		if prev, ok := seen[b.Port]; ok {
			d.PortConflicts = append(d.PortConflicts, fmt.Sprintf("port %d: %s と %s", b.Port, prev, b.Name))
		}
		seen[b.Port] = b.Name
	}
	if len(d.PortConflicts) == 0 {
		add("port conflicts", checkOK, "")
	} else {
		add("port conflicts", checkError, strings.Join(d.PortConflicts, "; "))
	}

	// branch の直結ポートが非 loopback で待ち受けていると、proxy の認証終端を
	// 迂回できる。対応 engine では実 listener を確認し、露出は警告する(#288)。
	if checker, ok := m.eng.(engine.ListenerExposureChecker); ok {
		instances := make([]engine.Instance, 0, len(branches))
		for _, b := range branches {
			if b.State != state.StateRunning {
				continue
			}
			instances = append(instances, engine.Instance{Branch: b.Name, Port: b.Port})
		}
		exposed, err := checker.ExposedListeners(ctx, instances)
		switch {
		case err != nil:
			add("branch listener exposure", checkWarn, "確認できません: "+err.Error())
		case len(exposed) > 0:
			d.ExposedListeners = exposed
			add("branch listener exposure", checkWarn, strings.Join(exposed, "; ")+" は loopback 以外から到達可能。"+
				"systemd 構成は `sudo sashiki init --skip-packages --yes` で unit を更新し、branch を再起動してください")
		default:
			add("branch listener exposure", checkOK, "loopback only")
		}
	}

	// pool 健全性(zpool status -x 相当)
	if psc, ok := m.st.(storage.PoolStatusChecker); ok {
		healthy, detail, err := psc.PoolStatus(ctx)
		switch {
		case err != nil:
			add("pool status", checkWarn, "取得できません: "+err.Error())
		case healthy:
			d.PoolStatusHealthy = true
			d.PoolStatusDetail = detail
			add("pool status", checkOK, "healthy")
		default:
			d.PoolStatusDetail = detail
			add("pool status", checkError, detail)
		}
	}

	// pool 使用率
	if cr, ok := m.st.(storage.CapacityReporter); ok {
		if used, total, err := cr.PoolCapacity(ctx); err == nil && total > 0 {
			d.PoolHealthy = true
			d.PoolUsedRatio = float64(used) / float64(total)
			pct := fmt.Sprintf("%.0f%%", d.PoolUsedRatio*100)
			switch {
			case m.cfg.CriticalWatermark > 0 && d.PoolUsedRatio >= m.cfg.CriticalWatermark:
				add("pool usage", checkError, "critical: "+pct)
			case m.cfg.HighWatermark > 0 && d.PoolUsedRatio >= m.cfg.HighWatermark:
				add("pool usage", checkWarn, "high: "+pct)
			default:
				add("pool usage", checkOK, pct)
			}
		}
	}

	// orphan
	if rep, err := m.inspect(ctx); err == nil {
		d.Orphans = rep.Orphans
		if len(d.Orphans) == 0 {
			add("orphan datasets", checkOK, "")
		} else {
			add("orphan datasets", checkWarn, strings.Join(d.Orphans, "; ")+" (gc --orphans で回収)")
		}
	}

	// memory headroom
	if m.cfg.AvailableMem != nil {
		if err := m.admitMemory("doctor"); err == nil {
			d.MemHeadroomOK = true
			add("memory headroom", checkOK, "")
		} else {
			add("memory headroom", checkWarn, err.Error())
		}
	} else {
		d.MemHeadroomOK = true
	}
	return d, nil
}
