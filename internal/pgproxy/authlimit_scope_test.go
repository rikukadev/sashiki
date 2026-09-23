package pgproxy

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type gateRouter struct {
	gate  chan struct{}
	calls atomic.Int32
}

func (r *gateRouter) RouteBranch(ctx context.Context, name string) (int, error) {
	r.calls.Add(1)
	select {
	case <-r.gate:
	case <-ctx.Done():
	}
	return 0, context.Canceled
}
func (r *gateRouter) TouchConn(string) {}

// authlimit のスロットは検証の間だけ握る(#318)。以前は pipe() が終わるまで
// 握っていたので、同じ IP からの同時セッションが 8 本で頭打ちだった。
// 認証済みのセッションを 1 本ずつ積み上げ(route でブロックさせたまま)、
// 9 本目以降も 53300 で弾かれずに route まで進むことを見る。
// 同時に検証中の試行を 8 に絞る動作そのものは残るので、1 本ずつ認証させる。
func TestAuthSlotReleasedAfterVerification(t *testing.T) {
	r := &gateRouter{gate: make(chan struct{})}
	addr := startProxy(t, Config{AppUser: "dev", AppPassword: "pw"}, r)
	defer close(r.gate)

	const n = 12
	for i := 1; i <= n; i++ {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		go func() { _ = scramClientHandshakeNoT(conn, "dev@pr-1", "pw") }()
		deadline := time.Now().Add(3 * time.Second)
		for r.calls.Load() < int32(i) && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if got := r.calls.Load(); got != int32(i) {
			t.Fatalf("session %d did not reach routing (%d reached); the auth slot must not be held for the whole session", i, got)
		}
	}
}

// scramClientHandshakeNoT は scramClientHandshake の goroutine 用(testing.T を使わない)。
func scramClientHandshakeNoT(conn net.Conn, user, password string) error {
	if err := writeStartup(conn, map[string]string{"user": user, "database": "app"}); err != nil {
		return err
	}
	cl := &scramClient{password: password}
	first, err := cl.first()
	if err != nil {
		return err
	}
	for {
		m, err := readMessage(conn)
		if err != nil {
			return err
		}
		switch m.typ {
		case msgErrorResponse:
			return errorTextErr(m.body)
		case msgAuthentication:
			code, data, _ := authCode(m.body)
			switch code {
			case authSASL:
				if err := writeSASLInitial(conn, "SCRAM-SHA-256", first); err != nil {
					return err
				}
			case authSASLContinue:
				final, err := cl.final(string(data))
				if err != nil {
					return err
				}
				if err := writeMessage(conn, msgPassword, []byte(final)); err != nil {
					return err
				}
			case authSASLFinal:
				if err := cl.verifyServerFinal(string(data)); err != nil {
					return err
				}
			case authOK:
				return nil
			}
		default:
			return nil
		}
	}
}

type textErr string

func (e textErr) Error() string { return string(e) }

func errorTextErr(body []byte) error { return textErr(errorText(body)) }
