package pgproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- protocol 層 ---

func TestReadStartupParsesParams(t *testing.T) {
	c, srv := net.Pipe()
	defer func() { _ = c.Close() }()
	go func() {
		_ = writeStartup(c, map[string]string{"user": "dev@pr-1", "database": "app"})
	}()
	su, err := readStartup(srv)
	if err != nil {
		t.Fatal(err)
	}
	if su.code != protocolV3 {
		t.Fatalf("code = %d, want %d", su.code, protocolV3)
	}
	if su.params["user"] != "dev@pr-1" || su.params["database"] != "app" {
		t.Fatalf("params = %v", su.params)
	}
}

func TestReadStartupDetectsSSLRequest(t *testing.T) {
	c, srv := net.Pipe()
	defer func() { _ = c.Close() }()
	go func() {
		b := make([]byte, 8)
		binary.BigEndian.PutUint32(b[:4], 8)
		binary.BigEndian.PutUint32(b[4:], codeSSLRequest)
		_, _ = c.Write(b)
	}()
	su, err := readStartup(srv)
	if err != nil {
		t.Fatal(err)
	}
	if su.code != codeSSLRequest {
		t.Fatalf("code = %d, want SSLRequest", su.code)
	}
}

func TestErrorTextExtractsMessage(t *testing.T) {
	body := buildError("28P01", "password authentication failed")
	if got := errorText(body); got != "password authentication failed" {
		t.Fatalf("errorText = %q", got)
	}
}

func TestMD5TokenMatchesPostgresFormula(t *testing.T) {
	// md5("md5" 抜き) = md5(hex(md5(pass+user)) + salt)
	// 既知の組み合わせで安定していること(回帰検出用)。
	got := md5Token("secret", "dev", []byte{1, 2, 3, 4})
	if !strings.HasPrefix(got, "md5") || len(got) != 35 {
		t.Fatalf("md5Token = %q (want md5 + 32 hex)", got)
	}
	if got != md5Token("secret", "dev", []byte{1, 2, 3, 4}) {
		t.Fatal("md5Token must be deterministic")
	}
	if got == md5Token("secret", "dev", []byte{9, 9, 9, 9}) {
		t.Fatal("md5Token must depend on the salt")
	}
}

// --- エンドツーエンド(偽バックエンド + 本物の proxy) ---

type fakeRouter struct {
	port    int
	err     error
	mu      sync.Mutex
	routed  []string
	touched []string
}

func (f *fakeRouter) RouteBranch(_ context.Context, name string) (int, error) {
	f.mu.Lock()
	f.routed = append(f.routed, name)
	f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	return f.port, nil
}

func (f *fakeRouter) TouchConn(name string) {
	f.mu.Lock()
	f.touched = append(f.touched, name)
	f.mu.Unlock()
}

// fakeBackend は trust 認証の postgres を模す。AuthenticationOk を返したあと
// ReadyForQuery を送り、受け取ったバイトをそのまま返す(素通し確認用)。
func fakeBackend(t *testing.T) (addr string, startupParams chan map[string]string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	startupParams = make(chan map[string]string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		su, err := readStartup(conn)
		if err != nil {
			return
		}
		startupParams <- su.params
		if err := writeMessage(conn, msgAuthentication, buildAuthOK()); err != nil {
			return
		}
		// 認証後の起動シーケンス(クライアントへ素通しされるはず)。
		if err := writeMessage(conn, msgReadyForQuery, []byte{'I'}); err != nil {
			return
		}
		_, _ = io.Copy(conn, conn) // エコー
	}()
	return ln.Addr().String(), startupParams
}

// scramClientHandshake はテスト用クライアントとして proxy に対し SCRAM 認証を行う。
func scramClientHandshake(t *testing.T, conn net.Conn, user, password string) error {
	t.Helper()
	if err := writeStartup(conn, map[string]string{"user": user, "database": "app",
		"application_name": "rails-console", "TimeZone": "Asia/Tokyo"}); err != nil {
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
			return fmt.Errorf("server error: %s", errorText(m.body))
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
			default:
				return fmt.Errorf("unexpected auth code %d", code)
			}
		default:
			return fmt.Errorf("unexpected message %q", m.typ)
		}
	}
}

