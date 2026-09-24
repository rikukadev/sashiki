package pgproxy

import (
	"errors"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// saslprep は RFC 4013(SASLprep、stringprep プロファイル)でパスワードを正規化する
// (#283)。SCRAM-SHA-256(RFC 5802)は StoredKey / ClientKey の導出前に
// SASLprep(password) を要求し、PostgreSQL もサーバ・libpq の両方で行う。
// proxy は client 検証(サーバ役)と backend 接続(クライアント役)の両 leg で
// 同じ app_pass を使うので、どちらも同じ正規化を通さないと非 ASCII の
// パスワードで認証が食い違う。
//
// 手順(RFC 4013 §2):
//  1. マッピング: 非 ASCII の空白(C.1.2)→ U+0020、map-to-nothing(B.1)→ 削除
//  2. 正規化: NFKC
//  3. 禁止文字(C.1.2 / C.2.1 / C.2.2 / C.3〜C.9)があればエラー
//  4. 双方向: RandALCat(右書き)文字を含むなら LCat を含んではならず、先頭と末尾が RandALCat
//
// エラー時の扱いは呼び出し側(prepPassword)で PostgreSQL に合わせる。
func saslprep(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("saslprep: invalid UTF-8")
	}
	// 1. mapping
	mapped := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case isMapToNothing(r):
			continue
		case isNonASCIISpace(r):
			mapped = append(mapped, ' ')
		default:
			mapped = append(mapped, r)
		}
	}
	// 2. NFKC
	out := norm.NFKC.String(string(mapped))
	// 3. prohibited
	hasRandAL, hasL := false, false
	var first, last rune
	for i, r := range out {
		if isProhibited(r) {
			return "", errors.New("saslprep: prohibited character")
		}
		if i == 0 {
			first = r
		}
		last = r
		if isRandALCat(r) {
			hasRandAL = true
		} else if isLCat(r) {
			hasL = true
		}
	}
	// 4. bidi(RFC 3454 §6)
	if hasRandAL {
		if hasL || !isRandALCat(first) || !isRandALCat(last) {
			return "", errors.New("saslprep: bidirectional check failed")
		}
	}
	return out, nil
}

// prepPassword は SASLprep を適用し、失敗したら元の文字列を返す。PostgreSQL は
// 不正 UTF-8 / 禁止文字のとき pg_saslprep が失敗した入力をそのまま使う
// (scram_build_secret / libpq の pg_fe_scram_*)ので、同じ挙動にして互換を保つ。
func prepPassword(p string) string {
	if out, err := saslprep(p); err == nil {
		return out
	}
	return p
}

// B.1 map-to-nothing(RFC 3454 附属書 B.1)。
func isMapToNothing(r rune) bool {
	switch r {
	case 0x00AD, 0x034F, 0x1806, 0x180B, 0x180C, 0x180D, 0x200B, 0x200C, 0x200D, 0x2060, 0xFEFF:
		return true
	}
	if r >= 0xFE00 && r <= 0xFE0F {
		return true
	}
	return false
}

// C.1.2 非 ASCII の空白。
func isNonASCIISpace(r rune) bool {
	switch r {
	case 0x00A0, 0x1680, 0x202F, 0x205F, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200B
}

// isProhibited は SASLprep が禁止する文字(RFC 4013 §2.3)。
//   - C.1.2 非 ASCII 空白(マッピング後にも残らないが念のため)
//   - C.2.1 ASCII 制御、C.2.2 非 ASCII 制御
//   - C.3 私用領域、C.4 非文字、C.5 サロゲート、C.6 プレーンテキスト不適合、
//     C.7 正規化不適合、C.8 表示方向の変更、C.9 タグ文字
func isProhibited(r rune) bool {
	switch {
	case isNonASCIISpace(r):
		return true
	case r < 0x20 || r == 0x7F: // C.2.1
		return true
	case r >= 0x80 && r <= 0x9F: // C.2.2
		return true
	case r == 0x06DD || r == 0x070F || r == 0x180E || (r >= 0x200C && r <= 0x200D) || (r >= 0x2028 && r <= 0x2029) ||
		(r >= 0x2060 && r <= 0x2063) || (r >= 0x206A && r <= 0x206F) || r == 0xFEFF || (r >= 0xFFF9 && r <= 0xFFFC) ||
		(r >= 0x1D173 && r <= 0x1D17A): // C.2.2 の残り
		return true
	case (r >= 0xE000 && r <= 0xF8FF) || (r >= 0xF0000 && r <= 0xFFFFD) || (r >= 0x100000 && r <= 0x10FFFD): // C.3
		return true
	case (r >= 0xFDD0 && r <= 0xFDEF) || r&0xFFFE == 0xFFFE: // C.4(U+xxFFFE / U+xxFFFF)
		return true
	case r >= 0xD800 && r <= 0xDFFF: // C.5
		return true
	case r >= 0xFFF9 && r <= 0xFFFD: // C.6
		return true
	case r >= 0x2FF0 && r <= 0x2FFB: // C.7
		return true
	case r == 0x0340 || r == 0x0341 || r == 0x200E || r == 0x200F || (r >= 0x202A && r <= 0x202E) || (r >= 0x206A && r <= 0x206F): // C.8
		return true
	case r == 0xE0001 || (r >= 0xE0020 && r <= 0xE007F): // C.9
		return true
	}
	return false
}

// isRandALCat は RFC 3454 D.1(右書き: Arabic / Hebrew 系)。Unicode の bidi class R / AL。
func isRandALCat(r rune) bool {
	return unicode.In(r, unicode.Hebrew, unicode.Arabic, unicode.Syriac, unicode.Thaana, unicode.Nko) ||
		r == 0x05BE || r == 0x05C0 || r == 0x05C3 || r == 0x200F || (r >= 0xFB1D && r <= 0xFDFD) || (r >= 0xFE70 && r <= 0xFEFC)
}

// isLCat は RFC 3454 D.2(左書き)。bidi class L の近似として、右書きでも
// 記号 / 数字 / 制御でもない文字を左書きとみなす。
func isLCat(r rune) bool {
	if isRandALCat(r) {
		return false
	}
	return unicode.IsLetter(r) && !unicode.Is(unicode.Common, r)
}
