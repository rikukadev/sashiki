// Package proxy は :3306 の固定エンドポイント。クライアントのユーザー名
// `<user>@<branch>` からバックエンドの mysqld を選ぶ。
//
// 方式A(認証終端, #51): sashiki が app_user のパスワードを保持し、クライアントの
// mysql_native_password 認証を自身で検証する。**検証に成功してから**ブランチを
// route / lazy create するため、認証前の無償リソース確保(#7 の DoS 構造)が
// 起きない。バックエンドへは sashiki が保持する credential で接続し直す。
// TLS 終端は cert 指定時のみ有効(クライアント↔sashiki=TLS、sashiki↔backend=
// localhost 平文)。以前の認証中継(ADR-006)は DECISIONS.md に経緯として残す。
package proxy

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"github.com/rikukadev/sashiki/internal/authlimit"
	"io"
	"log"
	"net"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Router はブランチ名から接続先を解決する(branch.Manager が実装)。
type Router interface {
	// RouteBranch はブランチの接続先ポートを返す。存在しなければエラー。
	RouteBranch(ctx context.Context, name string) (port int, err error)
	// RouteExistingBranch は既存ブランチだけを解決する(sleeping は wake、未知は
	// エラーで lazy create しない)。認証をバックエンドに委ねる passthrough(#394)用。
	RouteExistingBranch(ctx context.Context, name string) (port int, err error)
	// TouchConn は最終接続時刻を記録する(まとめ書き可)。
	TouchConn(name string)
}

// Config はプロキシの設定。
type Config struct {
	Listen           string // "0.0.0.0:3306"
	NamePattern      string
	BackendHost      string // 既定 127.0.0.1
	MaxConnPerBranch int    // 既定 50
	// AllowedUser が非空なら、<user>@<branch> の user 部がこれと一致する
	// 接続だけを受け付ける。認証前の第一関門。
	AllowedUser string

	// AppUser / AppPassword は方式A の app credential。クライアント認証の検証と
	// バックエンド接続の両方に使う(本番は Secrets Manager 由来の値を配線する)。
	AppUser     string
	AppPassword string

	// PassthroughUsers は認証をブランチ内の mysqld に委ねる user 名(#394)。
	// sashiki はこれらのパスワードを持たないので方式A の検証はできない。代わりに
	// **既存ブランチに限って**(lazy create なし)バックエンドの認証を中継する。
	// app_user と同名は無視する(app_user は常に方式A)。
	PassthroughUsers []string

	// TLSConfig が非 nil なら TLS 終端を有効にする(クライアント↔sashiki)。
	TLSConfig *tls.Config
}

// Server は MySQL プロトコルプロキシ。
type Server struct {
	cfg    Config
	router Router
	nameRe *regexp.Regexp
	// limiter は接続元ごとの認証失敗 backoff(#297)。
	limiter *authlimit.Limiter
	connID  atomic.Uint32

	mu          sync.Mutex
	conns       map[string]int
	passthrough map[string]bool // Config.PassthroughUsers の集合(#394)
}

// New は Server を作る。
func New(cfg Config, router Router) (*Server, error) {
	if cfg.BackendHost == "" {
		cfg.BackendHost = "127.0.0.1"
	}
	if cfg.MaxConnPerBranch == 0 {
		cfg.MaxConnPerBranch = 50
	}
	if cfg.AppUser == "" {
		cfg.AppUser = "dev"
	}
	passthrough := make(map[string]bool, len(cfg.PassthroughUsers))
	for _, u := range cfg.PassthroughUsers {
		if u != "" && u != cfg.AppUser {
			passthrough[u] = true
		}
	}
	re, err := regexp.Compile(cfg.NamePattern)
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, router: router, nameRe: re, conns: map[string]int{}, limiter: authlimit.New(), passthrough: passthrough}, nil
}

// Listen は接続を受け付ける。ctx キャンセルで停止。
func (s *Server) Listen(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("proxy listen: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(ctx, conn)
	}
}

func (s *Server) acquire(branch string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns[branch] >= s.cfg.MaxConnPerBranch {
		return false
	}
	s.conns[branch]++
	return true
}

// ActiveConns はブランチの現在の素通し接続数を返す(リーパーの使用中判定用)。
func (s *Server) ActiveConns(branch string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns[branch]
}

func (s *Server) release(branch string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns[branch] > 0 {
		s.conns[branch]--
	}
}