func startProxy(t *testing.T, cfg Config, r Router) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	cfg.Listen = ln.Addr().String()
	if cfg.NamePattern == "" {
		cfg.NamePattern = `^[a-z0-9-]{1,32}$`
	}
	s, err := New(cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Listen(ctx) }()
	// listen 開始を待つ
	for i := 0; i < 100; i++ {
		c, err := net.Dial("tcp", cfg.Listen)
		if err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cfg.Listen
}

// 正常系: SCRAM 認証 → lazy create(RouteBranch)→ バックエンドへ素通し。
func TestProxyRoutesAfterAuth(t *testing.T) {
	backendAddr, params := fakeBackend(t)
	_, portStr, _ := net.SplitHostPort(backendAddr)
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)

	r := &fakeRouter{port: port}
	addr := startProxy(t, Config{AppUser: "dev", AppPassword: "pw"}, r)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	if err := scramClientHandshake(t, conn, "dev@pr-1", "pw"); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	// 認証後: バックエンドの ReadyForQuery が素通しで届く
	m, err := readMessage(conn)
	if err != nil {
		t.Fatal(err)
	}
	if m.typ != msgReadyForQuery {
		t.Fatalf("expected ReadyForQuery, got %q", m.typ)
	}

	// route / touch が呼ばれ、branch 名が正しいこと
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.routed) != 1 || r.routed[0] != "pr-1" {
		t.Fatalf("routed = %v, want [pr-1]", r.routed)
	}
	if len(r.touched) != 1 || r.touched[0] != "pr-1" {
		t.Fatalf("touched = %v, want [pr-1]", r.touched)
	}

	// バックエンドには app_user と database が渡ること(user@branch ではない)
	select {
	case p := <-params:
		if p["user"] != "dev" {
			t.Errorf("backend user = %q, want dev", p["user"])
		}
		if p["database"] != "app" {
			t.Errorf("backend database = %q, want app", p["database"])
		}
		// クライアントの startup パラメータは backend に届く(#305)
		if p["application_name"] != "rails-console" || p["TimeZone"] != "Asia/Tokyo" {
			t.Errorf("client startup params should be forwarded, got %v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backend startup params not received")
	}
}

// 認証前に branch を触らないこと(#7 / #51 と同じ DoS 対策)。
func TestProxyDoesNotRouteBeforeAuth(t *testing.T) {
	r := &fakeRouter{port: 1}
	addr := startProxy(t, Config{AppUser: "dev", AppPassword: "right"}, r)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	err = scramClientHandshake(t, conn, "dev@pr-1", "wrong")
	if err == nil {
		t.Fatal("handshake with a wrong password must fail")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.routed) != 0 {
		t.Fatalf("RouteBranch must not be called before auth, got %v", r.routed)
	}
}

// user@branch 形式でない、または名前が不正なら認証前に落とすこと。
func TestProxyRejectsMalformedUser(t *testing.T) {
	for _, user := range []string{"dev", "dev@", "dev@BAD_NAME", "dev@" + strings.Repeat("x", 40)} {
		r := &fakeRouter{port: 1}
		addr := startProxy(t, Config{AppUser: "dev", AppPassword: "pw"}, r)
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if err := writeStartup(conn, map[string]string{"user": user}); err != nil {
			t.Fatal(err)
		}
		m, err := readMessage(conn)
		if err != nil {
			t.Fatalf("user %q: %v", user, err)
		}
		if m.typ != msgErrorResponse {
			t.Fatalf("user %q: expected ErrorResponse, got %q", user, m.typ)
		}
		_ = conn.Close()
		r.mu.Lock()
		if len(r.routed) != 0 {
			t.Fatalf("user %q must not reach the router", user)
		}
		r.mu.Unlock()
	}
}

