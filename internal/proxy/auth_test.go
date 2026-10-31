package proxy

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// recordRouter は RouteBranch の呼び出しを記録するフェイク Router。
type recordRouter struct {
	mu       sync.Mutex
	routed   []string
	existing []string
	touched  []string
	port     int
	err      error
}

func (r *recordRouter) RouteBranch(_ context.Context, name string) (int, error) {
	r.mu.Lock()
	r.routed = append(r.routed, name)
	r.mu.Unlock()
	return r.port, r.err
}

// RouteExistingBranch は passthrough(#394)の経路。呼び出しを別に記録し、
// lazy create 側(RouteBranch)が呼ばれていないことをテストで確かめられるようにする。
func (r *recordRouter) RouteExistingBranch(_ context.Context, name string) (int, error) {
	r.mu.Lock()
	r.existing = append(r.existing, name)
	r.mu.Unlock()
	return r.port, r.err
}

func (r *recordRouter) TouchConn(name string) {
	r.mu.Lock()
	r.touched = append(r.touched, name)
	r.mu.Unlock()
}

func (r *recordRouter) routeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.routed)
}

func newTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	if cfg.NamePattern == "" {
		cfg.NamePattern = `^[a-z0-9-]{1,32}$`
	}
	s, err := New(cfg, nil) // router は呼び出しごとに差し替える
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// clientHandshake は proxy の初期ハンドシェイクを読み、username と password で
// mysql_native_password 応答を書き、応答パケット(OK/ERR)を1つ読んで返す。
func clientHandshake(t *testing.T, conn net.Conn, username, password string) packet {
	t.Helper()
	hs, err := readPacket(conn)
	if err != nil {
		t.Fatalf("read initial handshake: %v", err)
	}
	salt, _, err := parseBackendHandshake(hs.body)
	if err != nil {
		t.Fatalf("parse handshake: %v", err)
	}
	hr := handshakeResponse{
		caps:     capProtocol41 | capSecureConn | capPluginAuth,
		maxLen:   1 << 24,
		charset:  0xff,
		username: username,
	}
	authResp := nativeToken(password, salt)
	body := buildBackendHandshakeResponse(hr, hr.caps, username, authResp, nativePlugin)
	if err := writePacket(conn, packet{seq: 1, body: body}); err != nil {
		t.Fatalf("write handshake response: %v", err)
	}
	resp, err := readPacket(conn)
	if err != nil {
		t.Fatalf("read auth result: %v", err)
	}
	return resp
}

// runAuth は authTerminate を server 側で走らせ、client 側の 1 パケット応答を返す。
func runAuth(t *testing.T, s *Server, username, password string) packet {
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
	resp := clientHandshake(t, clientConn, username, password)
	<-done
	return resp
}

func TestAuthTerminateRejectsWrongPasswordWithoutRouting(t *testing.T) {
	r := &recordRouter{port: 3401}
	s := newTestServer(t, Config{AppUser: "dev", AppPassword: "s3cret"})
	s.router = r

	resp := runAuth(t, s, "dev@pr-1", "wrong-password")
	if !isErr(resp.body) {
		t.Fatalf("wrong password should get an ERR packet, got %v", resp.body)
	}
	// 方式A の要: 認証に失敗したら branch には一切触れない(#7/#51 の DoS 不変条件)。
	if n := r.routeCount(); n != 0 {
		t.Errorf("RouteBranch called %d times on auth failure, want 0 (auth must precede routing)", n)
	}
}

func TestAuthTerminateRejectsMissingBranchWithoutRouting(t *testing.T) {
	r := &recordRouter{port: 3401}
	s := newTestServer(t, Config{AppUser: "dev", AppPassword: "s3cret"})
	s.router = r

	// user@branch 形式でない(@ が無い)。
	resp := runAuth(t, s, "dev", "s3cret")
	if !isErr(resp.body) {
		t.Fatal("username without @branch should be rejected")
	}
	if n := r.routeCount(); n != 0 {
		t.Errorf("RouteBranch called %d times for malformed username, want 0", n)
	}
}

func TestAuthTerminateEnforcesAllowedUser(t *testing.T) {
	r := &recordRouter{port: 3401}
	s := newTestServer(t, Config{AppUser: "dev", AppPassword: "s3cret", AllowedUser: "dev"})
	s.router = r

	// パスワードは正しいが user 部が AllowedUser と不一致。
	resp := runAuth(t, s, "mallory@pr-1", "s3cret")
	if !isErr(resp.body) {
		t.Fatal("user not matching AllowedUser should be rejected")
	}
	if n := r.routeCount(); n != 0 {
		t.Errorf("RouteBranch called %d times for disallowed user, want 0", n)
	}
}

func TestAuthTerminateRoutesAfterSuccessfulAuth(t *testing.T) {
	// 認証成功後に branch 解決を試みる。router がエラーを返すと Unknown branch。
	// これで「認証 → route」の順序(認証が先)を確認する。
	r := &recordRouter{err: context.DeadlineExceeded}
	s := newTestServer(t, Config{AppUser: "dev", AppPassword: "s3cret"})
	s.router = r

	resp := runAuth(t, s, "dev@pr-1", "s3cret")
	if !isErr(resp.body) {
		t.Fatal("unknown branch should get an ERR packet")
	}
	if n := r.routeCount(); n != 1 {
		t.Errorf("RouteBranch called %d times, want 1 (routing must happen after successful auth)", n)
	}
	if r.routed[0] != "pr-1" {
		t.Errorf("routed branch = %q, want pr-1", r.routed[0])
	}
}
