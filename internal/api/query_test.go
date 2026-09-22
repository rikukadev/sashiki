package api

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rikukadev/sashiki/internal/state"
	"github.com/rikukadev/sashiki/internal/workspace"
)

// --- database/sql/driver のフェイク(外部依存なしで *sql.Rows を作る) ---

type fakeRows struct {
	cols []string
	data [][]driver.Value
	i    int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.i])
	r.i++
	return nil
}

type fakeStmt struct{ rows func() *fakeRows }

func (s fakeStmt) Close() error  { return nil }
func (s fakeStmt) NumInput() int { return 0 }
func (s fakeStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("exec not supported")
}
func (s fakeStmt) Query([]driver.Value) (driver.Rows, error) { return s.rows(), nil }

type fakeConn struct{ rows func() *fakeRows }

func (c fakeConn) Prepare(string) (driver.Stmt, error) { return fakeStmt(c), nil }
func (c fakeConn) Close() error                        { return nil }
func (c fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("tx not supported") }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

type fakeConnector struct{ rows func() *fakeRows }

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return fakeConn(c), nil
}
func (c fakeConnector) Driver() driver.Driver { return fakeDriver{} }

// newQueryTestServer は openDB をフェイク DB に差し替えた Server を作る。
func newQueryTestServer(t *testing.T, engineType string, rows func() *fakeRows) *Server {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fs := fakeStorage{}
	mgr, err := workspace.New(workspace.Config{
		NamePattern: `^[a-z0-9-]{1,32}$`, MaxBranches: 10,
		PortLow: 3401, PortHigh: 3410, EngineType: "mysql", StateDir: t.TempDir(),
	}, fs, fs, fakeEngine{}, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	s := New(mgr, "sashiki.internal", engineType, "dev", "dev", "", nil)
	if rows != nil {
		s.openDB = func(ctx context.Context, name string) (*sql.DB, error) {
			return sql.OpenDB(fakeConnector{rows: rows}), nil
		}
	}
	return s
}

func postQuery(s *Server, body, contentType, origin, host string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "http://"+host+"/v1/branches/pr-q/query", bytes.NewBufferString(body))
	req.RemoteAddr = "127.0.0.1:9999"
	req.Header.Set("Content-Type", contentType)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

func TestQueryRowLimitAndConversion(t *testing.T) {
	big := bytes.Repeat([]byte("x"), maxCellBytes+100)
	rows := func() *fakeRows {
		r := &fakeRows{cols: []string{"id", "blob", "nul"}}
		r.data = append(r.data, []driver.Value{int64(0), append([]byte(nil), big...), nil})
		for i := 1; i < 250; i++ {
			r.data = append(r.data, []driver.Value{int64(i), []byte("v"), nil})
		}
		return r
	}
	s := newQueryTestServer(t, "mysql", rows)

	w := postQuery(s, `{"sql":"SELECT * FROM t"}`, "application/json", "", "localhost:8080")
	if w.Code != 200 {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Columns   []string `json:"columns"`
		Rows      [][]any  `json:"rows"`
		Truncated bool     `json:"truncated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Rows) != maxRows || !resp.Truncated {
		t.Errorf("rows=%d truncated=%v, want %d/true", len(resp.Rows), resp.Truncated, maxRows)
	}
	// []byte → string 変換と NULL
	if resp.Rows[1][1] != "v" || resp.Rows[1][2] != nil {
		t.Errorf("row conversion: %+v", resp.Rows[1])
	}
	// 巨大セルの切り詰め
	cell, _ := resp.Rows[0][1].(string)
	if len(cell) > maxCellBytes+64 || !strings.HasSuffix(cell, "…(truncated)") {
		t.Errorf("big cell should be truncated (len=%d suffix=%q)", len(cell), cell[max(0, len(cell)-20):])
	}
}

func TestSchemaGrouping(t *testing.T) {
	rows := func() *fakeRows {
		return &fakeRows{
			cols: []string{"s", "t", "r", "c", "ct", "n", "k"},
			data: [][]driver.Value{
				{[]byte("app"), []byte("items"), int64(3), []byte("id"), []byte("bigint"), []byte("NO"), []byte("PRI")},
				{[]byte("app"), []byte("items"), int64(3), []byte("name"), []byte("text"), []byte("YES"), []byte("")},
				{[]byte("app"), []byte("users"), int64(1), []byte("id"), []byte("bigint"), []byte("NO"), []byte("PRI")},
				{[]byte("logs"), []byte("events"), int64(9), []byte("id"), []byte("bigint"), []byte("NO"), []byte("PRI")},
			},
		}
	}
	s := newQueryTestServer(t, "mysql", rows)

	req := httptest.NewRequest("GET", "http://localhost:8080/v1/branches/pr-q/schema", nil)
	req.RemoteAddr = "127.0.0.1:9999"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Databases []schemaDB `json:"databases"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Databases) != 2 {
		t.Fatalf("databases = %d, want 2", len(resp.Databases))
	}
	app := resp.Databases[0]
	if app.Name != "app" || len(app.Tables) != 2 || len(app.Tables[0].Columns) != 2 {
		t.Errorf("grouping broken: %+v", app)
	}
	if !app.Tables[0].Columns[1].Nullable || app.Tables[0].Columns[0].Key != "PRI" {
		t.Errorf("column attrs broken: %+v", app.Tables[0].Columns)
	}
}

func TestQueryRejectsWrongContentType(t *testing.T) {
	s := newQueryTestServer(t, "mysql", nil)
	w := postQuery(s, `{"sql":"SELECT 1"}`, "text/plain", "", "localhost:8080")
	if w.Code != 415 {
		t.Errorf("text/plain should be 415, got %d", w.Code)
	}
}

func TestQueryRejectsCrossOrigin(t *testing.T) {
	s := newQueryTestServer(t, "mysql", nil)
	w := postQuery(s, `{"sql":"SELECT 1"}`, "application/json", "https://evil.example", "localhost:8080")
	if w.Code != 403 {
		t.Errorf("cross-site Origin should be 403, got %d", w.Code)
	}
	// localhost Origin は許可(UI 自身の fetch)
	w = postQuery(s, `{"sql":"SELECT 1"}`, "application/json", "http://localhost:8080", "localhost:8080")
	if w.Code == 403 {
		t.Errorf("localhost Origin should not be rejected, got %d", w.Code)
	}
}

func TestQueryRejectsDNSRebindingHost(t *testing.T) {
	s := newQueryTestServer(t, "mysql", nil)
	// loopback 接続なのに Host が外部名 → DNS リバインディングの疑い
	w := postQuery(s, `{"sql":"SELECT 1"}`, "application/json", "", "evil.example")
	if w.Code != 403 {
		t.Errorf("rebound Host should be 403, got %d", w.Code)
	}
}

// postgres は #228 で対応済み。未知の engine だけ 501 を返すこと。
func TestQueryUnsupportedEngine(t *testing.T) {
	s := newQueryTestServer(t, "sqlite", nil) // openDB は本物の branchDB のまま
	w := postQuery(s, `{"sql":"SELECT 1"}`, "application/json", "", "localhost:8080")
	if w.Code != 501 {
		t.Errorf("unknown engine should be 501, got %d body=%s", w.Code, w.Body.String())
	}
}

// postgres は 501 にならないこと(未対応扱いに戻っていないかの回帰検出)。
// ブランチが無いので 404 になるが、501 でなければ「対応している」と言える。
func TestQueryPostgresIsSupported(t *testing.T) {
	s := newQueryTestServer(t, "postgres", nil)
	w := postQuery(s, `{"sql":"SELECT 1"}`, "application/json", "", "localhost:8080")
	if w.Code == 501 {
		t.Errorf("postgres should be supported now (#228), got 501 body=%s", w.Body.String())
	}
}

// engine ごとにスキーマ問い合わせが切り替わること。返す列の並びは共通で、
// postgres 側は MySQL 固有の column_type / column_key / table_rows を使わない。
func TestSchemaQueryPerEngine(t *testing.T) {
	pg := &Server{engine: "postgres"}
	my := &Server{engine: "mysql"}
	if pg.schemaQuery() == my.schemaQuery() {
		t.Fatal("engine ごとに別のクエリを使うべき")
	}
	q := pg.schemaQuery()
	// MySQL 固有の information_schema 列を参照していないこと
	// (出力の別名としての column_key は残してよい。列の並びを揃えるため)。
	for _, bad := range []string{"c.column_type", "c.column_key", "t.table_rows"} {
		if strings.Contains(q, bad) {
			t.Errorf("postgres のクエリが MySQL 固有の %s を参照している", bad)
		}
	}
	for _, want := range []string{"data_type", "reltuples", "PRIMARY KEY"} {
		if !strings.Contains(q, want) {
			t.Errorf("postgres のクエリに %s が無い", want)
		}
	}
}

func TestPgDSNEscapesCredentials(t *testing.T) {
	dsn := pgDSN("dev@x", "p@ss:w/rd", 5433, "app")
	if !strings.HasPrefix(dsn, "postgres://") {
		t.Fatalf("dsn = %q", dsn)
	}
	// 記号がそのまま出て URL を壊していないこと
	if strings.Contains(dsn, "p@ss:w/rd") {
		t.Errorf("パスワードがエスケープされていない: %q", dsn)
	}
	if !strings.Contains(dsn, "/app") || !strings.Contains(dsn, "sslmode=disable") {
		t.Errorf("dsn = %q", dsn)
	}
}

func TestQueryInvalidBody(t *testing.T) {
	s := newQueryTestServer(t, "mysql", nil)
	w := postQuery(s, `{}`, "application/json", "", "localhost:8080")
	if w.Code != 400 {
		t.Errorf("empty sql should be 400, got %d", w.Code)
	}
}

func TestQuerySchemaRequireAuthFromNonLoopback(t *testing.T) {
	s := newQueryTestServer(t, "mysql", nil)
	s.token = "secret"
	for _, path := range []string{"/v1/branches/pr-q/schema", "/v1/branches/pr-q/query"} {
		req := httptest.NewRequest("GET", "http://sashiki.internal"+path, nil)
		req.RemoteAddr = "10.0.0.5:1234"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code != 401 {
			t.Errorf("%s without token should be 401, got %d", path, w.Code)
		}
	}
}

// トークン認証なら同一オリジンの UI からデータブラウザを使える。loopback 無認証の
// 同一オリジン(DNS リバインディング)は従来どおり拒否(#301)。
func TestBrowserSafeAllowsSameOriginWithToken(t *testing.T) {
	s := &Server{}
	mk := func(caller string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://sashiki.internal:8080/v1/branches/x/schema", nil)
		r.Host = "sashiki.internal:8080"
		r.Header.Set("Origin", "http://sashiki.internal:8080")
		r.RemoteAddr = "10.0.0.5:1"
		if caller != "" {
			r = r.WithContext(context.WithValue(r.Context(), principalKey{}, principal{Name: caller, Scope: state.ScopeAdmin}))
		}
		return r
	}
	if !s.browserSafe(httptest.NewRecorder(), mk("ops")) {
		t.Error("token-authenticated same-origin request should be allowed")
	}
	if s.browserSafe(httptest.NewRecorder(), mk("loopback")) {
		t.Error("loopback-exempt same-origin (non-localhost) must still be rejected")
	}
	r := mk("ops")
	r.Header.Set("Origin", "http://evil.example")
	if s.browserSafe(httptest.NewRecorder(), r) {
		t.Error("cross-origin must be rejected even with a token")
	}
}