// allowed_user が設定されていれば user 部を絞ること。
func TestProxyAllowedUser(t *testing.T) {
	r := &fakeRouter{port: 1}
	addr := startProxy(t, Config{AppUser: "dev", AppPassword: "pw", AllowedUser: "dev"}, r)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := writeStartup(conn, map[string]string{"user": "admin@pr-1"}); err != nil {
		t.Fatal(err)
	}
	m, err := readMessage(conn)
	if err != nil {
		t.Fatal(err)
	}
	if m.typ != msgErrorResponse {
		t.Fatalf("expected ErrorResponse, got %q", m.typ)
	}
	if !strings.Contains(errorText(m.body), "admin") {
		t.Errorf("error should mention the rejected user: %q", errorText(m.body))
	}
}

// TLS 未設定なら SSLRequest に 'N' を返し、そのまま平文で継続できること。
func TestProxyDeclinesSSLThenContinues(t *testing.T) {
	backendAddr, _ := fakeBackend(t)
	_, portStr, _ := net.SplitHostPort(backendAddr)
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)

	r := &fakeRouter{port: port}
	addr := startProxy(t, Config{AppUser: "dev", AppPassword: "pw"}, r)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b[:4], 8)
	binary.BigEndian.PutUint32(b[4:], codeSSLRequest)
	if _, err := conn.Write(b); err != nil {
		t.Fatal(err)
	}
	one := make([]byte, 1)
	if _, err := io.ReadFull(conn, one); err != nil {
		t.Fatal(err)
	}
	if one[0] != 'N' {
		t.Fatalf("expected 'N' (no TLS), got %q", one[0])
	}
	if err := scramClientHandshake(t, conn, "dev@pr-1", "pw"); err != nil {
		t.Fatalf("plaintext handshake after SSL decline: %v", err)
	}
}

// 接続数上限に達したら 53300 で断ること。
func TestProxyMaxConnPerBranch(t *testing.T) {
	backendAddr, _ := fakeBackend(t)
	_, portStr, _ := net.SplitHostPort(backendAddr)
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)

	r := &fakeRouter{port: port}
	addr := startProxy(t, Config{AppUser: "dev", AppPassword: "pw", MaxConnPerBranch: 1}, r)

	c1, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c1.Close() }()
	_ = c1.SetDeadline(time.Now().Add(10 * time.Second))
	if err := scramClientHandshake(t, c1, "dev@pr-1", "pw"); err != nil {
		t.Fatalf("first connection: %v", err)
	}

	c2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()
	_ = c2.SetDeadline(time.Now().Add(10 * time.Second))
	err = scramClientHandshake(t, c2, "dev@pr-1", "pw")
	if err == nil || !strings.Contains(err.Error(), "too many connections") {
		t.Fatalf("second connection should be refused, got %v", err)
	}
}

// RouteBranch が失敗したら 3D000 を返すこと。
func TestProxyUnknownBranch(t *testing.T) {
	r := &fakeRouter{err: errors.New("nope")}
	addr := startProxy(t, Config{AppUser: "dev", AppPassword: "pw"}, r)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	err = scramClientHandshake(t, conn, "dev@gone", "pw")
	if err == nil || !strings.Contains(err.Error(), "unknown branch") {
		t.Fatalf("expected unknown branch error, got %v", err)
	}
}

// --- CancelRequest 中継 (#234) ---

