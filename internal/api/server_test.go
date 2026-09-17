package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rikukadev/sashiki/internal/engine"
	"github.com/rikukadev/sashiki/internal/ops"
	"github.com/rikukadev/sashiki/internal/state"
	"github.com/rikukadev/sashiki/internal/storage"
	"github.com/rikukadev/sashiki/internal/workspace"
)

type fakeStorage struct{}

func (fakeStorage) Capabilities() storage.Capabilities {
	return storage.Capabilities{FastRollback: true}
}
func (fakeStorage) Clone(ctx context.Context, b storage.SnapshotRef, name string) (storage.Volume, error) {
	return storage.Volume{Name: name, Dataset: "p/b/" + name, Path: "/p/b/" + name}, nil
}
func (fakeStorage) SnapshotInit(ctx context.Context, v storage.Volume) (storage.SnapshotRef, error) {
	return storage.SnapshotRef(v.Dataset + "@init"), nil
}
func (fakeStorage) Rollback(ctx context.Context, v storage.Volume, s storage.SnapshotRef) error {
	return nil
}
func (fakeStorage) DeleteAsync(ctx context.Context, v storage.Volume) (storage.JobID, error) {
	return "done", nil
}
func (fakeStorage) Poll(ctx context.Context, j storage.JobID) (storage.JobStatus, error) {
	return storage.JobCompleted, nil
}
func (fakeStorage) SnapshotBase(ctx context.Context, tag string) (storage.SnapshotRef, error) {
	return "", nil
}
func (fakeStorage) ListSnapshots(ctx context.Context) ([]storage.SnapshotRef, error) {
	return []storage.SnapshotRef{"p/base@baseline", "p/base@baseline-20260901"}, nil
}
func (fakeStorage) UsedBytes(ctx context.Context, v storage.Volume) (int64, error) { return 42, nil }
func (fakeStorage) CurrentBaseline() storage.SnapshotRef                           { return "p/base@baseline" }

type fakeEngine struct{}

func (fakeEngine) Start(ctx context.Context, i engine.Instance) error     { return nil }
func (fakeEngine) Stop(ctx context.Context, i engine.Instance) error      { return nil }
func (fakeEngine) Kill(ctx context.Context, i engine.Instance) error      { return nil }
func (fakeEngine) WaitReady(ctx context.Context, i engine.Instance) error { return nil }
func (fakeEngine) IsRunning(ctx context.Context, i engine.Instance) (bool, error) {
	return true, nil
}
func (fakeEngine) ExposedListeners(context.Context, []engine.Instance) ([]string, error) {
	return []string{"pr-1 (10.0.0.10:3401)"}, nil
}

func newTestServer(t *testing.T, token string) *httptest.Server {
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
	s := New(mgr, "sashiki.internal", "mysql", "dev", "dev", token, nil)
	s.SetProxyListen("0.0.0.0:3306") // 実運用と同じく proxy 有効(#260)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv
}

func newTestServerWithDB(t *testing.T) (*httptest.Server, *state.DB) {
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
	s := New(mgr, "sashiki.internal", "mysql", "dev", "dev", "", nil)
	s.SetOps(ops.New(db))
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv, db
}

