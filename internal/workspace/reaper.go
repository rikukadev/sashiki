// リーパー: アイドルブランチの mysqld を停止(sleeping)し、長期未使用の
// ブランチを削除する。メモリを消費するのは「動いている mysqld」であって
// ブランチのデータではない(PoC 実測: 1 本 ≈ 400MB)ため、停止だけで
// メモリ上限は「同時アクティブ数」にのみ比例するようになる。
package workspace

import (
	"context"
	"log"
	"time"

	"github.com/rikukadev/sashiki/internal/engine"
	"github.com/rikukadev/sashiki/internal/state"
)

// RunReaper は interval ごとに Reap を回す。ctx キャンセルで止まる。
func (m *Manager) RunReaper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := m.Reap(ctx); err != nil {
				log.Printf("reaper: %v", err)
			}
		}
	}
}

// Reap は 1 パス分の回収を行う。
//   - running かつ IdleStopAfter 以上接続なし → 正常終了して sleeping
//   - running/sleeping かつ DeleteAfterIdle 以上接続なし → 削除
//     (error 状態はログ確認のため自動削除しない: 仕様 14-2)
func (m *Manager) Reap(ctx context.Context) error {
	branches, err := m.db.ListBranches()
	if err != nil {
		return err
	}
	now := time.Now()
	for _, b := range branches {
		// lease 失効(expires_at)は絶対期限。idle と違い「使用中でも」回収する
		// (仕様 13-6: TTL/lease = correctness の担保)。activeConns の判定より先に見る。
		if b.ExpiresAt != nil && now.After(*b.ExpiresAt) &&
			(b.State == state.StateRunning || b.State == state.StateSleeping) {
			backing, berr := m.backingBaselinesForBranch(ctx, b)
			if berr != nil {
				log.Printf("reaper: baseline protection check %s: %v (保護)", b.Name, berr)
				continue
			}
			if len(backing) > 0 {
				// promote 元の実体は消せない。running のまま毎 tick Delete を拒否
				// され続ける代わりに、一度 sleeping へ落として保持する(#289)。
				if b.State == state.StateRunning {
					log.Printf("reaper: stopping %s instead of lease deletion (backs baseline %v)", b.Name, backing)
					if err := m.Sleep(ctx, b.Name); err != nil {
						log.Printf("reaper: stop protected branch %s: %v", b.Name, err)
					}
				}
				continue
			}
			log.Printf("reaper: deleting %s (lease expired %s ago)", b.Name, now.Sub(*b.ExpiresAt).Round(time.Second))
			if err := m.Delete(ctx, b.Name); err != nil {
				log.Printf("reaper: delete %s: %v", b.Name, err)
			}
			continue
		}
		activity := b.CreatedAt
		if b.LastConnAt != nil && b.LastConnAt.After(activity) {
			activity = *b.LastConnAt
		}
		idle := now.Sub(activity)

		// idle 閾値は branch の profile 由来(未設定は global へフォールバック)。
		pol := m.resolveProfile(b.Profile)
		deleteDue := pol.DeleteAfterIdle > 0 && idle >= pol.DeleteAfterIdle &&
			(b.State == state.StateRunning || b.State == state.StateSleeping)
		stopDue := pol.IdleStopAfter > 0 && idle >= pol.IdleStopAfter && b.State == state.StateRunning
		if !deleteDue && !stopDue {
			continue
		}

		// 長寿命接続を張ったままのブランチは last_conn_at が進まないため、
		// 現在の接続数を見て使用中なら idle 停止・削除の対象から外す。
		if m.activeConns(b.Name) > 0 {
			continue
		}
		// connpoll と reaper は独立した ticker で動く。poll の直後に直接接続が
		// 始まると、次の poll より reaper が先に走って「接続なし」の古い
		// キャッシュで停止し得る。実際に回収対象になった running branch だけ、
		// 破壊的な操作の直前に engine へ再確認する。取得失敗も「使用中」に倒す。
		if b.State == state.StateRunning {
			if cc, ok := m.eng.(engine.ConnCounter); ok {
				n, err := m.checkedConnCount(ctx, cc, engine.Instance{Branch: b.Name, Port: b.Port})
				if err != nil {
					log.Printf("reaper: connection check %s: %v (使用中として保護)", b.Name, err)
					continue
				}
				if n > 0 {
					if err := m.db.TouchLastConn(b.Name); err != nil {
						log.Printf("reaper: touch %s: %v", b.Name, err)
					}
					continue
				}
			}
		}

		if deleteDue {
			backing, berr := m.backingBaselinesForBranch(ctx, b)
			if berr != nil {
				log.Printf("reaper: baseline protection check %s: %v (保護)", b.Name, berr)
				continue
			}
			if len(backing) > 0 {
				if b.State == state.StateRunning {
					log.Printf("reaper: stopping %s instead of idle deletion (backs baseline %v)", b.Name, backing)
					if err := m.Sleep(ctx, b.Name); err != nil {
						log.Printf("reaper: stop protected branch %s: %v", b.Name, err)
					}
				}
				continue
			}
			log.Printf("reaper: deleting %s (idle %s, profile %q)", b.Name, idle.Round(time.Second), b.Profile)
			if err := m.Delete(ctx, b.Name); err != nil {
				log.Printf("reaper: delete %s: %v", b.Name, err)
			}
			continue
		}
		if stopDue {
			log.Printf("reaper: stopping %s (idle %s, profile %q)", b.Name, idle.Round(time.Second), b.Profile)
			if err := m.Sleep(ctx, b.Name); err != nil {
				log.Printf("reaper: stop %s: %v", b.Name, err)
			}
		}
	}
	// 完了/失敗した古い operation を掃除する(operations テーブルの無限成長防止, #83)。
	if m.cfg.OperationRetention > 0 {
		if n, err := m.db.PruneOperations(now.Add(-m.cfg.OperationRetention)); err != nil {
			log.Printf("reaper: prune operations: %v", err)
		} else if n > 0 {
			log.Printf("reaper: pruned %d old operations", n)
		}
	}
	return nil
}

// Sleep は mysqld を正常終了させて sleeping にする(データは残る、再開は Wake)。
func (m *Manager) Sleep(ctx context.Context, name string) error {
	unlock := m.lock(name)
	defer unlock()

	b, err := m.db.GetBranch(name)
	if err != nil {
		return err
	}
	if b.State != state.StateRunning {
		return nil
	}
	vol, err := m.resolveVolume(ctx, b)
	if err != nil {
		return err
	}
	if err := m.eng.Stop(ctx, m.instance(b, vol)); err != nil {
		return err
	}
	return m.db.SetState(name, state.StateSleeping, "")
}
