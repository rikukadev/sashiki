package pgproxy

import "testing"

// RFC 4013 §3 の例と PostgreSQL の saslprep テスト(src/test/regress/sql/password.sql 相当)。
func TestSaslprepExamples(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"I\u00adX", "IX", true},             // soft hyphen は map-to-nothing
		{"user", "user", true},          // 変化なし
		{"USER", "USER", true},          // 大文字は保存(SASLprep は case-fold しない)
		{"ª", "a", true},                // ª → a(NFKC)
		{"Ⅸ", "IX", true},               // Ⅸ → IX(NFKC)
		{" x", " x", true},              // 非 ASCII 空白 → U+0020
		{"pässword", "pässword", true},  // 合成済みラテン文字はそのまま
		{"pässword", "pässword", true}, // 結合文字は NFKC で合成
		{"\u0007", "", false},           // 制御文字は禁止
		{"ا1", "", false},               // RandALCat の末尾が数字 → bidi 違反
		{"اب", "اب", true},              // 右書きだけならよい
		{"اa", "", false},               // 右書きと左書きの混在は禁止
		{"foo", "", false},             // 私用領域は禁止
		{"\xff\xfe", "", false},         // 不正 UTF-8
	}
	for _, c := range cases {
		got, err := saslprep(c.in)
		if (err == nil) != c.ok {
			t.Errorf("saslprep(%q): err=%v, want ok=%v", c.in, err, c.ok)
			continue
		}
		if c.ok && got != c.want {
			t.Errorf("saslprep(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// 失敗したら元の文字列を使う(PostgreSQL 互換)
	if prepPassword("\u0007") != "\u0007" {
		t.Error("prepPassword should fall back to the raw password when SASLprep fails")
	}
}

// 正規化の違う書き方(NFC / NFD / 互換文字)のパスワードでも SCRAM が成立する:
// サーバ役とクライアント役の両方が同じ SASLprep を通るため。
func TestScramRoundTripWithUnicodePassword(t *testing.T) {
	for _, pair := range [][2]string{
		{"pässⅨ", "pässIX"},   // 結合文字 + ローマ数字 ↔ 合成済み + ASCII
		{"I\u00adX", "IX"},          // soft hyphen ↔ 無し
		{" secret", " secret"}, // NBSP ↔ 空白
	} {
		cl := &scramClient{password: pair[0]}
		sv := &scramServer{password: pair[1]}
		clientFirst, err := cl.first()
		if err != nil {
			t.Fatal(err)
		}
		serverFirst, err := sv.firstReply([]byte(clientFirst))
		if err != nil {
			t.Fatal(err)
		}
		clientFinal, err := cl.final(serverFirst)
		if err != nil {
			t.Fatal(err)
		}
		serverFinal, err := sv.finalReply([]byte(clientFinal))
		if err != nil {
			t.Errorf("%q vs %q: server should accept after SASLprep: %v", pair[0], pair[1], err)
			continue
		}
		if err := cl.verifyServerFinal(serverFinal); err != nil {
			t.Errorf("%q: client should accept the server signature: %v", pair[0], err)
		}
	}
}
