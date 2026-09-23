package pgproxy

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// SCRAM-SHA-256(RFC 5802 / RFC 7677)。PostgreSQL 10 以降の既定認証方式で、
// md5 と違いパスワードそのものも md5 ハッシュも回線に出ない。
//
// sashiki は「認証終端(方式A)」なので app パスワードの平文を持っている。
// サーバ役では接続ごとにランダム salt を作って SaltedPassword を都度計算し、
// クライアント役(バックエンドへ接続する側)ではサーバから来た salt を使う。
//
// 注意: RFC は SASLprep(RFC 4013)によるパスワード正規化を要求するが、
// ここでは ASCII パスワードを前提に素通しする(非 ASCII は将来対応)。

const scramIterations = 4096

// scramKeys は SaltedPassword から派生する鍵一式。
type scramKeys struct {
	salted    []byte
	clientKey []byte
	storedKey []byte
	serverKey []byte
}

func deriveScramKeys(password string, salt []byte, iter int) (scramKeys, error) {
	salted, err := pbkdf2.Key(sha256.New, password, salt, iter, sha256.Size)
	if err != nil {
		return scramKeys{}, err
	}
	clientKey := hmacSHA256(salted, []byte("Client Key"))
	stored := sha256.Sum256(clientKey)
	return scramKeys{
		salted:    salted,
		clientKey: clientKey,
		storedKey: stored[:],
		serverKey: hmacSHA256(salted, []byte("Server Key")),
	}, nil
}

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