func (s *Server) handle(ctx context.Context, client net.Conn) {
	defer func() { _ = client.Close() }()
	enableKeepAlive(client)
	_ = client.SetDeadline(time.Now().Add(60 * time.Second))

	if err := s.authTerminate(ctx, client); err != nil {
		log.Printf("proxy: %v", err)
	}
}

// verifyClient は authlimit のスロットを取って app パスワードを検証する。
// スロットは検証中だけ保持し、結果に応じて Fail / Reset してから返す。
// admitted=false は同時試行の上限超過(検証していない)。
func (s *Server) verifyClient(ctx context.Context, src string, authResp, salt []byte) (verified, admitted bool) {
	if !s.limiter.Acquire(src) {
		return false, false
	}
	defer s.limiter.Release(src)
	authlimit.Sleep(ctx, s.limiter.Penalty(src))
	if ctx.Err() != nil {
		return false, true
	}
	// クライアントの応答長で認証方式を判別する(#197): 32byte=caching_sha2
	// (MySQL 8.0 既定 / 9.x)、20byte=mysql_native_password(旧クライアント)。
	if len(authResp) != 20 {
		verified = verifyCachingSha2Password(s.cfg.AppPassword, salt[:20], authResp)
	} else {
		verified = verifyNativePassword(s.cfg.AppPassword, salt, authResp)
	}
	if verified {
		s.limiter.Reset(src)
	} else {
		s.limiter.Fail(src)
	}
	return verified, true
}

// authErr は ERR パケットを送って接続を切る。
func authErr(client net.Conn, seq byte, code uint16, state, msg string) error {
	_ = writePacket(client, packet{seq: seq, body: buildErr(code, state, msg)})
	return fmt.Errorf("auth rejected: %s", msg)
}

