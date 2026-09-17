package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rikukadev/sashiki/internal/state"
	"github.com/rikukadev/sashiki/internal/workspace"
)

// branches スコープのトークンは admin 専用の経路(baseline 変更 / drain / gc /
// データブラウザ / hook 手動実行)に届かない(#294)。
func TestTokenScopeGatesAdminRoutes(t *testing.T) {
	db, _ := state.Open(filepath.Join(t.TempDir(), "s.db"))
	defer func() { _ = db.Close() }()
	fs := fakeStorage{}
	mgr, _ := workspace.New(workspace.Config{NamePattern: `^.+$`, PortLow: 1, PortHigh: 2, EngineType: "mysql"}, fs, fs, fakeEngine{}, nil, db)
	s := New(mgr, "d", "mysql", "dev", "dev", "", db)

	mk := func(name, plain, scope string) {
		sum := sha256.Sum256([]byte(plain))
		if err := db.CreateToken(name, hex.EncodeToString(sum[:]), scope); err != nil {
			t.Fatal(err)
		}
	}
	mk("ci", "sashiki_ci", state.ScopeBranches)
	mk("ops", "sashiki_ops", state.ScopeAdmin)

	do := func(token, method, path string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(`{"branch":"x","sql":"select 1"}`))
		req.RemoteAddr = "10.0.0.5:1"
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec.Code
	}

	// 読み取りとブランチ操作は両方通る(404 等は「認可は通った」の意味)。
	for _, path := range []string{"/v1/branches", "/v1/baseline", "/v1/capacity"} {
		if c := do("sashiki_ci", http.MethodGet, path); c == http.StatusForbidden || c == http.StatusUnauthorized {
			t.Errorf("branches scope should read %s, got %d", path, c)
		}
	}
	if c := do("sashiki_ci", http.MethodPost, "/v1/branches/nope/reset"); c == http.StatusForbidden {
		t.Error("branches scope should be allowed to reset a branch")
	}

	// admin 専用は 403 insufficient_scope。
	for _, tc := range []struct{ m, p string }{
		{http.MethodPost, "/v1/baseline/promote"},
		{http.MethodPost, "/v1/baseline/set"},
		{http.MethodPost, "/v1/baseline/gc"},
		{http.MethodPost, "/v1/drain"},
		{http.MethodPost, "/v1/gc/orphans"},
		{http.MethodPost, "/v1/branches/x/query"},
		{http.MethodGet, "/v1/branches/x/schema"},
		{http.MethodPost, "/v1/branches/x/hooks/on-create"},
	} {
		if c := do("sashiki_ci", tc.m, tc.p); c != http.StatusForbidden {
			t.Errorf("branches scope: %s %s = %d, want 403", tc.m, tc.p, c)
		}
		if c := do("sashiki_ops", tc.m, tc.p); c == http.StatusForbidden || c == http.StatusUnauthorized {
			t.Errorf("admin scope: %s %s = %d, must not be refused by scope", tc.m, tc.p, c)
		}
	}

	// loopback は admin。
	req := httptest.NewRequest(http.MethodPost, "/v1/drain", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
		t.Errorf("loopback should be admin, got %d", rec.Code)
	}
}

// scope 列が無い旧トークン(NULL)は admin として扱う(後方互換)。
func TestLegacyTokenWithoutScopeIsAdmin(t *testing.T) {
	db, _ := state.Open(filepath.Join(t.TempDir(), "s.db"))
	defer func() { _ = db.Close() }()
	sum := sha256.Sum256([]byte("sashiki_old"))
	if err := db.CreateToken("old", hex.EncodeToString(sum[:]), ""); err != nil {
		t.Fatal(err)
	}
	name, scope, ok, err := db.LookupTokenHash(hex.EncodeToString(sum[:]))
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if name != "old" || scope != state.ScopeAdmin {
		t.Errorf("legacy token = %s/%s, want old/admin", name, scope)
	}
	toks, _ := db.ListTokens()
	if len(toks) != 1 || toks[0].Scope != state.ScopeAdmin {
		t.Errorf("ListTokens should report admin for legacy rows: %+v", toks)
	}
}
