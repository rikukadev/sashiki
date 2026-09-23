package proxy

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// gateRouter は RouteBranch でブロックする(セッションが長く続く状況の代わり)。
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
// ここでは 12 本を同時に認証させ、全部が route まで進む(= 9 本目以降が
// 1040 で弾かれない)ことを見る。
func TestAuthSlotReleasedAfterVerification(t *testing.T) {
	r := &gateRouter{gate: make(chan struct{})}
	s := newTestServer(t, Config{AppUser: "dev", AppPassword: "s3cret"})
	s.router = r

	const n = 12
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 1; i <= n; i++ {
		serverConn, clientConn := net.Pipe()
		go func() { _ = s.authTerminate(ctx, serverConn); _ = serverConn.Close() }()
		go func() {
			_ = clientConn.SetDeadline(time.Now().Add(10 * time.Second))
			hs, err := readPacket(clientConn)
			if err != nil {
				return
			}
			salt, _, _ := parseBackendHandshake(hs.body)
			hr := handshakeResponse{caps: capProtocol41 | capSecureConn | capPluginAuth, maxLen: 1 << 24, charset: 0xff, username: "dev@pr-1"}
			body := buildBackendHandshakeResponse(hr, hr.caps, "dev@pr-1", nativeToken("s3cret", salt), nativePlugin)
			_ = writePacket(clientConn, packet{seq: 1, body: body})
			_, _ = readPacket(clientConn) // 応答は route が返るまで来ない
			_ = clientConn.Close()
		}()
		// 1 本ずつ認証させ、route でブロックしたまま次を積む。同時に検証中の
		// 試行を 8 に絞る動作は残るので、認証自体は逐次にする。
		deadline := time.Now().Add(3 * time.Second)
		for r.calls.Load() < int32(i) && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if got := r.calls.Load(); got != int32(i) {
			t.Fatalf("session %d did not reach routing (%d reached); the auth slot must not be held for the whole session", i, got)
		}
	}
	close(r.gate)
}
