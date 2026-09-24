// MySQL プロトコルのパケット読み書きと、認証フェーズに必要な最小限の
// パース/構築。認証完了後は素通しするため、ここで扱うのは
// handshake / handshake response / auth switch / OK / ERR のみ。
package proxy

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"io"
)

// capability flags (使う分だけ)
const (
	capLongPassword    = 0x00000001
	capProtocol41      = 0x00000200
	capSSL             = 0x00000800
	capTransactions    = 0x00002000
	capSecureConn      = 0x00008000
	capPluginAuth      = 0x00080000
	capConnectAttrs    = 0x00100000
	capPluginAuthLenC  = 0x00200000
	capDeprecateEOF    = 0x01000000
	capConnectWithDB   = 0x00000008
	capLocalFiles      = 0x00000080
	capMultiStatements = 0x00010000
	capMultiResults    = 0x00020000
)

const (
	nativePlugin = "mysql_native_password"
	sha2Plugin   = "caching_sha2_password"
)

// packet は 1 パケット(ヘッダ除くペイロード + シーケンス番号)。
type packet struct {
	seq  byte
	body []byte
}

func readPacket(r io.Reader) (packet, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return packet{}, err
	}
	length := int(head[0]) | int(head[1])<<8 | int(head[2])<<16
	if length > 16*1024*1024 {
		return packet{}, fmt.Errorf("packet too large: %d", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return packet{}, err
	}
	return packet{seq: head[3], body: body}, nil
}

func writePacket(w io.Writer, p packet) error {
	head := []byte{
		byte(len(p.body)), byte(len(p.body) >> 8), byte(len(p.body) >> 16), p.seq,
	}
	if _, err := w.Write(head); err != nil {
		return err
	}
	_, err := w.Write(p.body)
	return err
}

// synthCaps は sashiki が合成ハンドシェイクで広告する capability。
// クライアントはこの範囲でしか機能を使わないため、バックエンドへの申告も
// 必ずこの範囲に絞る(広告していない機能を申告すると、クライアントが送る
// パケットとバックエンドの期待がずれて Malformed packet になる)。
const synthCaps = uint32(capLongPassword | capProtocol41 | capSecureConn | capPluginAuth |
	capPluginAuthLenC | capTransactions | capConnectWithDB | capDeprecateEOF |
	capMultiStatements | capMultiResults)

// buildInitialHandshake は sashiki が名乗る合成ハンドシェイク(protocol 10)。
// 返す salt(20byte)は方式A(#51)でクライアント認証の検証に使う。
// sslAvailable が true のとき capSSL を広告し、クライアントの TLS 要求を受け入れる。
func buildInitialHandshake(connID uint32, sslAvailable bool) ([]byte, []byte, error) {
	salt := make([]byte, 20)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, err
	}
	// salt に 0x00 が混ざると null 終端と衝突するため避ける
	for i := range salt {
		salt[i] = salt[i]%94 + 33
	}
	caps := synthCaps
	if sslAvailable {
		caps |= capSSL
	}

	b := []byte{10} // protocol version
	// server version は固定(ADR-009)。方式A では user@branch を見るまで接続先が
	// 決まらないので backend の版は映せない。8.0 世代を名乗ればクライアントは
	// caching_sha2 / utf8mb4 の既定で話し、以降の版(8.4 / 26.7)ともプロトコル互換。
	b = append(b, []byte("8.0.0-sashiki-proxy")...)
	b = append(b, 0)                                // null terminator
	b = binary.LittleEndian.AppendUint32(b, connID) // thread id
	b = append(b, salt[:8]...)                      // auth-plugin-data part1
	b = append(b, 0)                                // filler
	b = append(b, byte(caps), byte(caps>>8))        // capabilities low
	b = append(b, 0xff)                             // charset (utf8mb4)
	b = append(b, 0x02, 0x00)                       // status flags
	b = append(b, byte(caps>>16), byte(caps>>24))   // capabilities high
	b = append(b, 21)                               // auth plugin data len (20+1)
	b = append(b, make([]byte, 10)...)              // reserved
	b = append(b, salt[8:20]...)                    // auth-plugin-data part2
	b = append(b, 0)
	// caching_sha2_password を広告する(#197)。MySQL 8.0 既定 / 9.x(native 廃止)の
	// クライアントに対応。native しか話せない旧クライアントには authResp の長さで
	// フォールバックする(authTerminate 参照)。
	b = append(b, []byte(sha2Plugin)...)
	b = append(b, 0)
	return b, salt, nil
}

