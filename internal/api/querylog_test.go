package api

import "testing"

// 監査ログは SQL 本文を含めない(#310 review)。種別と digest だけ。
func TestQueryLogFieldsCarryNoLiterals(t *testing.T) {
	sql := "  (INSERT INTO users (token) VALUES ('sashiki_secret'))"
	if k := sqlKind(sql); k != "INSERT" {
		t.Errorf("kind = %q", k)
	}
	if d := sqlDigest(sql); len(d) != 12 || d == sqlDigest("other") {
		t.Errorf("digest = %q", d)
	}
	if sqlKind("") != "?" {
		t.Error("empty sql kind")
	}
}
