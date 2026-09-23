package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/rikukadev/sashiki/internal/state"
	"github.com/rikukadev/sashiki/internal/workspace"
)

// retry も存在しないブランチには同期で 404 を返す(#323)。
func TestRetryReturns404Synchronously(t *testing.T) {
	db, _ := state.Open(filepath.Join(t.TempDir(), "s.db"))
	defer func() { _ = db.Close() }()
	fs := fakeStorage{}
	mgr, _ := workspace.New(workspace.Config{NamePattern: `^.+$`, PortLow: 1, PortHigh: 2, EngineType: "mysql"}, fs, fs, fakeEngine{}, nil, db)
	s := New(mgr, "d", "mysql", "dev", "dev", "", db)
	req := httptest.NewRequest(http.MethodPost, "/v1/branches/nope/retry", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("retry of a missing branch = %d, want 404 (synchronous)", rec.Code)
	}
}

// Bearer 認証なら、同一ホストのリバースプロキシ越し(接続元 loopback、Host が公開名)
// でもデータブラウザを使える(#326)。loopback 無認証は従来どおり Host を検査する。
func TestBrowserSafeHostCheckSkipsTokenAuth(t *testing.T) {
	s := &Server{}
	mk := func(caller string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://sashiki.example.com/v1/branches/x/schema", nil)
		r.Host = "sashiki.example.com"
		r.RemoteAddr = "127.0.0.1:40000"
		if caller != "" {
			r = r.WithContext(context.WithValue(r.Context(), principalKey{}, principal{Name: caller, Scope: state.ScopeAdmin}))
		}
		return r
	}
	if !s.browserSafe(httptest.NewRecorder(), mk("ops")) {
		t.Error("token-authenticated request behind a reverse proxy should pass the Host check")
	}
	if s.browserSafe(httptest.NewRecorder(), mk("loopback")) {
		t.Error("loopback-exempt request with a non-localhost Host must still be rejected (DNS rebinding)")
	}
}
