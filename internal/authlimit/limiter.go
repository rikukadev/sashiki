// Package authlimit は proxy の認証失敗に接続元ごとの backoff を掛ける(#297)。
//
// proxy は app パスワード 1 つで認証を終端している。既定値が dev のまま公開されて
// いたり、パスワードが短かったりすると、総当たりを止めるものが 60 秒の接続
// deadline しか無かった。失敗が続いた接続元には、次の失敗応答を遅らせる
// (tarpit)。正しいパスワードで通れば即リセットするので、typo を数回した利用者が
// 締め出されることはない。
package authlimit

import (
	"context"
	"net"
	"sync"
	"time"
)

// Limiter は接続元アドレスごとの連続失敗回数と同時試行数を持つ。
type Limiter struct {
	mu        sync.Mutex
	fails     map[string]entry
	inflight  map[string]int
	maxInFly  int           // 接続元ごとの同時認証試行の上限
	threshold int           // この回数までは遅延なし
	base      time.Duration // threshold 超過 1 回目の遅延
	max       time.Duration // 遅延の上限
	window    time.Duration // 最後の失敗からこれだけ経てば忘れる
	now       func() time.Time
}

type entry struct {
	count int
	last  time.Time
}

// New は既定値の Limiter を返す: 5 回までは遅延なし、以降 1s, 2s, 4s, 8s(上限)。
// 10 分失敗が無ければ忘れる。
func New() *Limiter {
	return &Limiter{
		fails:     map[string]entry{},
		inflight:  map[string]int{},
		maxInFly:  8,
		threshold: 5,
		base:      time.Second,
		max:       8 * time.Second,
		window:    10 * time.Minute,
		now:       time.Now,
	}
}

// Key は net.Conn の接続元から limiter のキー(IP 部)を取り出す。
// 取り出せなければ空文字を返し、その場合 Limiter は何もしない。
func Key(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}

// Acquire は接続元の認証試行スロットを取る。同時試行が上限を超えていれば
// false(呼び出し側は検証せず即拒否する)。並列化で backoff を迂回されない
// ようにし、待機中の接続を大量に抱える DoS も抑える(#310 review)。
// true のときは必ず Release すること。
func (l *Limiter) Acquire(key string) bool {
	if key == "" {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight[key] >= l.maxInFly {
		return false
	}
	l.inflight[key]++
	return true
}

// Release は Acquire で取ったスロットを返す。
func (l *Limiter) Release(key string) {
	if key == "" {
		return
	}
	l.mu.Lock()
	if l.inflight[key] <= 1 {
		delete(l.inflight, key)
	} else {
		l.inflight[key]--
	}
	l.mu.Unlock()
}

// Penalty はこれまでの失敗に応じて、検証の**前**に置く遅延を返す(記録は変えない)。
// 検証後に遅らせるだけだと、並列に接続すれば検証自体は無制限に進む。
func (l *Limiter) Penalty(key string) time.Duration {
	if key == "" {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.fails[key]
	if !ok || l.now().Sub(e.last) > l.window {
		return 0
	}
	return l.delayFor(e.count)
}

// Fail は失敗を記録し、次の試行に掛かる遅延を返す。
func (l *Limiter) Fail(key string) time.Duration {
	if key == "" {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.fails[key]
	if !e.last.IsZero() && now.Sub(e.last) > l.window {
		e = entry{}
	}
	e.count++
	e.last = now
	l.fails[key] = e
	l.sweepLocked(now)
	return l.delayFor(e.count)
}

// delayFor は連続失敗 count 回に対する遅延。閾値までは 0、以降 base, 2base, … max。
func (l *Limiter) delayFor(count int) time.Duration {
	over := count - l.threshold
	if over <= 0 {
		return 0
	}
	d := l.base << uint(over-1)
	if d > l.max || d <= 0 {
		d = l.max
	}
	return d
}

// Reset は認証成功で失敗履歴を消す。
func (l *Limiter) Reset(key string) {
	if key == "" {
		return
	}
	l.mu.Lock()
	delete(l.fails, key)
	l.mu.Unlock()
}

// Sleep は d だけ待つ。ctx が先に終われば抜ける。
func Sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// sweepLocked は window を過ぎたエントリを捨てる(map が伸び続けないように)。
// 呼び出し側で lock 済み。
func (l *Limiter) sweepLocked(now time.Time) {
	if len(l.fails) < 1024 {
		return
	}
	for k, e := range l.fails {
		if now.Sub(e.last) > l.window {
			delete(l.fails, k)
		}
	}
}