// nativeToken は mysql_native_password のクライアント応答トークンを計算する。
//
//	token = SHA1(pass) XOR SHA1(salt || SHA1(SHA1(pass)))
//
// 空パスワードは空トークン。
func nativeToken(password string, salt []byte) []byte {
	if password == "" {
		return nil
	}
	stage1 := sha1.Sum([]byte(password))
	stage2 := sha1.Sum(stage1[:])
	h := sha1.New()
	h.Write(salt)
	h.Write(stage2[:])
	scr := h.Sum(nil)
	out := make([]byte, len(stage1))
	for i := range stage1 {
		out[i] = stage1[i] ^ scr[i]
	}
	return out
}

// verifyNativePassword は salt に対するクライアント応答 token が password と
// 一致するかを定時間比較で検証する(方式A: 認証終端)。
func verifyNativePassword(password string, salt, token []byte) bool {
	return subtle.ConstantTimeCompare(nativeToken(password, salt), token) == 1
}

// cachingSha2Token は caching_sha2_password のクライアント応答スクランブルを計算する。
//
//	token = SHA256(pass) XOR SHA256( SHA256(SHA256(pass)) || nonce )
//
// nonce は 20byte の auth-plugin-data(salt)。空パスワードは空トークン。MySQL 8.0
// 既定 / 9.x(native 廃止)のクライアントはこれを使う(#197)。
func cachingSha2Token(password string, nonce []byte) []byte {
	if password == "" {
		return nil
	}
	h1 := sha256.Sum256([]byte(password))
	h2 := sha256.Sum256(h1[:])
	h := sha256.New()
	h.Write(h2[:])
	h.Write(nonce)
	scr := h.Sum(nil)
	out := make([]byte, len(h1))
	for i := range h1 {
		out[i] = h1[i] ^ scr[i]
	}
	return out
}

// verifyCachingSha2Password は nonce に対するクライアント応答 token を定時間比較する。
func verifyCachingSha2Password(password string, nonce, token []byte) bool {
	return subtle.ConstantTimeCompare(cachingSha2Token(password, nonce), token) == 1
}

// isSSLRequest は HandshakeResponse が SSLRequest(TLS へ切り替える短いパケット)
// かどうか。SSLRequest は caps+maxlen+charset+reserved(23)のみで username が無い。
func isSSLRequest(body []byte) bool {
	if len(body) < 4 {
		return false
	}
	caps := binary.LittleEndian.Uint32(body[0:4])
	return caps&capSSL != 0 && len(body) <= 36
}

// buildOK は認証成功時にクライアントへ返す OK パケット(protocol 41)。
func buildOK() []byte {
	return []byte{
		0x00,       // OK header
		0x00,       // affected_rows (lenenc 0)
		0x00,       // last_insert_id (lenenc 0)
		0x02, 0x00, // status flags (SERVER_STATUS_AUTOCOMMIT)
		0x00, 0x00, // warnings
	}
}

// handshakeResponse は client の HandshakeResponse41 のうち必要な項目。
type handshakeResponse struct {
	caps     uint32
	maxLen   uint32
	charset  byte
	username string
	database string
	authResp []byte // 方式A(#51): salt に対するクライアント認証トークン
}

func parseHandshakeResponse(body []byte) (handshakeResponse, error) {
	var r handshakeResponse
	if len(body) < 32 {
		return r, fmt.Errorf("handshake response too short")
	}
	r.caps = binary.LittleEndian.Uint32(body[0:4])
	if r.caps&capProtocol41 == 0 {
		return r, fmt.Errorf("client does not speak protocol 4.1")
	}
	r.maxLen = binary.LittleEndian.Uint32(body[4:8])
	r.charset = body[8]
	pos := 32 // 4+4+1+23
	// username (null 終端)
	end := pos
	for end < len(body) && body[end] != 0 {
		end++
	}
	if end >= len(body) {
		return r, fmt.Errorf("username not terminated")
	}
	r.username = string(body[pos:end])
	pos = end + 1
	// auth response(方式A ではこれを検証に使う)
	if r.caps&capPluginAuthLenC != 0 {
		if pos >= len(body) {
			return r, fmt.Errorf("truncated auth data")
		}
		alen := int(body[pos])
		pos++
		if pos+alen > len(body) {
			return r, fmt.Errorf("truncated auth data")
		}
		r.authResp = body[pos : pos+alen]
		pos += alen
	} else if r.caps&capSecureConn != 0 {
		if pos >= len(body) {
			return r, fmt.Errorf("truncated auth data")
		}
		alen := int(body[pos])
		pos++
		if pos+alen > len(body) {
			return r, fmt.Errorf("truncated auth data")
		}
		r.authResp = body[pos : pos+alen]
		pos += alen
	} else {
		start := pos
		for pos < len(body) && body[pos] != 0 {
			pos++
		}
		r.authResp = body[start:pos]
		pos++
	}
	if pos > len(body) {
		return r, fmt.Errorf("truncated packet")
	}
	// database (CLIENT_CONNECT_WITH_DB)
	if r.caps&capConnectWithDB != 0 && pos < len(body) {
		end = pos
		for end < len(body) && body[end] != 0 {
			end++
		}
		r.database = string(body[pos:end])
	}
	return r, nil
}

