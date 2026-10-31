package proxy

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeBackend は caching_sha2_password を名乗る最小の mysqld。passthrough(#394)の
// 中継が「backend の salt で改めてスクランブルされた応答」を正しく届けるかを見る。
// 認証の正否は backend が決める(= 方式A のように sashiki が検証しない)ことを、
// 受け取った username と検証結果を記録して確かめる。
type fakeBackend struct {
	ln       net.Listener
	password string
	mu       sync.Mutex
	users    []string
	oks      int
}

func newFakeBackend(t *testing.T, password string) *fakeBackend {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fb := &fakeBackend{ln: ln, password: password}
	go fb.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return fb
}

func (fb *fakeBackend) port() int { return fb.ln.Addr().(*net.TCPAddr).Port }

func (fb *fakeBackend) serve() {
	for {
		c, err := fb.ln.Accept()
		if err != nil {
			return
		}
		go fb.handle(c)
	}
}

func (fb *fakeBackend) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	hs, salt, err := buildInitialHandshake(7, false) // sha2Plugin を名乗る
	if err != nil {
		return
	}
	if err := writePacket(c, packet{seq: 0, body: hs}); err != nil {
		return
	}
	resp, err := readPacket(c)
	if err != nil {
		return
	}
	hr, err := parseHandshakeResponse(resp.body)
	if err != nil {
		return
	}
	fb.mu.Lock()
	fb.users = append(fb.users, hr.username)
	fb.mu.Unlock()
	if !verifyCachingSha2Password(fb.password, salt, hr.authResp) {
		_ = writePacket(c, packet{seq: resp.seq + 1, body: buildErr(1045, "28000", "Access denied for user '"+hr.username+"'@'localhost' (using password: YES)")})
		return
	}
	// 実物の MySQL と同じく fast_auth_success → OK の 2 パケット
	_ = writePacket(c, packet{seq: resp.seq + 1, body: []byte{0x01, 0x03}})
	_ = writePacket(c, packet{seq: resp.seq + 2, body: buildOK()})
	fb.mu.Lock()
	fb.oks++
	fb.mu.Unlock()
	// 認証後の素通し相手として、クライアントが閉じるまで待つ
	buf := make([]byte, 1)
	_, _ = c.Read(buf)
}

// passthroughClient は proxy の合成ハンドシェイクに適当な応答を返し、AuthSwitchRequest
// を受けたら backend の salt で caching_sha2 のトークンを作って返し、最終応答を返す。
func passthroughClient(t *testing.T, conn net.Conn, username, password string) (switched bool, final packet) {
	t.Helper()
	hs, err := readPacket(conn)
	if err != nil {
		t.Fatalf("read initial handshake: %v", err)
	}
	salt1, _, err := parseBackendHandshake(hs.body)
	if err != nil {
		t.Fatal(err)
	}
	hr := handshakeResponse{caps: capProtocol41 | capSecureConn | capPluginAuth | capPluginAuthLenC, maxLen: 1 << 24, charset: 0xff, username: username}
	// sashiki は passthrough ユーザーのこの応答を検証しない(password は知らない)
	body := buildBackendHandshakeResponse(hr, hr.caps, username, cachingSha2Token(password, salt1), sha2Plugin)
	if err := writePacket(conn, packet{seq: 1, body: body}); err != nil {
		t.Fatal(err)
	}
	p, err := readPacket(conn)
	if err != nil {
		t.Fatalf("read after handshake response: %v", err)
	}
	if len(p.body) == 0 || p.body[0] != 0xfe {
		return false, p // ERR 等(中継に入らなかった)
	}
	plugin, salt2 := parseAuthSwitch(p.body)
	if plugin != sha2Plugin {
		t.Fatalf("AuthSwitch plugin = %q, want %q", plugin, sha2Plugin)
	}
	if len(salt2) != 20 {
		t.Fatalf("AuthSwitch salt len = %d, want 20", len(salt2))
	}
	if err := writePacket(conn, packet{seq: p.seq + 1, body: cachingSha2Token(password, salt2)}); err != nil {
		t.Fatal(err)
	}
	for {
		q, err := readPacket(conn)
		if err != nil {
			t.Fatalf("read auth result: %v", err)
		}
		if isOK(q.body) || isErr(q.body) {
			return true, q
		}
		// 0x01 0x03(fast_auth_success)は読み飛ばして OK を待つ
	}
}