// authTerminate は方式A の認証フェーズ。クライアント認証を sashiki 自身が
// 検証し、成功してから branch を route / create してバックエンドへ接続し直す。
func (s *Server) authTerminate(ctx context.Context, client net.Conn) error {
	// 1. 合成ハンドシェイク送信(salt はクライアント検証に使う)
	hs, salt, err := buildInitialHandshake(s.connID.Add(1), s.cfg.TLSConfig != nil)
	if err != nil {
		return err
	}
	if err := writePacket(client, packet{seq: 0, body: hs}); err != nil {
		return err
	}

	// 2. クライアント応答。TLS 要求なら終端してから本応答を読み直す。
	resp, err := readPacket(client)
	if err != nil {
		return err
	}
	seq := resp.seq
	if isSSLRequest(resp.body) {
		if s.cfg.TLSConfig == nil {
			return authErr(client, seq+1, 1045, "28000", "TLS not configured")
		}
		tlsConn := tls.Server(client, s.cfg.TLSConfig)
		if err := tlsConn.Handshake(); err != nil {
			return fmt.Errorf("tls handshake: %w", err)
		}
		client = tlsConn
		if resp, err = readPacket(client); err != nil {
			return err
		}
		seq = resp.seq
	}
	hr, err := parseHandshakeResponse(resp.body)
	if err != nil {
		return authErr(client, seq+1, 1045, "28000", err.Error())
	}
	user, branch, ok := strings.Cut(hr.username, "@")
	if !ok || !s.nameRe.MatchString(branch) {
		return authErr(client, seq+1, 1045, "28000",
			fmt.Sprintf("Access denied: user must be <user>@<branch> (got %q)", hr.username))
	}
	// passthrough(#394): 設定で明示した user は方式A の検証をせず、既存ブランチに
	// 限ってバックエンドの認証を中継する。allowed_user の絞りとは独立に許可する
	passthrough := s.passthrough[user]
	if s.cfg.AllowedUser != "" && user != s.cfg.AllowedUser && !passthrough {
		return authErr(client, seq+1, 1045, "28000",
			fmt.Sprintf("Access denied for user %q", user))
	}
	if passthrough {
		_, isTLS := client.(*tls.Conn)
		return s.authPassthrough(ctx, client, hr, seq, user, branch, isTLS)
	}

	// 3. 認証終端: app パスワードで検証する。**ここを通るまで branch に触れない**
	//    (認証前 lazy create の DoS 構造を解消 — #7 / #51)。
	// クライアントの応答長で認証方式を判別する(#197): 32byte=caching_sha2
	// (MySQL 8.0 既定 / 9.x)、20byte=mysql_native_password(旧クライアント)。
	// 総当たり対策(#297)。同一接続元の同時試行を絞り、失敗が続いていれば
	// **検証の前に**待たせる。検証後に遅らせるだけだと並列接続で迂回できる。
	// スロットは**検証の間だけ**握る(#318)。以前は defer で関数末尾 = pipe() が
	// 終わるまで握っていたので、同じ IP からの同時セッションが 8 本で頭打ちになり、
	// 接続プールや NAT 越しのクライアントが 9 本目で拒否されていた。
	src := authlimit.Key(client.RemoteAddr())
	sha2 := len(hr.authResp) != 20 // 応答の形は後段(OK の返し方)でも使う
	verified, admitted := s.verifyClient(ctx, src, hr.authResp, salt)
	if !admitted {
		return authErr(client, seq+1, 1040, "08004", "Too many concurrent authentication attempts")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !verified {
		return authErr(client, seq+1, 1045, "28000",
			fmt.Sprintf("Access denied for user '%s'@'%s' (using password: YES)", user, branch))
	}

	// 4. 認証済み → branch 解決(必要なら lazy create)
	port, err := s.router.RouteBranch(ctx, branch)
	if err != nil {
		return authErr(client, seq+1, 1049, "42000", fmt.Sprintf("Unknown branch '%s'", branch))
	}
	if !s.acquire(branch) {
		return authErr(client, seq+1, 1040, "08004", "Too many connections for branch")
	}
	defer s.release(branch)

	// 5. バックエンドへ sashiki 保持の credential で接続し直す
	backend, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", s.cfg.BackendHost, port), 10*time.Second)
	if err != nil {
		return authErr(client, seq+1, 2003, "HY000", "backend unavailable")
	}
	defer func() { _ = backend.Close() }()
	if berr := s.authenticateBackend(backend, hr.caps, hr.database); berr != nil {
		log.Printf("proxy: backend auth for %s@%s: %v", user, branch, berr)
		return authErr(client, seq+1, 2003, "HY000", "backend auth failed")
	}

	// 6. 接続成立を記録する。クライアントへ OK を返す**前**に呼ぶ(pgproxy と同じ理由)。
	//    後ろに置くと「クライアントは繋がったのに last_conn_at が未更新」の瞬間ができる。
	s.router.TouchConn(branch)

	// 7. クライアントへ認証完了を返す。caching_sha2 は AuthMoreData(0x01 0x03 =
	//    fast_auth_success)を先に送ってから OK(#197)。native は OK のみ。
	if sha2 {
		if err := writePacket(client, packet{seq: seq + 1, body: []byte{0x01, 0x03}}); err != nil {
			return err
		}
		if err := writePacket(client, packet{seq: seq + 2, body: buildOK()}); err != nil {
			return err
		}
	} else {
		if err := writePacket(client, packet{seq: seq + 1, body: buildOK()}); err != nil {
			return err
		}
	}

	// 認証完了 → 素通し(接続終了までブロック)
	enableKeepAlive(backend)
	_ = client.SetDeadline(time.Time{})
	pipe(client, backend)
	return nil
}

// authPassthrough はブランチ内のユーザー(app_user 以外)の認証をバックエンドの
// mysqld に委ねる(#394)。sashiki はそのパスワードを持たないので方式A の検証は
// できない。ADR-007 が方式A に寄せた理由は「認証前に lazy create させない」こと
// なので、この経路は **既存ブランチだけ**を解決し(未知の名前は作らない)、同時試行と
// 失敗の遅延は方式A と同じ authlimit に掛ける。sleeping の起床は認証前に起きる——
// 直結では起こせない起床を proxy に任せるのが、この経路の目的そのもの。
//
// 流れ: backend のハンドシェイクを読んで salt と plugin を得る → クライアントへ
// AuthSwitchRequest で同じ plugin と salt を提示し、改めてスクランブルさせる →
// それを backend への HandshakeResponse に載せる → 以降の認証パケット
// (caching_sha2 の fast/full auth、AuthSwitch の連鎖)を OK / ERR まで双方向に中継する。
// シーケンス番号は両側で別に数える(クライアントは sashiki のハンドシェイクから、
// backend は自分のハンドシェイクから数えているため)。
func (s *Server) authPassthrough(ctx context.Context, client net.Conn, hr handshakeResponse, seq byte, user, branch string, clientTLS bool) error {
	src := authlimit.Key(client.RemoteAddr())
	if !s.limiter.Acquire(src) {
		return authErr(client, seq+1, 1040, "08004", "Too many concurrent authentication attempts")
	}
	defer s.limiter.Release(src)
	authlimit.Sleep(ctx, s.limiter.Penalty(src))
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// 既存ブランチだけ。未知の名前はここで落ちる(lazy create しない)
	port, err := s.router.RouteExistingBranch(ctx, branch)
	if err != nil {
		s.limiter.Fail(src)
		return authErr(client, seq+1, 1049, "42000", fmt.Sprintf("Unknown branch '%s'", branch))
	}
	if !s.acquire(branch) {
		return authErr(client, seq+1, 1040, "08004", "Too many connections for branch")
	}
	defer s.release(branch)

	backend, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", s.cfg.BackendHost, port), 10*time.Second)
	if err != nil {
		return authErr(client, seq+1, 2003, "HY000", "backend unavailable")
	}
	defer func() { _ = backend.Close() }()
	bhs, err := readPacket(backend)
	if err != nil {
		return authErr(client, seq+1, 2003, "HY000", "backend unavailable")
	}
	salt, backendCaps, err := parseBackendHandshake(bhs.body)
	if err != nil {
		return authErr(client, seq+1, 2003, "HY000", "backend handshake unreadable")
	}
	plugin := parseBackendPlugin(bhs.body)
	if plugin == "" {
		plugin = sha2Plugin
	}

	// クライアントに backend の salt でスクランブルし直させる
	cseq := seq + 1
	if err := writePacket(client, packet{seq: cseq, body: buildAuthSwitchRequest(plugin, salt)}); err != nil {
		return err
	}
	cresp, err := readPacket(client)
	if err != nil {
		return err
	}
	cseq = cresp.seq

	// backend へは user(@branch を除く)と、クライアントが交渉した capability で名乗る
	// (方式A の authenticateBackend と同じ理由: DEPRECATE_EOF 等をクライアントに揃える)
	fwd := handshakeResponse{maxLen: 16 * 1024 * 1024, charset: 0xff}
	fwd.caps = hr.caps & synthCaps
	if hr.database != "" {
		fwd.caps |= capConnectWithDB
		fwd.database = hr.database
	} else {
		fwd.caps &^= capConnectWithDB
	}
	bseq := bhs.seq + 1
	if err := writePacket(backend, packet{seq: bseq, body: buildBackendHandshakeResponse(fwd, backendCaps, user, cresp.body, plugin)}); err != nil {
		return err
	}

	// 認証パケットの中継。OK / ERR で終わる
	nonce := salt
	for {
		p, err := readPacket(backend)
		if err != nil {
			return fmt.Errorf("passthrough %s@%s: read backend auth: %w", user, branch, err)
		}
		bseq = p.seq
		cseq++
		switch {
		case isOK(p.body):
			s.limiter.Reset(src)
			s.router.TouchConn(branch) // OK を返す前に記録する(方式A と同じ)
			if err := writePacket(client, packet{seq: cseq, body: p.body}); err != nil {
				return err
			}
			enableKeepAlive(backend)
			_ = client.SetDeadline(time.Time{})
			pipe(client, backend)
			return nil
		case isErr(p.body):
			s.limiter.Fail(src)
			_ = writePacket(client, packet{seq: cseq, body: p.body})
			return fmt.Errorf("passthrough %s@%s: backend rejected: %s", user, branch, errText(p.body))
		case len(p.body) == 2 && p.body[0] == 0x01 && p.body[1] == 0x03:
			// caching_sha2 fast_auth_success: クライアントの応答は要らず、次に OK が来る
			if err := writePacket(client, packet{seq: cseq, body: p.body}); err != nil {
				return err
			}
		case clientTLS && len(p.body) == 2 && p.body[0] == 0x01 && p.body[1] == 0x04:
			// full auth で、クライアント側が TLS のとき: クライアントは平文パスワードを
			// 送ってくる(TLS なので正当)。backend 側は平文 TCP なので、そのまま渡すと
			// backend が「secure connection が要る」と拒む。sashiki が RSA に詰め替える。
			if err := writePacket(client, packet{seq: cseq, body: p.body}); err != nil {
				return err
			}
			c, err := readPacket(client)
			if err != nil {
				return err
			}
			cseq = c.seq
			pw := c.body
			if n := len(pw); n > 0 && pw[n-1] == 0 {
				pw = pw[:n-1]
			}
			if err := backendFullAuthRSAWith(backend, bseq+1, nonce, string(pw)); err != nil {
				s.limiter.Fail(src)
				return authErr(client, cseq+1, 1045, "28000",
					fmt.Sprintf("Access denied for user '%s'@'%s' (using password: YES)", user, branch))
			}
			// backend 側の番号は次の readPacket で p.seq から取り直すので、ここで進める必要はない
		default:
			// AuthMoreData(full auth 要求 / RSA 公開鍵)や AuthSwitchRequest:
			// クライアントに渡し、その応答を backend に返す
			if len(p.body) > 0 && p.body[0] == 0xfe {
				_, nonce = parseAuthSwitch(p.body)
			}
			if err := writePacket(client, packet{seq: cseq, body: p.body}); err != nil {
				return err
			}
			c, err := readPacket(client)
			if err != nil {
				return err
			}
			cseq = c.seq
			bseq++
			if err := writePacket(backend, packet{seq: bseq, body: c.body}); err != nil {
				return err
			}
		}
	}
}

// closeWriter は書き込み側だけを閉じられる接続(*net.TCPConn / *tls.Conn)。
type closeWriter interface{ CloseWrite() error }

// pipe は認証後のデータフェーズを双方向に素通しする。片方向の EOF で
// 全体を叩き切るのではなく、その向きだけ CloseWrite で half-close して
// もう片方向を完走させる。これにより:
//   - backend が結果セットを流し込んでいる最中に client 側が先に終わっても、
//     結果を途中でぶった切らない
//   - backend(mysqld)が idle_stop_after で消えたときは client へ綺麗な EOF が
//     伝わり、プールしたコネクションを再利用するドライバが「半端に閉じた/残
//     バイトのある」接続を掴んで readColumns で panic するのを防ぐ
func pipe(client, backend net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(backend, client) // client → backend(リクエスト)
		if cw, ok := backend.(closeWriter); ok {
			_ = cw.CloseWrite() // これ以上リクエストは来ない、と backend に伝える
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, backend) // backend → client(応答)
		if cw, ok := client.(closeWriter); ok {
			_ = cw.CloseWrite() // 応答完了/backend 消滅を client へ綺麗な EOF で伝える
		}
	}()
	wg.Wait()
}