// parseBackendHandshake はバックエンド mysqld のハンドシェイクから salt と caps を取る。
func parseBackendHandshake(body []byte) (salt []byte, caps uint32, err error) {
	if len(body) < 1 || body[0] != 10 {
		return nil, 0, fmt.Errorf("unexpected protocol version")
	}
	pos := 1
	for pos < len(body) && body[pos] != 0 { // server version
		pos++
	}
	pos++    // null
	pos += 4 // thread id
	if pos+8 > len(body) {
		return nil, 0, fmt.Errorf("short handshake")
	}
	salt = append(salt, body[pos:pos+8]...)
	pos += 8
	pos++ // filler
	if pos+2 > len(body) {
		return nil, 0, fmt.Errorf("short handshake")
	}
	caps = uint32(binary.LittleEndian.Uint16(body[pos : pos+2]))
	pos += 2
	if pos+1+2+2+1+10 <= len(body) {
		pos++    // charset
		pos += 2 // status
		caps |= uint32(binary.LittleEndian.Uint16(body[pos:pos+2])) << 16
		pos += 2
		authLen := int(body[pos])
		pos++
		pos += 10 // reserved
		// salt part2: max(13, authLen-8) だが末尾 null を除いた 12 byte が慣例
		rest := 12
		if authLen > 0 && authLen-8-1 < rest {
			rest = authLen - 8 - 1
		}
		if pos+rest <= len(body) {
			salt = append(salt, body[pos:pos+rest]...)
		}
	}
	return salt, caps, nil
}

// buildBackendHandshakeResponse はバックエンドへ送る HandshakeResponse41。
// plugin にユーザーの実プラグインと異なる名前を渡すと、バックエンドは必ず
// AuthSwitchRequest を返す(proxy はこれを利用する)。
func buildBackendHandshakeResponse(r handshakeResponse, backendCaps uint32, user string, authResp []byte, plugin string) []byte {
	caps := r.caps & backendCaps & synthCaps
	caps |= capProtocol41 | capSecureConn | capPluginAuth

	b := binary.LittleEndian.AppendUint32(nil, caps)
	b = binary.LittleEndian.AppendUint32(b, r.maxLen)
	b = append(b, r.charset)
	b = append(b, make([]byte, 23)...)
	b = append(b, []byte(user)...)
	b = append(b, 0)
	b = append(b, byte(len(authResp)))
	b = append(b, authResp...)
	// capConnectWithDB を広告したら db フィールドは必須(空でも null を書く)。
	// 省くと backend が後続の plugin 名を DB 名として誤読する。
	if caps&capConnectWithDB != 0 {
		b = append(b, []byte(r.database)...)
		b = append(b, 0)
	}
	b = append(b, []byte(plugin)...)
	b = append(b, 0)
	return b
}

// buildErr は ERR パケットを作る(認証前フェーズ用)。
func buildErr(code uint16, sqlState, msg string) []byte {
	b := []byte{0xff}
	b = binary.LittleEndian.AppendUint16(b, code)
	b = append(b, '#')
	b = append(b, []byte(sqlState)...)
	b = append(b, []byte(msg)...)
	return b
}

func isOK(body []byte) bool  { return len(body) > 0 && body[0] == 0x00 }
func isErr(body []byte) bool { return len(body) > 0 && body[0] == 0xff }

// errText は ERR パケットから人間可読なメッセージ(code + 本文)を取り出す。
func errText(body []byte) string {
	if len(body) < 3 {
		return "unknown error"
	}
	code := binary.LittleEndian.Uint16(body[1:3])
	msg := body[3:]
	if len(msg) > 6 && msg[0] == '#' { // '#' + 5 桁 SQLSTATE
		msg = msg[6:]
	}
	return fmt.Sprintf("%d %s", code, string(msg))
}