func xorBytes(a, b []byte) []byte {
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// nonce は base64 で安全に運べるランダム文字列を返す。
func nonce() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// --- サーバ役(クライアントを検証する) ---

// scramServer は SCRAM-SHA-256 のサーバ側状態。
type scramServer struct {
	password string

	gs2Header      string // "n,," / "y,,"
	clientFirstBar string // "n=,r=<cnonce>"
	serverFirst    string
	combinedNonce  string
	keys           scramKeys
}

// firstReply は client-first-message を受けて server-first-message を返す。
// data は SASLInitialResponse のメカニズム固有データ。
func (s *scramServer) firstReply(data []byte) (string, error) {
	// gs2 ヘッダは "n,,"(チャネルバインディング無し)か "y,,"(クライアントは
	// 対応しているがサーバが広告しなかった、と判断した場合)。どちらも
	// SCRAM-SHA-256(非 PLUS)では正常。
	msg := string(data)
	parts := strings.SplitN(msg, ",", 3)
	if len(parts) < 3 {
		return "", fmt.Errorf("malformed client-first-message")
	}
	switch parts[0] {
	case "n", "y":
	default:
		// "p" はチャネルバインディング必須。SCRAM-SHA-256-PLUS を広告して
		// いないので来ないはず。
		return "", fmt.Errorf("channel binding is not supported")
	}
	s.gs2Header = parts[0] + "," + parts[1] + ","
	s.clientFirstBar = parts[2]

	cnonce := ""
	for _, kv := range strings.Split(s.clientFirstBar, ",") {
		if strings.HasPrefix(kv, "r=") {
			cnonce = kv[2:]
		}
	}
	if cnonce == "" {
		return "", fmt.Errorf("client nonce is missing")
	}
	snonce, err := nonce()
	if err != nil {
		return "", err
	}
	s.combinedNonce = cnonce + snonce

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	if s.keys, err = deriveScramKeys(s.password, salt, scramIterations); err != nil {
		return "", err
	}
	s.serverFirst = fmt.Sprintf("r=%s,s=%s,i=%d",
		s.combinedNonce, base64.StdEncoding.EncodeToString(salt), scramIterations)
	return s.serverFirst, nil
}

// finalReply は client-final-message を検証し、server-final-message を返す。
func (s *scramServer) finalReply(data []byte) (string, error) {
	clientFinal := string(data)
	var channel, rnonce, proofB64 string
	for _, kv := range strings.Split(clientFinal, ",") {
		switch {
		case strings.HasPrefix(kv, "c="):
			channel = kv[2:]
		case strings.HasPrefix(kv, "r="):
			rnonce = kv[2:]
		case strings.HasPrefix(kv, "p="):
			proofB64 = kv[2:]
		}
	}
	// c= は gs2 ヘッダの base64。クライアントが最初に申告したものと一致すること。
	if channel != base64.StdEncoding.EncodeToString([]byte(s.gs2Header)) {
		return "", fmt.Errorf("channel binding mismatch")
	}
	if rnonce != s.combinedNonce {
		return "", fmt.Errorf("nonce mismatch")
	}
	proof, err := base64.StdEncoding.DecodeString(proofB64)
	if err != nil || len(proof) != sha256.Size {
		return "", fmt.Errorf("malformed client proof")
	}

	withoutProof := clientFinal
	if i := strings.Index(clientFinal, ",p="); i >= 0 {
		withoutProof = clientFinal[:i]
	}
	authMessage := s.clientFirstBar + "," + s.serverFirst + "," + withoutProof

	clientSig := hmacSHA256(s.keys.storedKey, []byte(authMessage))
	// ClientProof = ClientKey XOR ClientSignature なので、逆算した ClientKey の
	// SHA256 が StoredKey と一致すればパスワードが正しい。
	recovered := xorBytes(proof, clientSig)
	got := sha256.Sum256(recovered)
	if subtle.ConstantTimeCompare(got[:], s.keys.storedKey) != 1 {
		return "", errPasswordMismatch
	}
	serverSig := hmacSHA256(s.keys.serverKey, []byte(authMessage))
	return "v=" + base64.StdEncoding.EncodeToString(serverSig), nil
}

// --- クライアント役(バックエンドへ接続する) ---

// errPasswordMismatch はパスワード不一致。authlimit の Fail に数えるのはこれだけで、
// SCRAM 途中の切断・不正メッセージは数えない(#327: ネットワーク障害でリトライする
// クライアントが tarpit されていた。MySQL 側も parse 失敗は数えない)。
var errPasswordMismatch = errors.New("password verification failed")

// scramClient は SCRAM-SHA-256 のクライアント側状態。
type scramClient struct {
	password string

	clientFirstBar string
	cnonce         string
	authMessage    string
	keys           scramKeys
}

// first は client-first-message(gs2 ヘッダ込み)を返す。
func (c *scramClient) first() (string, error) {
	n, err := nonce()
	if err != nil {
		return "", err
	}
	c.cnonce = n
	c.clientFirstBar = "n=,r=" + n
	return "n,," + c.clientFirstBar, nil
}

// final は server-first-message を受けて client-final-message を返す。
func (c *scramClient) final(serverFirst string) (string, error) {
	var rnonce, saltB64, iterStr string
	for _, kv := range strings.Split(serverFirst, ",") {
		switch {
		case strings.HasPrefix(kv, "r="):
			rnonce = kv[2:]
		case strings.HasPrefix(kv, "s="):
			saltB64 = kv[2:]
		case strings.HasPrefix(kv, "i="):
			iterStr = kv[2:]
		}
	}
	if !strings.HasPrefix(rnonce, c.cnonce) {
		return "", fmt.Errorf("server nonce does not extend client nonce")
	}
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return "", fmt.Errorf("malformed server salt")
	}
	iter, err := strconv.Atoi(iterStr)
	if err != nil || iter <= 0 {
		return "", fmt.Errorf("malformed iteration count")
	}
	if c.keys, err = deriveScramKeys(c.password, salt, iter); err != nil {
		return "", err
	}

	withoutProof := "c=" + base64.StdEncoding.EncodeToString([]byte("n,,")) + ",r=" + rnonce
	c.authMessage = c.clientFirstBar + "," + serverFirst + "," + withoutProof
	clientSig := hmacSHA256(c.keys.storedKey, []byte(c.authMessage))
	proof := xorBytes(c.keys.clientKey, clientSig)
	return withoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof), nil
}

// verifyServerFinal は server-final-message(v=...)を検証する。
func (c *scramClient) verifyServerFinal(serverFinal string) error {
	var sigB64 string
	for _, kv := range strings.Split(serverFinal, ",") {
		if strings.HasPrefix(kv, "v=") {
			sigB64 = kv[2:]
		}
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("malformed server signature")
	}
	want := hmacSHA256(c.keys.serverKey, []byte(c.authMessage))
	if subtle.ConstantTimeCompare(sig, want) != 1 {
		return fmt.Errorf("server signature mismatch")
	}
	return nil
}