func TestAPILifecycle(t *testing.T) {
	srv := newTestServer(t, "")

	// create → 202(非同期化 #82。newTestServer は ops 未配線なので同期実行される)
	resp, err := http.Post(srv.URL+"/v1/branches", "application/json",
		strings.NewReader(`{"name":"pr-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create status = %d, want 202", resp.StatusCode)
	}
	_ = resp.Body.Close()
	// 作成結果は GET で確認する(応答は operation_id のみ)
	resp, _ = http.Get(srv.URL + "/v1/branches/pr-1")
	var b struct {
		Name  string `json:"name"`
		State string `json:"state"`
		User  string `json:"user"`
		Host  string `json:"host"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&b)
	_ = resp.Body.Close()
	if b.State != "running" || b.User != "dev@pr-1" || b.Host != "sashiki.internal" {
		t.Errorf("branch = %+v", b)
	}

	// duplicate → 409
	resp, _ = http.Post(srv.URL+"/v1/branches", "application/json", strings.NewReader(`{"name":"pr-1"}`))
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("dup status = %d, want 409", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// exist_ok=true → 200
	resp, _ = http.Post(srv.URL+"/v1/branches?exist_ok=true", "application/json", strings.NewReader(`{"name":"pr-1"}`))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("exist_ok status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// invalid name → 400
	resp, _ = http.Post(srv.URL+"/v1/branches", "application/json", strings.NewReader(`{"name":"BAD NAME"}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// reset → 202
	resp, _ = http.Post(srv.URL+"/v1/branches/pr-1/reset", "application/json", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("reset status = %d, want 202", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// delete → 202
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/branches/pr-1", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("delete status = %d, want 202", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// not found → 404
	resp, _ = http.Get(srv.URL + "/v1/branches/pr-1")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("get status = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// #180: 存在しないブランチの delete は 404(「deleted」と誤表示しない)。
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/v1/branches/does-not-exist", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("delete of missing branch = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestAPIAuthFromNonLoopback(t *testing.T) {
	// httptest は loopback なので、authorized() を直接検証する。
	db, _ := state.Open(filepath.Join(t.TempDir(), "s.db"))
	defer func() { _ = db.Close() }()
	fs := fakeStorage{}
	mgr, _ := workspace.New(workspace.Config{NamePattern: `^.+$`, PortLow: 1, PortHigh: 2, EngineType: "mysql"}, fs, fs, fakeEngine{}, nil, db)
	s := New(mgr, "d", "mysql", "dev", "dev", "secret", db)

	req := httptest.NewRequest(http.MethodGet, "/v1/branches", nil)
	req.RemoteAddr = "10.0.0.5:12345"
	if s.authorized(req) {
		t.Error("no token from non-loopback should be denied")
	}
	req.Header.Set("Authorization", "Bearer wrong")
	if s.authorized(req) {
		t.Error("wrong token should be denied")
	}
	req.Header.Set("Authorization", "Bearer secret")
	if !s.authorized(req) {
		t.Error("correct token should be allowed")
	}
	// loopback は無認証(既定 trust_loopback=true)
	req2 := httptest.NewRequest(http.MethodGet, "/v1/branches", nil)
	req2.RemoteAddr = "127.0.0.1:9999"
	if !s.authorized(req2) {
		t.Error("loopback should be allowed without token")
	}

	// #198: trust_loopback=false なら loopback でもトークン必須
	s.SetTrustLoopback(false)
	req3 := httptest.NewRequest(http.MethodGet, "/v1/branches", nil)
	req3.RemoteAddr = "127.0.0.1:9999"
	if s.authorized(req3) {
		t.Error("trust_loopback=false: loopback without token should be denied")
	}
	req3.Header.Set("Authorization", "Bearer secret")
	if !s.authorized(req3) {
		t.Error("trust_loopback=false: loopback with correct token should be allowed")
	}
}

func TestAPIBaseline(t *testing.T) {
	srv := newTestServer(t, "")
	resp, err := http.Get(srv.URL + "/v1/baseline")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var b struct {
		Current   string   `json:"current"`
		Snapshots []string `json:"snapshots"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&b)
	if b.Current != "p/base@baseline" {
		t.Errorf("current = %q", b.Current)
	}
	if len(b.Snapshots) != 2 {
		t.Errorf("snapshots = %v", b.Snapshots)
	}
}

func TestAPIDoctorIncludesExposedListeners(t *testing.T) {
	srv := newTestServer(t, "")
	resp, err := http.Get(srv.URL + "/v1/doctor")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Exposed []string `json:"exposed_listeners"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Exposed) != 1 || !strings.Contains(body.Exposed[0], "10.0.0.10:3401") {
		t.Fatalf("exposed_listeners = %v", body.Exposed)
	}
}

func TestAPIAuthWithDBToken(t *testing.T) {
	db, _ := state.Open(filepath.Join(t.TempDir(), "s.db"))
	defer func() { _ = db.Close() }()
	fs := fakeStorage{}
	mgr, _ := workspace.New(workspace.Config{NamePattern: `^.+$`, PortLow: 1, PortHigh: 2, EngineType: "mysql"}, fs, fs, fakeEngine{}, nil, db)
	s := New(mgr, "d", "mysql", "dev", "dev", "", db)

	// トークン登録(平文 "sashiki_abc" のハッシュ)
	sum := sha256.Sum256([]byte("sashiki_abc"))
	if err := db.CreateToken("t1", hex.EncodeToString(sum[:]), ""); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/branches", nil)
	req.RemoteAddr = "10.0.0.5:1"
	req.Header.Set("Authorization", "Bearer sashiki_abc")
	if !s.authorized(req) {
		t.Error("db token should be accepted")
	}
	req.Header.Set("Authorization", "Bearer sashiki_wrong")
	if s.authorized(req) {
		t.Error("wrong token should be denied")
	}
	// revoke 後は拒否
	_ = db.RevokeToken("t1")
	req.Header.Set("Authorization", "Bearer sashiki_abc")
	if s.authorized(req) {
		t.Error("revoked token should be denied")
	}
}

func TestMetricsAndWebUI(t *testing.T) {
	srv := newTestServer(t, "")
	// ブランチを1つ作ってから
	resp, _ := http.Post(srv.URL+"/v1/branches", "application/json", strings.NewReader(`{"name":"pr-m"}`))
	_ = resp.Body.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Errorf("webui status=%d type=%s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestMetricsHandler(t *testing.T) {
	db, _ := state.Open(filepath.Join(t.TempDir(), "s.db"))
	defer func() { _ = db.Close() }()
	fs := fakeStorage{}
	mgr, _ := workspace.New(workspace.Config{NamePattern: `^.+$`, PortLow: 3401, PortHigh: 3410, EngineType: "mysql", StateDir: t.TempDir()}, fs, fs, fakeEngine{}, nil, db)
	_, _ = mgr.Create(context.Background(), "pr-1", 0)

	ms := httptest.NewServer(MetricsHandler(mgr))
	defer ms.Close()
	resp, err := http.Get(ms.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := new(strings.Builder)
	_, _ = io.Copy(body, resp.Body)
	out := body.String()
	if !strings.Contains(out, `sashiki_branches{state="running"} 1`) {
		t.Errorf("metrics missing running gauge:\n%s", out)
	}
	if !strings.Contains(out, `sashiki_branch_used_bytes{branch="pr-1"} 42`) {
		t.Errorf("metrics missing used bytes:\n%s", out)
	}
	// capacity 系(仕様 20-5)。mock は PoolCapacity 未設定(total=0)なので
	// pool 系は出ず、watermark / memory / ports は常に出る
	for _, want := range []string{
		`sashiki_storage_watermark{level="critical"}`,
		"sashiki_memory_available_bytes",
		"sashiki_max_running",
		"sashiki_ports_total 10",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "sashiki_hook_failures_total 0") {
		t.Errorf("metrics missing hook failures:\n%s", out)
	}
}

func TestMetricsOperationStats(t *testing.T) {
	db, _ := state.Open(filepath.Join(t.TempDir(), "s.db"))
	defer func() { _ = db.Close() }()
	fs := fakeStorage{}
	mgr, _ := workspace.New(workspace.Config{NamePattern: `^.+$`, PortLow: 3401, PortHigh: 3410, EngineType: "mysql", StateDir: t.TempDir()}, fs, fs, fakeEngine{}, nil, db)
	_ = db.CreateOperation("op_1", "create", "pr-1")
	_ = db.FinishOperation("op_1", "", "")
	_ = db.CreateOperation("op_2", "reset", "pr-1")
	_ = db.FinishOperation("op_2", "", "boom")

	ms := httptest.NewServer(MetricsHandler(mgr))
	defer ms.Close()
	resp, err := http.Get(ms.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := new(strings.Builder)
	_, _ = io.Copy(body, resp.Body)
	out := body.String()
	for _, want := range []string{
		`sashiki_operations_total{type="create",state="completed"} 1`,
		`sashiki_operations_total{type="reset",state="failed"} 1`,
		`sashiki_operation_duration_seconds_count{type="create",state="completed"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
}

// waitOp は operation が完了(running 以外)するまでポーリングして最終 state を返す。
func waitOp(t *testing.T, srv *httptest.Server, opID string) string {
	t.Helper()
	for i := 0; i < 200; i++ {
		resp, err := http.Get(srv.URL + "/v1/operations/" + opID)
		if err != nil {
			t.Fatal(err)
		}
		var o struct {
			State string `json:"state"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&o)
		_ = resp.Body.Close()
		if o.State != "running" && o.State != "" {
			return o.State
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("operation %s did not finish", opID)
	return ""
}

func TestAPIOperationsTracking(t *testing.T) {
	srv, db := newTestServerWithDB(t)
	// create → 202、ヘッダに operation id。完了を待ってから operation を検証する。
	resp, err := http.Post(srv.URL+"/v1/branches", "application/json", strings.NewReader(`{"name":"pr-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create status = %d, want 202", resp.StatusCode)
	}
	opID := resp.Header.Get("Sashiki-Operation-Id")
	_ = resp.Body.Close()
	if opID == "" {
		t.Fatal("create should return operation id header")
	}
	if st := waitOp(t, srv, opID); st != "completed" {
		t.Fatalf("op state = %s, want completed", st)
	}
	// GET /v1/operations/{id}
	resp, _ = http.Get(srv.URL + "/v1/operations/" + opID)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get op status = %d", resp.StatusCode)
	}
	var o struct {
		Type, State, Target string
	}
	_ = json.NewDecoder(resp.Body).Decode(&o)
	if o.Type != "create" || o.State != "completed" || o.Target != "pr-1" {
		t.Errorf("op = %+v", o)
	}
	// GET /v1/operations 一覧
	r2, _ := http.Get(srv.URL + "/v1/operations")
	defer func() { _ = r2.Body.Close() }()
	var list struct {
		Operations []map[string]any `json:"operations"`
	}
	_ = json.NewDecoder(r2.Body).Decode(&list)
	if len(list.Operations) == 0 {
		t.Error("operations list should not be empty")
	}
	_ = db
}

func TestAPICreateWithProvenance(t *testing.T) {
	srv, _ := newTestServerWithDB(t)
	body := `{"name":"pr-1","owner":"bob","purpose":"qa","source":{"type":"github_pr","ref":"7"}}`
	resp, err := http.Post(srv.URL+"/v1/branches", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create status = %d, want 202", resp.StatusCode)
	}
	opID := resp.Header.Get("Sashiki-Operation-Id")
	_ = resp.Body.Close()
	if st := waitOp(t, srv, opID); st != "completed" {
		t.Fatalf("create op state = %s, want completed", st)
	}
	// provenance は作成後に GET で確認する(応答は operation_id のみ)
	resp, _ = http.Get(srv.URL + "/v1/branches/pr-1")
	defer func() { _ = resp.Body.Close() }()
	var b struct {
		Owner   string          `json:"owner"`
		Purpose string          `json:"purpose"`
		Source  json.RawMessage `json:"source"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&b)
	if b.Owner != "bob" || b.Purpose != "qa" {
		t.Errorf("provenance = %+v", b)
	}
	if len(b.Source) == 0 || !strings.Contains(string(b.Source), "github_pr") {
		t.Errorf("source round-trip failed: %s", b.Source)
	}
}

// promote 元を壊す操作は非同期 operation に積む前に 412 を返す(#289)。
// operation 内で拒否するだけだと HTTP は 202 になり、CLI/Action が成功と誤認する。
func TestAPIMutationsReturn412BeforeAsyncOperation(t *testing.T) {
	srv, db := newTestServerWithDB(t)
	resp, err := http.Post(srv.URL+"/v1/branches", "application/json", strings.NewReader(`{"name":"pr-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	opID := resp.Header.Get("Sashiki-Operation-Id")
	_ = resp.Body.Close()
	if st := waitOp(t, srv, opID); st != "completed" {
		t.Fatalf("create op state = %s, want completed", st)
	}
	// fakeStorage は ResolveVolume を持たず dataset="" なので、その dataset 上に
	// baseline を登録して promote 後と同じ保護状態にする。
	if err := db.RegisterBaseline("@baseline-promoted", state.BaselineProvenance{}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{name: "reset", method: http.MethodPost, path: "/v1/branches/pr-1/reset"},
		{name: "recreate", method: http.MethodPost, path: "/v1/branches/pr-1/recreate"},
		{name: "delete", method: http.MethodDelete, path: "/v1/branches/pr-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, srv.URL+tc.path, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusPreconditionFailed {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 412: %s", resp.StatusCode, body)
			}
			if got := resp.Header.Get("Sashiki-Operation-Id"); got != "" {
				t.Errorf("refused request must not create an operation, got %q", got)
			}
		})
	}
}

func TestWriteErrorClassification(t *testing.T) {
	db, _ := state.Open(filepath.Join(t.TempDir(), "s.db"))
	defer func() { _ = db.Close() }()
	fs := fakeStorage{}
	mgr, _ := workspace.New(workspace.Config{NamePattern: `^.+$`, PortLow: 1, PortHigh: 2, EngineType: "mysql"}, fs, fs, fakeEngine{}, nil, db)
	s := New(mgr, "d", "mysql", "dev", "dev", "", nil)

	cases := []struct {
		err  error
		code int
		id   string
	}{
		{workspace.StageError("hook", errors.New("on-create exited 1")), 500, "hook_failed"},
		{workspace.StageError("engine-start", errors.New("start failed")), 500, "engine_error"},
		{workspace.StageError("engine-ready", errors.New("timeout")), 500, "engine_error"},
		{workspace.StageError("clone", errors.New("zfs clone failed")), 500, "storage_error"},
		{fmt.Errorf("not masked: %w", workspace.ErrPreconditionFailed), 412, "precondition_failed"},
		{fmt.Errorf("x: %w", workspace.ErrBaselineNotFound), 404, "baseline_not_found"},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		s.writeError(w, c.err)
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.id) {
			t.Errorf("err=%v → status=%d body=%s (want %d %s)", c.err, w.Code, w.Body.String(), c.code, c.id)
		}
	}
}

// 接続情報(host/port/user)は「そのまま繋がる 3 つ組」でなければならない。
// proxy 有効時に port だけブランチ内部のものを返していて、どう解釈しても
// 接続できない値になっていた(#260)。
func TestConnectionInfoTargetsProxy(t *testing.T) {
	db, _ := state.Open(filepath.Join(t.TempDir(), "s.db"))
	defer func() { _ = db.Close() }()
	fs := fakeStorage{}
	mgr, _ := workspace.New(workspace.Config{NamePattern: `^.+$`, PortLow: 3401, PortHigh: 3410, EngineType: "mysql", StateDir: t.TempDir()}, fs, fs, fakeEngine{}, nil, db)
	info, err := mgr.Create(context.Background(), "pr-1", 0)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("proxy 有効なら固定エンドポイント宛", func(t *testing.T) {
		s := New(mgr, "sashiki.internal", "mysql", "dev", "dev", "", nil)
		s.SetProxyListen("0.0.0.0:3306")
		b := s.toJSON(info)
		if b.Port != 3306 {
			t.Errorf("port=%d, proxy のポート(3306)を返すべき", b.Port)
		}
		if b.User != "dev@pr-1" {
			t.Errorf("user=%q, proxy は user でルーティングする", b.User)
		}
		if b.EnginePort != info.Port {
			t.Errorf("engine_port=%d, ブランチ自身の listener(%d)を返すべき", b.EnginePort, info.Port)
		}
	})

	// proxy を使わない構成では user@branch でルーティングする相手がいない。
	// 内部ポートへ直結し、user は素の dev(直結先の mysqld に dev@pr-1 は無い)。
	t.Run("proxy 無効ならブランチ直結", func(t *testing.T) {
		s := New(mgr, "sashiki.internal", "mysql", "dev", "dev", "", nil)
		b := s.toJSON(info)
		if b.Port != info.Port {
			t.Errorf("port=%d, ブランチの内部ポート(%d)を返すべき", b.Port, info.Port)
		}
		if b.User != "dev" {
			t.Errorf("user=%q, 直結では @branch を付けてはいけない", b.User)
		}
	})
}

// listen.proxy の表記から接続用ポートを取れること。解釈できない値で
// 落ちるのではなく 0(proxy 無効)に倒すのは、API が止まるより
// 「直結の接続情報が返る」方が復旧しやすいため。
func TestProxyPortParsing(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"0.0.0.0:3306", 3306},
		{"127.0.0.1:13306", 13306},
		{":5432", 5432},
		{"", 0},
		{"3306", 0},          // ホストが無い
		{"0.0.0.0:mysql", 0}, // サービス名は解決しない
	} {
		s := New(nil, "d", "mysql", "dev", "dev", "", nil)
		s.SetProxyListen(tc.in)
		if s.proxyPort != tc.want {
			t.Errorf("SetProxyListen(%q) -> proxyPort=%d, want %d", tc.in, s.proxyPort, tc.want)
		}
	}
}
