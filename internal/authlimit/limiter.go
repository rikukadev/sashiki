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

// Limiter は接続元アドレスごとの連続失敗回数を持つ。
type Limiter struct {
	mu        sync.Mutex
	fails     map[string]entry
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

// Fail は失敗を記録し、この失敗応答の前に置くべき遅延を返す。
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
	over := e.count - l.threshold
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
