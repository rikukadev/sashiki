package authlimit

import (
	"net"
	"testing"
	"time"
)

func newTest() (*Limiter, *time.Time) {
	now := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	l := New()
	l.now = func() time.Time { return now }
	return l, &now
}

// 閾値までは遅延なし、超えたら 1s, 2s, 4s, 8s で頭打ち。成功でリセット。
func TestBackoffGrowsThenResets(t *testing.T) {
	l, _ := newTest()
	for i := 0; i < 5; i++ {
		if d := l.Fail("10.0.0.1"); d != 0 {
			t.Fatalf("fail %d: delay %v, want 0 (under threshold)", i+1, d)
		}
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, w := range want {
		if d := l.Fail("10.0.0.1"); d != w {
			t.Errorf("fail %d over threshold: delay %v, want %v", i+1, d, w)
		}
	}
	// 別の接続元は独立
	if d := l.Fail("10.0.0.2"); d != 0 {
		t.Errorf("other source should start fresh, got %v", d)
	}
	l.Reset("10.0.0.1")
	if d := l.Fail("10.0.0.1"); d != 0 {
		t.Errorf("after reset the first failure should be free, got %v", d)
	}
}

// window を過ぎた失敗は忘れる(古い typo で永久に遅くならない)。
func TestForgetsAfterWindow(t *testing.T) {
	l, now := newTest()
	for i := 0; i < 6; i++ {
		l.Fail("10.0.0.1")
	}
	*now = now.Add(11 * time.Minute)
	if d := l.Fail("10.0.0.1"); d != 0 {
		t.Errorf("failures older than the window should be forgotten, got %v", d)
	}
}

// 空キー(接続元が取れない)は何もしない。
func TestEmptyKeyIsNoop(t *testing.T) {
	l, _ := newTest()
	for i := 0; i < 20; i++ {
		if d := l.Fail(""); d != 0 {
			t.Fatalf("empty key must never delay, got %v", d)
		}
	}
}

func TestKey(t *testing.T) {
	a, _ := net.ResolveTCPAddr("tcp", "192.0.2.1:5555")
	if k := Key(a); k != "192.0.2.1" {
		t.Errorf("Key = %q", k)
	}
	if k := Key(nil); k != "" {
		t.Errorf("Key(nil) = %q", k)
	}
}