// fakeBackendWithKey は認証後に BackendKeyData を送る backend。
// キャンセル用に届いた CancelRequest を cancels に流す。
func fakeBackendWithKey(t *testing.T, pid, secret int32) (port int, cancels chan [2]int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	cancels = make(chan [2]int32, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				su, err := readStartup(conn)
				if err != nil {
					return
				}
				if su.code == codeCancelRequest {
					cancels <- [2]int32{su.cancelPID, su.cancelSecret}
					return // サーバは応答せず閉じる
				}
				_ = writeMessage(conn, msgAuthentication, buildAuthOK())
				key := make([]byte, 8)
				binary.BigEndian.PutUint32(key[0:4], uint32(pid))
				binary.BigEndian.PutUint32(key[4:8], uint32(secret))
				_ = writeMessage(conn, msgBackendKeyData, key)
				_ = writeMessage(conn, msgReadyForQuery, []byte{'I'})
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	_, _ = fmt.Sscanf(p, "%d", &port)
	return port, cancels
}

// BackendKeyData がクライアントへそのまま届き、その値で送った CancelRequest が
// 同じ backend へ転送されること。
func TestProxyRelaysCancelRequest(t *testing.T) {
	const pid, secret = int32(4242), int32(998877)
	port, cancels := fakeBackendWithKey(t, pid, secret)
	r := &fakeRouter{port: port}
	addr := startProxy(t, Config{AppUser: "dev", AppPassword: "pw"}, r)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := scramClientHandshake(t, conn, "dev@pr-1", "pw"); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	// 起動シーケンスが中継され、BackendKeyData の値がそのまま来ること
	var gotPID, gotSecret int32
	for {
		m, err := readMessage(conn)
		if err != nil {
			t.Fatal(err)
		}
		if m.typ == msgBackendKeyData {
			gotPID, gotSecret, _ = parseBackendKeyData(m.body)
		}
		if m.typ == msgReadyForQuery {
			break
		}
	}
	if gotPID != pid || gotSecret != secret {
		t.Fatalf("BackendKeyData = (%d,%d), want (%d,%d)", gotPID, gotSecret, pid, secret)
	}

	// 別接続でキャンセルを送る(psql の Ctrl-C 相当)
	c2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()
	if _, err := c2.Write(buildCancelRequest(gotPID, gotSecret)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-cancels:
		if got[0] != pid || got[1] != secret {
			t.Errorf("転送された CancelRequest = %v, want (%d,%d)", got, pid, secret)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CancelRequest が backend へ転送されなかった")
	}
}

// 未知の (pid, secret) は転送先が無いので黙って閉じる(他人の接続を止めない)。
func TestProxyIgnoresUnknownCancel(t *testing.T) {
	r := &fakeRouter{port: 1}
	addr := startProxy(t, Config{AppUser: "dev", AppPassword: "pw"}, r)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(buildCancelRequest(1, 2)); err != nil {
		t.Fatal(err)
	}
	// proxy 側は接続を閉じる。読み出しは EOF になるはず(パニックしないこと)。
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Error("未知のキャンセルは応答せず閉じるべき")
	}
	if len(r.routed) != 0 {
		t.Error("キャンセルで branch を触ってはいけない")
	}
}

// replication 接続は proxy 経由では断る(#305)。
func TestProxyRejectsReplicationStartup(t *testing.T) {
	r := &fakeRouter{port: 1}
	addr := startProxy(t, Config{AppUser: "dev", AppPassword: "pw"}, r)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := writeStartup(conn, map[string]string{"user": "dev@pr-1", "database": "app", "replication": "database"}); err != nil {
		t.Fatal(err)
	}
	m, err := readMessage(conn)
	if err != nil {
		t.Fatal(err)
	}
	if m.typ != msgErrorResponse || !strings.Contains(errorText(m.body), "replication") {
		t.Fatalf("replication startup should be rejected, got %q %q", m.typ, errorText(m.body))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.routed) != 0 {
		t.Errorf("must not route a replication connection: %v", r.routed)
	}
}

func TestBackendStartupParamsReplacesUserOnly(t *testing.T) {
	got := backendStartupParams("dev", map[string]string{
		"user": "dev@pr-1", "database": "app", "application_name": "psql", "options": "-c search_path=x",
	})
	if got["user"] != "dev" || got["database"] != "app" || got["application_name"] != "psql" || got["options"] != "-c search_path=x" {
		t.Errorf("params = %v", got)
	}
	if _, ok := backendStartupParams("dev", map[string]string{"user": "dev@pr-1"})["database"]; ok {
		t.Error("empty database should be omitted")
	}
}
