package api

import (
	"strings"
	"testing"
	"time"
)

// parseExpiry は相対(ttl / for)と絶対(expires_at / until)を排他で受ける(#381)。
func TestParseExpiry(t *testing.T) {
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)

	t.Run("どちらも空なら zero(期限なし)", func(t *testing.T) {
		got, err := parseExpiry("", "", "ttl", "expires_at")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.IsZero() {
			t.Errorf("got %v, want zero", got)
		}
	})

	t.Run("絶対時刻はそのまま通る", func(t *testing.T) {
		got, err := parseExpiry("", future, "ttl", "expires_at")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want, _ := time.Parse(time.RFC3339, future)
		if !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
		if got.Location() != time.UTC {
			t.Errorf("UTC で返すこと: got %v", got.Location())
		}
	})

	t.Run("相対は now からの加算になる", func(t *testing.T) {
		before := time.Now().UTC()
		got, err := parseExpiry("2h", "", "ttl", "expires_at")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		lo, hi := before.Add(2*time.Hour), time.Now().UTC().Add(2*time.Hour)
		if got.Before(lo) || got.After(hi) {
			t.Errorf("got %v, want between %v and %v", got, lo, hi)
		}
	})

	t.Run("d / w も相対として読める", func(t *testing.T) {
		got, err := parseExpiry("7d", "", "ttl", "expires_at")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d := time.Until(got); d < 6*24*time.Hour || d > 8*24*time.Hour {
			t.Errorf("7d が反映されていない: %v", d)
		}
	})

	t.Run("両方指定は排他エラー", func(t *testing.T) {
		_, err := parseExpiry("2h", future, "ttl", "expires_at")
		if err == nil {
			t.Fatal("error を期待したが nil")
		}
		if !strings.Contains(err.Error(), "mutually exclusive") {
			t.Errorf("排他のエラーであること: %v", err)
		}
	})

	t.Run("過去の絶対時刻は拒否", func(t *testing.T) {
		_, err := parseExpiry("", past, "ttl", "expires_at")
		if err == nil {
			t.Fatal("error を期待したが nil")
		}
		if !strings.Contains(err.Error(), "future") {
			t.Errorf("未来であることを要求するエラー: %v", err)
		}
	})

	t.Run("RFC3339 でない絶対時刻は拒否", func(t *testing.T) {
		for _, bad := range []string{"2026-10-01", "tomorrow", "1759312800", ""} {
			if bad == "" {
				continue // 空は「未指定」なのでここでは扱わない
			}
			if _, err := parseExpiry("", bad, "ttl", "expires_at"); err == nil {
				t.Errorf("%q は拒否されるべき", bad)
			}
		}
	})

	t.Run("読めない相対は拒否", func(t *testing.T) {
		if _, err := parseExpiry("いつか", "", "ttl", "expires_at"); err == nil {
			t.Error("error を期待したが nil")
		}
	})

	t.Run("0 以下の相対は拒否", func(t *testing.T) {
		for _, bad := range []string{"0s", "-1h"} {
			if _, err := parseExpiry(bad, "", "ttl", "expires_at"); err == nil {
				t.Errorf("%q は拒否されるべき", bad)
			}
		}
	})

	t.Run("フィールド名がエラーに出る(lease は for / until)", func(t *testing.T) {
		_, err := parseExpiry("7d", future, "for", "until")
		if err == nil {
			t.Fatal("error を期待したが nil")
		}
		if !strings.Contains(err.Error(), "for") || !strings.Contains(err.Error(), "until") {
			t.Errorf("呼び出し元のフィールド名を使うこと: %v", err)
		}
	})

	t.Run("タイムゾーン付きでも同じ瞬間なら同値", func(t *testing.T) {
		utc, err1 := parseExpiry("", "2026-10-01T10:00:00Z", "ttl", "expires_at")
		jst, err2 := parseExpiry("", "2026-10-01T19:00:00+09:00", "ttl", "expires_at")
		if err1 != nil || err2 != nil {
			t.Fatalf("unexpected error: %v / %v", err1, err2)
		}
		if !utc.Equal(jst) {
			t.Errorf("同じ瞬間を指すはず: %v != %v", utc, jst)
		}
	})
}