func runPassthrough(t *testing.T, s *Server, username, password string) (bool, packet) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	done := make(chan struct{})
	go func() {
		_ = clientConn.SetDeadline(time.Now().Add(3 * time.Second))
		_ = serverConn.SetDeadline(time.Now().Add(3 * time.Second))
		_ = s.authTerminate(context.Background(), serverConn)
		_ = serverConn.Close()
		close(done)
	}()
	switched, final := passthroughClient(t, clientConn, username, password)
	_ = clientConn.Close()
	<-done
	return switched, final
}

func TestPassthroughRelaysBackendAuthForExistingBranch(t *testing.T) {
	fb := newFakeBackend(t, "photo-secret")
	s := newTestServer(t, Config{AppUser: "dev", AppPassword: "dev", PassthroughUsers: []string{"photo_app"}})
	r := &recordRouter{port: fb.port()}
	s.router = r

	switched, final := runPassthrough(t, s, "photo_app@b1", "photo-secret")
	if !switched {
		t.Fatalf("AuthSwitchRequest が来なかった: %x", final.body)
	}
	if !isOK(final.body) {
		t.Fatalf("final = %s, want OK", errText(final.body))
	}
	// backend が受け取ったのは @branch を外した user 名
	fb.mu.Lock()
	users, oks := fb.users, fb.oks
	fb.mu.Unlock()
	if len(users) != 1 || users[0] != "photo_app" || oks != 1 {
		t.Fatalf("backend users=%v oks=%d, want [photo_app] 1", users, oks)
	}
	// 既存ブランチの経路だけを使い、lazy create 側(RouteBranch)には触れない
	if r.routeCount() != 0 || len(r.existing) != 1 || r.existing[0] != "b1" {
		t.Fatalf("routed=%v existing=%v, want lazy なし / existing=[b1]", r.routed, r.existing)
	}
	if len(r.touched) != 1 {
		t.Fatalf("TouchConn が %d 回、want 1", len(r.touched))
	}
}

func TestPassthroughWrongPasswordIsRejectedByBackend(t *testing.T) {
	fb := newFakeBackend(t, "photo-secret")
	s := newTestServer(t, Config{AppUser: "dev", AppPassword: "dev", PassthroughUsers: []string{"photo_app"}})
	r := &recordRouter{port: fb.port()}
	s.router = r

	switched, final := runPassthrough(t, s, "photo_app@b1", "WRONG")
	if !switched {
		t.Fatalf("AuthSwitchRequest が来なかった: %x", final.body)
	}
	if !isErr(final.body) {
		t.Fatalf("final = %x, want ERR(backend が拒否)", final.body)
	}
	if len(r.touched) != 0 {
		t.Fatalf("認証失敗なのに TouchConn が呼ばれた")
	}
}

func TestPassthroughDoesNotLazyCreate(t *testing.T) {
	s := newTestServer(t, Config{AppUser: "dev", AppPassword: "dev", PassthroughUsers: []string{"photo_app"}})
	r := &recordRouter{err: context.DeadlineExceeded} // 既存ブランチが無い
	s.router = r

	switched, final := runPassthrough(t, s, "photo_app@nope", "x")
	if switched {
		t.Fatal("ブランチが無いのに AuthSwitch(= backend 接続)まで進んだ")
	}
	if !isErr(final.body) {
		t.Fatalf("final = %x, want ERR", final.body)
	}
	if r.routeCount() != 0 {
		t.Fatalf("lazy create 側の RouteBranch が呼ばれた: %v", r.routed)
	}
}

func TestPassthroughUserMustBeListed(t *testing.T) {
	fb := newFakeBackend(t, "photo-secret")
	// passthrough_users に無い user は、allowed_user(既定 app_user)の絞りで落ちる
	s := newTestServer(t, Config{AppUser: "dev", AppPassword: "dev", AllowedUser: "dev", PassthroughUsers: []string{"photo_app"}})
	r := &recordRouter{port: fb.port()}
	s.router = r

	switched, final := runPassthrough(t, s, "gear_app@b1", "whatever")
	if switched || !isErr(final.body) {
		t.Fatalf("switched=%v final=%x, want 拒否(ERR)", switched, final.body)
	}
	if r.routeCount() != 0 || len(r.existing) != 0 {
		t.Fatal("拒否なのにブランチに触れた")
	}
}

func TestAppUserIsNeverPassthrough(t *testing.T) {
	// app_user を passthrough_users に書いても方式A のまま(sashiki が検証する)
	s := newTestServer(t, Config{AppUser: "dev", AppPassword: "dev", PassthroughUsers: []string{"dev"}})
	if s.passthrough["dev"] {
		t.Fatal("app_user が passthrough 扱いになっている")
	}
}