// enableKeepAlive は TCP keepalive を有効化して、消えた peer を早く検知する。
func enableKeepAlive(c net.Conn) {
	if t, ok := c.(*net.TCPConn); ok {
		_ = t.SetKeepAlive(true)
		_ = t.SetKeepAlivePeriod(30 * time.Second)
	}
}

// authenticateBackend は sashiki がクライアントとして backend mysqld へ
// caching_sha2_password で認証する(方式A)。backend の dev は caching_sha2 で
// 作成される(#version-compat)。平文 TCP の cold cache では full-auth になるため
// RSA 公開鍵でパスワードを送る。AuthSwitch には plugin にあわせて応答する。
func (s *Server) authenticateBackend(backend net.Conn, clientCaps uint32, database string) error {
	bhs, err := readPacket(backend)
	if err != nil {
		return fmt.Errorf("read backend handshake: %w", err)
	}
	salt, backendCaps, err := parseBackendHandshake(bhs.body)
	if err != nil {
		return err
	}
	// backend への capability 申告は「クライアントが実際に交渉した capability」に
	// 合わせる(#125)。特に DEPRECATE_EOF を揃えないと、backend が deprecate 形式の
	// 結果セット(中間 EOF 無し・末尾 OK)を返し、DEPRECATE_EOF を立てないドライバ
	// (PHP mysqlnd / Node 等)が読めず結果が空/エラーになる。以前は synthCaps を
	// 固定申告していたため、非 deprecate クライアントで壊れていた。
	//
	// DB 選択(capConnectWithDB)だけは sashiki 側で制御する: クライアントが接続時に
	// 指定した DB を backend にも引き継ぐ(go-sql-driver 等は handshake の database
	// フィールドでのみ DB を選ぶため、転送しないと "No database selected" になる)。
	hr := handshakeResponse{maxLen: 16 * 1024 * 1024, charset: 0xff}
	hr.caps = clientCaps & synthCaps
	if database != "" {
		hr.caps |= capConnectWithDB
		hr.database = database
	} else {
		hr.caps &^= capConnectWithDB
	}
	// backend の dev は caching_sha2_password で作成される(MySQL 8.0/8.4/9.x 共通。
	// 8.4 は native 既定 OFF、9.x は native 廃止のため、#version-compat)。初回応答は
	// caching_sha2 の fast-auth スクランブル。nonce は AuthSwitch で更新され得る。
	nonce := salt
	token := cachingSha2Token(s.cfg.AppPassword, nonce)
	resp := buildBackendHandshakeResponse(hr, backendCaps, s.cfg.AppUser, token, sha2Plugin)
	if err := writePacket(backend, packet{seq: bhs.seq + 1, body: resp}); err != nil {
		return err
	}
	for {
		p, err := readPacket(backend)
		if err != nil {
			return fmt.Errorf("read backend auth result: %w", err)
		}
		switch {
		case isOK(p.body):
			return nil
		case isErr(p.body):
			return fmt.Errorf("backend rejected app credential: %s", errText(p.body))
		case len(p.body) >= 2 && p.body[0] == 0x01: // AuthMoreData(caching_sha2)
			switch p.body[1] {
			case 0x03: // fast_auth_success → 次は OK
				continue
			case 0x04: // perform_full_authentication → 平文 TCP なので RSA 公開鍵で送る
				if err := s.backendFullAuthRSA(backend, p.seq+1, nonce); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unexpected caching_sha2 auth status 0x%02x", p.body[1])
			}
		case len(p.body) > 0 && p.body[0] == 0xfe: // AuthSwitchRequest → 指定 plugin で応答
			plugin, swSalt := parseAuthSwitch(p.body)
			nonce = swSalt
			var tok []byte
			if plugin == nativePlugin {
				tok = nativeToken(s.cfg.AppPassword, swSalt)
			} else {
				tok = cachingSha2Token(s.cfg.AppPassword, swSalt)
			}
			if err := writePacket(backend, packet{seq: p.seq + 1, body: tok}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected backend auth packet 0x%02x", p.body[0])
		}
	}
}

// backendFullAuthRSA は caching_sha2 の full-auth を平文接続で完了させる。公開鍵を
// 要求(0x02)→ PEM 受領 → パスワード(null 終端)を nonce で XOR して RSA-OAEP で
// 暗号化して送る(TLS 無しでも安全に送るための標準手順)。
func (s *Server) backendFullAuthRSA(backend net.Conn, seq byte, nonce []byte) error {
	return backendFullAuthRSAWith(backend, seq, nonce, s.cfg.AppPassword)
}

// backendFullAuthRSAWith は任意のパスワードで RSA full-auth を行う。passthrough(#394)で
// クライアント側が TLS(平文パスワードが来る)・backend 側が平文 TCP のとき、
// sashiki が RSA に詰め替えて渡すのに使う。
func backendFullAuthRSAWith(backend net.Conn, seq byte, nonce []byte, password string) error {
	if err := writePacket(backend, packet{seq: seq, body: []byte{0x02}}); err != nil {
		return err
	}
	p, err := readPacket(backend)
	if err != nil {
		return fmt.Errorf("read backend rsa public key: %w", err)
	}
	if len(p.body) < 2 || p.body[0] != 0x01 {
		return fmt.Errorf("unexpected backend public-key packet 0x%02x", firstByte(p.body))
	}
	block, _ := pem.Decode(p.body[1:])
	if block == nil {
		return fmt.Errorf("backend public key not PEM")
	}
	pubAny, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse backend public key: %w", err)
	}
	pub, ok := pubAny.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("backend public key is not RSA")
	}
	if len(nonce) == 0 {
		return fmt.Errorf("empty auth nonce for backend full-auth")
	}
	plain := append([]byte(password), 0) // null 終端
	xored := make([]byte, len(plain))
	for i := range plain {
		xored[i] = plain[i] ^ nonce[i%len(nonce)]
	}
	enc, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, pub, xored, nil)
	if err != nil {
		return fmt.Errorf("rsa encrypt backend password: %w", err)
	}
	return writePacket(backend, packet{seq: p.seq + 1, body: enc})
}

func firstByte(b []byte) byte {
	if len(b) == 0 {
		return 0
	}
	return b[0]
}

// parseAuthSwitch は AuthSwitchRequest(0xfe + plugin\0 + salt)から plugin 名と
// salt を取る。
func parseAuthSwitch(body []byte) (plugin string, salt []byte) {
	pos := 1
	start := pos
	for pos < len(body) && body[pos] != 0 { // plugin name
		pos++
	}
	plugin = string(body[start:pos])
	pos++ // null
	salt = body[pos:]
	for len(salt) > 0 && salt[len(salt)-1] == 0 { // 末尾 null を落とす
		salt = salt[:len(salt)-1]
	}
	return plugin, salt
}
