package api

import (
	"encoding/json"
	"testing"
)

// withHostName は config の name を応答のトップレベルに host_name として足す(#383)。
// 空なら足さない — 1 台構成の既存利用者には応答が変わらない。
func TestWithHostName(t *testing.T) {
	t.Run("設定されていれば host_name が付く", func(t *testing.T) {
		s := &Server{}
		s.SetHostName("mysql-main")
		got := s.withHostName(map[string]any{"status": "ok"})
		if got["host_name"] != "mysql-main" {
			t.Errorf("host_name = %v, want mysql-main", got["host_name"])
		}
		if got["status"] != "ok" {
			t.Errorf("既存のキーを壊さないこと: %v", got)
		}
	})

	t.Run("空なら応答に現れない", func(t *testing.T) {
		s := &Server{}
		got := s.withHostName(map[string]any{"status": "ok"})
		if _, ok := got["host_name"]; ok {
			t.Errorf("未設定なら host_name を足さないこと: %v", got)
		}
	})

	t.Run("JSON にしたときキー名が host_name である(name だとブランチ名と紛れる)", func(t *testing.T) {
		s := &Server{}
		s.SetHostName("pg-main")
		b, err := json.Marshal(s.withHostName(map[string]any{"branches": []string{}}))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if m["host_name"] != "pg-main" {
			t.Errorf("host_name = %v", m["host_name"])
		}
		if _, ok := m["name"]; ok {
			t.Error("トップレベルに name を置かないこと(branches[].name と紛れる)")
		}
	})
}
