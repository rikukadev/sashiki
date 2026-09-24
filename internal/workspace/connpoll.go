// connpoll: engine を定期ポーリングして last_conn_at を更新する(仕様 13-5 / #41)。
// proxy の TouchConn は「接続開始時」しか last_conn_at を進めないため、長寿命接続を
// 張ったままの branch を reaper が誤って停止・削除しうる(PR #20 の critical)。また
// proxy を通らない postgres / fsx 直続では last_conn_at が一切更新されない(PR #25 で
// postgres のリーパーを無効化していた制限)。engine が現在の接続数を返せる
// (engine.ConnCounter)なら、それを定期取得して last_conn_at と活動判定を更新する。
package workspace

import (
	"context"
	"time"

	"github.com/rikukadev/sashiki/internal/engine"
	"github.com/rikukadev/sashiki/internal/oplog"
	"github.com/rikukadev/sashiki/internal/state"
)

// connPollTimeout は 1 branch あたりの接続数取得のタイムアウト。
const connPollTimeout = 5 * time.Second

// RunConnPoller は interval ごとに全 running branch の接続数を engine から取得し、
// last_conn_at を更新する。engine が ConnCounter 未実装なら即 return(何もしない)。
func (m *Manager) RunConnPoller(ctx context.Context, interval time.Duration) {
	cc, ok := m.eng.(engine.ConnCounter)
	if !ok {
		return
	}
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
			m.pollConnsOnce(ctx, cc)
		}
	}
}

// pollConnsOnce は 1 パス分のポーリング。running branch ごとに接続数を取得し、
//   - 取得成功かつ >0 → last_conn_at を進める(idle タイマをリセット)
//   - 取得失敗 → 「判定不能=使用中」として保護する(誤って active branch を
//     削除しデータを失うより、回収漏れ(leak)を選ぶ)
//
// 観測した接続数は pollConns に載せ、activeConns 経由で reaper の使用中判定に使う。
func (m *Manager) pollConnsOnce(ctx context.Context, cc engine.ConnCounter) {
	branches, err := m.db.ListBranches()
	if err != nil {
		oplog.Errorf(ctx, "connpoll: list branches: %v", err)
		return
	}
	counts := make(map[string]int)
	for _, b := range branches {
		if b.State != state.StateRunning {
			continue
		}
		n, err := m.checkedConnCount(ctx, cc, engine.Instance{Branch: b.Name, Port: b.Port})
		if err != nil {
			// 判定不能: 保護側に倒す(使用中として扱い reaper の対象から外す)。
			// ただし失敗が続くなら、それは「使用中」ではなく監視が壊れている
			// (psql / mysql が無い、認証が通らない)。毎分の同じログに埋もれて
			// idle 回収が一度も効いていないことに気づけないので、閾値で
			// 一度だけ強く言う(#291)。
			streak := m.noteConnFail(b.Name)
			if streak == connFailWarnAt {
				oplog.Errorf(oplog.WithBranch(ctx, b.Name), "connpoll: %s: 接続数の取得に %d 回連続で失敗しています。"+
					"このブランチは idle 停止 / 自動削除の対象になりません(監視の認証・クライアントを確認): %v",
					b.Name, streak, err)
			} else {
				oplog.Logf(oplog.WithBranch(ctx, b.Name), "connpoll: %s: %v (使用中として保護)", b.Name, err)
			}
			counts[b.Name] = 1
			continue
		}
		m.clearConnFail(b.Name)
		counts[b.Name] = n
		if n > 0 {
			if terr := m.db.TouchLastConn(b.Name); terr != nil {
				oplog.Errorf(oplog.WithBranch(ctx, b.Name), "connpoll: touch %s: %v", b.Name, terr)
			}
		}
	}
	m.pollMu.Lock()
	m.pollConns = counts
	m.pollMu.Unlock()
}

// connFailWarnAt は連続失敗を強く警告する回数(reaper_interval 既定 1 分なら 3 分)。
const connFailWarnAt = 3

// noteConnFail は連続失敗回数を増やして返す。
func (m *Manager) noteConnFail(name string) int {
	m.pollMu.Lock()
	defer m.pollMu.Unlock()
	if m.connFails == nil {
		m.connFails = map[string]int{}
	}
	m.connFails[name]++
	return m.connFails[name]
}

// clearConnFail は成功で連続失敗をリセットする。
func (m *Manager) clearConnFail(name string) {
	m.pollMu.Lock()
	delete(m.connFails, name)
	m.pollMu.Unlock()
}

// checkedConnCount は内部の接続数チェックを直列化する。MySQL の ConnCount は
// Threads_connected から自分自身を1つ引くため、監視が同時実行されると相手の
// 監視接続を実クライアントと誤認する。timeout は lock 獲得後から数える。
func (m *Manager) checkedConnCount(ctx context.Context, cc engine.ConnCounter, ins engine.Instance) (int, error) {
	m.connCheckMu.Lock()
	defer m.connCheckMu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, connPollTimeout)
	defer cancel()
	return cc.ConnCount(cctx, ins)
}
