package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rikukadev/sashiki/internal/engine"
	"github.com/rikukadev/sashiki/internal/hooks"
	"github.com/rikukadev/sashiki/internal/state"
	"github.com/rikukadev/sashiki/internal/storage"
)

// --- mocks ---

type mockStorage struct {
	caps             storage.Capabilities
	renamed          []string
	baselinesDeleted []string
	poolUsed         int64
	poolTotal        int64
	volumes          []string
	cloned           []string
	snapshots        []string
	rollbacks        []string
	destroyed        []string
	cloneErr         error
	rollbackErr      error
	promoteErr       error
	quota            map[string]int64 // #85: SetQuota で記録
	missing          map[string]bool  // #319: ResolveVolume を失敗させる branch
}

func (m *mockStorage) SetQuota(ctx context.Context, vol storage.Volume, bytes int64) error {
	if m.quota == nil {
		m.quota = map[string]int64{}
	}
	m.quota[vol.Dataset] = bytes
	return nil
}

func (m *mockStorage) Capabilities() storage.Capabilities { return m.caps }

func (m *mockStorage) Clone(ctx context.Context, baseline storage.SnapshotRef, name string) (storage.Volume, error) {
	if m.cloneErr != nil {
		return storage.Volume{}, m.cloneErr
	}
	m.cloned = append(m.cloned, name)
	return storage.Volume{Name: name, Dataset: "pool/branches/" + name, Path: "/pool/branches/" + name}, nil
}

func (m *mockStorage) ResolveVolume(ctx context.Context, name string) (storage.Volume, error) {
	if m.missing[name] {
		return storage.Volume{}, errors.New("dataset does not exist")
	}
	return storage.Volume{Name: name, Dataset: "pool/branches/" + name, Path: "/pool/branches/" + name}, nil
}

func (m *mockStorage) SnapshotInit(ctx context.Context, vol storage.Volume) (storage.SnapshotRef, error) {
	snap := vol.Dataset + "@init"
	m.snapshots = append(m.snapshots, snap)
	return storage.SnapshotRef(snap), nil
}

func (m *mockStorage) Rollback(ctx context.Context, vol storage.Volume, snap storage.SnapshotRef) error {
	if m.rollbackErr != nil {
		return m.rollbackErr
	}
	m.rollbacks = append(m.rollbacks, string(snap))
	return nil
}

func (m *mockStorage) Rename(ctx context.Context, vol storage.Volume, newName string) (storage.Volume, error) {
	m.renamed = append(m.renamed, vol.Name+"->"+newName)
	return storage.Volume{Name: newName, Dataset: "pool/branches/" + newName, Path: "/pool/branches/" + newName}, nil
}

func (m *mockStorage) DeleteAsync(ctx context.Context, vol storage.Volume) (storage.JobID, error) {
	m.destroyed = append(m.destroyed, vol.Dataset)
	return "done", nil
}

func (m *mockStorage) Poll(ctx context.Context, job storage.JobID) (storage.JobStatus, error) {
	return storage.JobCompleted, nil
}

func (m *mockStorage) SnapshotBase(ctx context.Context, tag string) (storage.SnapshotRef, error) {
	return storage.SnapshotRef("pool/base@" + tag), nil
}

func (m *mockStorage) PromoteBranch(ctx context.Context, vol storage.Volume, tag string) (storage.SnapshotRef, error) {
	if m.promoteErr != nil {
		return "", m.promoteErr
	}
	return storage.SnapshotRef(vol.Dataset + "@" + tag), nil
}

func (m *mockStorage) ListSnapshots(ctx context.Context) ([]storage.SnapshotRef, error) {
	return nil, nil
}

func (m *mockStorage) UsedBytes(ctx context.Context, vol storage.Volume) (int64, error) {
	return 1024, nil
}

func (m *mockStorage) CurrentBaseline() storage.SnapshotRef { return "pool/base@baseline" }

func (m *mockStorage) DeleteBaselineSnapshot(ctx context.Context, snap storage.SnapshotRef) error {
	m.baselinesDeleted = append(m.baselinesDeleted, string(snap))
	return nil
}

func (m *mockStorage) PoolCapacity(ctx context.Context) (int64, int64, error) {
	return m.poolUsed, m.poolTotal, nil
}

func (m *mockStorage) ListBranchVolumes(ctx context.Context) ([]string, error) {
	return m.volumes, nil
}

func (m *mockStorage) LogicalBytes(ctx context.Context, vol storage.Volume) (int64, error) {
	return 500 * 1024 * 1024 * 1024, nil // 論理 500GiB(CoW: private とは別)
}

type mockEngine struct {
	started         []string
	stopped         []string
	killed          []string
	startErr        error
	connCount       int   // ConnCount が返す接続数(#41)
	connErr         error // ConnCount が返すエラー
	exposed         []string
	exposeErr       error
	exposureChecked []engine.Instance
}

type overlapCounter struct {
	mu     sync.Mutex
	active int
	max    int
}

func (c *overlapCounter) ConnCount(context.Context, engine.Instance) (int, error) {
	c.mu.Lock()
	c.active++
	if c.active > c.max {
		c.max = c.active
	}
	c.mu.Unlock()
	time.Sleep(10 * time.Millisecond)
	c.mu.Lock()
	c.active--
	c.mu.Unlock()
	return 0, nil
}

func (m *mockEngine) Start(ctx context.Context, ins engine.Instance) error {
	if m.startErr != nil {
		return m.startErr
	}
	m.started = append(m.started, ins.Branch)
	return nil
}
func (m *mockEngine) Stop(ctx context.Context, ins engine.Instance) error {
	m.stopped = append(m.stopped, ins.Branch)
	return nil
}
func (m *mockEngine) Kill(ctx context.Context, ins engine.Instance) error {
	m.killed = append(m.killed, ins.Branch)
	return nil
}
func (m *mockEngine) WaitReady(ctx context.Context, ins engine.Instance) error { return nil }
func (m *mockEngine) IsRunning(ctx context.Context, ins engine.Instance) (bool, error) {
	return false, nil
}
func (m *mockEngine) ConnCount(ctx context.Context, ins engine.Instance) (int, error) {
	return m.connCount, m.connErr
}
func (m *mockEngine) ExposedListeners(ctx context.Context, instances []engine.Instance) ([]string, error) {
	m.exposureChecked = append([]engine.Instance(nil), instances...)
	return m.exposed, m.exposeErr
}

// --- helpers ---

// testStorage は newTestManager に渡せる storage。BasePath 付きの
// mock(mockStorageWithBase)も差し込めるようインターフェースで受ける。
type testStorage interface {
	storage.Storage
	BaselineProvider
}

func newTestManager(t *testing.T, st testStorage, eng *mockEngine, hooksDir string) *Manager {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var hr *hooks.Runner
	if hooksDir != "" {
		hr = hooks.NewRunner(hooksDir, t.TempDir(), time.Minute)
	}
	m, err := New(Config{
		NamePattern: `^[a-z0-9-]{1,32}$`,
		MaxBranches: 3,
		PortLow:     3401,
		PortHigh:    3403,
		EngineType:  "mysql",
		StateDir:    t.TempDir(),
	}, st, st, eng, hr, db)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func newTestManagerCfg(t *testing.T, st testStorage, eng *mockEngine, hooksDir string, mod func(*Config)) *Manager {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var hr *hooks.Runner
	if hooksDir != "" {
		hr = hooks.NewRunner(hooksDir, t.TempDir(), time.Minute)
	}
	cfg := Config{
		NamePattern: `^[a-z0-9-]{1,32}$`,
		MaxBranches: 3,
		PortLow:     3401,
		PortHigh:    3403,
		EngineType:  "mysql",
		StateDir:    t.TempDir(),
	}
	if mod != nil {
		mod(&cfg)
	}
	m, err := New(cfg, st, st, eng, hr, db)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// --- tests ---

func TestCreateHappyPath(t *testing.T) {
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}}
	eng := &mockEngine{}
	m := newTestManager(t, st, eng, "")

	info, err := m.Create(context.Background(), "pr-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if info.State != state.StateRunning {
		t.Errorf("state = %s, want running", info.State)
	}
	if info.Port != 3401 {
		t.Errorf("port = %d, want 3401", info.Port)
	}
	if len(st.cloned) != 1 || st.cloned[0] != "pr-1" {
		t.Errorf("cloned = %v", st.cloned)
	}
	// hook なし経路: @init は起動前に撮られ、start は 1 回だけ
	if len(st.snapshots) != 1 {
		t.Errorf("snapshots = %v", st.snapshots)
	}
	if len(eng.started) != 1 {
		t.Errorf("started = %v", eng.started)
	}
	if len(eng.stopped) != 0 {
		t.Errorf("stopped = %v, want none", eng.stopped)
	}
}

func TestCreateInvalidName(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	for _, name := range []string{"", "UPPER", "has_underscore", "日本語", "a b"} {
		if _, err := m.Create(context.Background(), name, 0); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Create(%q) err = %v, want ErrInvalidName", name, err)
		}
	}
}

func TestCreateDuplicate(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(context.Background(), "pr-1", 0); !errors.Is(err, ErrExists) {
		t.Errorf("err = %v, want ErrExists", err)
	}
}

func TestCreatePortAllocation(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	a, _ := m.Create(context.Background(), "pr-1", 0)
	b, _ := m.Create(context.Background(), "pr-2", 0)
	if a.Port == b.Port {
		t.Errorf("duplicate ports: %d", a.Port)
	}
	// 明示ポート指定
	c, err := m.Create(context.Background(), "pr-3", 3403)
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 3403 {
		t.Errorf("port = %d, want 3403", c.Port)
	}
}

func TestCreateLimitAndPortExhaustion(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	for i := 1; i <= 3; i++ {
		if _, err := m.Create(context.Background(), fmt.Sprintf("pr-%d", i), 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Create(context.Background(), "pr-4", 0); !errors.Is(err, ErrLimitReached) {
		t.Errorf("err = %v, want ErrLimitReached", err)
	}
}

func TestCreateStorageFailureLeavesErrorState(t *testing.T) {
	st := &mockStorage{cloneErr: errors.New("boom")}
	m := newTestManager(t, st, &mockEngine{}, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err == nil {
		t.Fatal("want error")
	}
	// error 状態で残る(自動削除しない)
	info, err := m.Get(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if info.State != state.StateError {
		t.Errorf("state = %s, want error", info.State)
	}
}

func TestCreateWithHookTakesCleanInitSnapshot(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "on-create.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := &mockStorage{}
	eng := &mockEngine{}
	m := newTestManager(t, st, eng, dir)

	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	// hook あり経路: start → hook → stop → @init → start
	if len(eng.started) != 2 {
		t.Errorf("started %d times, want 2", len(eng.started))
	}
	if len(eng.stopped) != 1 {
		t.Errorf("stopped %d times, want 1 (clean stop before @init)", len(eng.stopped))
	}
	if len(st.snapshots) != 1 {
		t.Errorf("snapshots = %v", st.snapshots)
	}
}

// provenance(#81)が on-create hook の環境変数へ素通しされること。
func TestCreateWithMetaPassesProvenanceToHook(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "env.out")
	script := "#!/bin/sh\necho \"owner=$SASHIKI_OWNER purpose=$SASHIKI_PURPOSE profile=$SASHIKI_PROFILE source=$SASHIKI_SOURCE_JSON rev=${SASHIKI_BASELINE_SCHEMA_REVISION-unset}\" > " + out + "\n"
	if err := os.WriteFile(filepath.Join(dir, "on-create.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, dir)
	// origin baseline に schema_revision を登録しておく(素通し検証用)
	if err := m.db.RegisterBaseline("pool/base@baseline",
		state.BaselineProvenance{SchemaRevision: "20260905_042"}); err != nil {
		t.Fatal(err)
	}

	_, err := m.CreateWithMeta(context.Background(), "pr-1", 0, state.Meta{
		Owner:   "alice",
		Purpose: "review",
		Source:  `{"type":"github_pr","ref":"42"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	env := strings.TrimSpace(string(got))
	for _, want := range []string{
		"owner=alice", "purpose=review",
		`source={"type":"github_pr","ref":"42"}`, "rev=20260905_042",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("hook env = %q, want contains %q", env, want)
		}
	}
}

func TestCreateHookFailureLeavesErrorState(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "on-create.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, dir)

	if _, err := m.Create(context.Background(), "pr-1", 0); err == nil {
		t.Fatal("want error")
	}
	info, _ := m.Get(context.Background(), "pr-1")
	if info.State != state.StateError {
		t.Errorf("state = %s, want error", info.State)
	}
	if info.HookStatus["on-create"] != "failed(7)" {
		t.Errorf("hook status = %v", info.HookStatus)
	}
}

func TestResetRollsBackToInit(t *testing.T) {
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}}
	eng := &mockEngine{}
	m := newTestManager(t, st, eng, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	info, err := m.Reset(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if info.State != state.StateRunning {
		t.Errorf("state = %s", info.State)
	}
	if len(st.rollbacks) != 1 || st.rollbacks[0] != "pool/branches/pr-1@init" {
		t.Errorf("rollbacks = %v", st.rollbacks)
	}
}

func TestResetRecreateOnSlowBackend(t *testing.T) {
	st := &mockStorage{caps: storage.Capabilities{FastRollback: false}}
	eng := &mockEngine{}
	m := newTestManager(t, st, eng, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	info, err := m.Reset(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if info.State != state.StateRunning {
		t.Errorf("state = %s", info.State)
	}
	// 作り直し: clone が 2 回(create + reset)、rollback は呼ばれない
	if len(st.cloned) != 2 {
		t.Errorf("cloned %d times, want 2 (recreate)", len(st.cloned))
	}
	if len(st.rollbacks) != 0 {
		t.Errorf("rollback should not be used on slow backend")
	}
	// 旧ボリュームは非同期削除に投入される
	if len(st.destroyed) != 1 {
		t.Errorf("old volume should be destroyed: %v", st.destroyed)
	}
	// エンジンは旧 Kill(dirty state 破棄)→ 新 start
	if len(eng.killed) < 1 || len(eng.started) < 2 {
		t.Errorf("engine kill/start: killed=%d started=%d", len(eng.killed), len(eng.started))
	}
}

func TestDeleteRemovesBranchAndFreesPort(t *testing.T) {
	st := &mockStorage{}
	m := newTestManager(t, st, &mockEngine{}, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(context.Background(), "pr-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(context.Background(), "pr-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if len(st.destroyed) != 1 {
		t.Errorf("destroyed = %v", st.destroyed)
	}
	// ポートが解放されて再利用できる
	info, err := m.Create(context.Background(), "pr-2", 3401)
	if err != nil {
		t.Fatal(err)
	}
	if info.Port != 3401 {
		t.Errorf("port = %d", info.Port)
	}
}

func TestDeleteNotFound(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	if err := m.Delete(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestRouteBranchLazyCreate(t *testing.T) {
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true, TypicalCreate: 2 * time.Second}}
	eng := &mockEngine{}
	m := newTestManagerCfg(t, st, eng, "", func(c *Config) { c.LazyCreate = true })

	port, err := m.RouteBranch(context.Background(), "pr-9")
	if err != nil {
		t.Fatal(err)
	}
	if port != 3401 {
		t.Errorf("port = %d", port)
	}
	info, _ := m.Get(context.Background(), "pr-9")
	if info.State != state.StateRunning {
		t.Errorf("state = %s", info.State)
	}
	// 2 回目は既存を返す(再作成しない)
	if _, err := m.RouteBranch(context.Background(), "pr-9"); err != nil {
		t.Fatal(err)
	}
	if len(st.cloned) != 1 {
		t.Errorf("cloned %d times, want 1", len(st.cloned))
	}
}

func TestRouteBranchLazyDisabled(t *testing.T) {
	m := newTestManagerCfg(t, &mockStorage{}, &mockEngine{}, "", func(c *Config) { c.LazyCreate = false })
	if _, err := m.RouteBranch(context.Background(), "pr-9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestRouteBranchLazyGatedBySlowBackend(t *testing.T) {
	st := &mockStorage{caps: storage.Capabilities{TypicalCreate: 70 * time.Second}}
	m := newTestManagerCfg(t, st, &mockEngine{}, "", func(c *Config) {
		c.LazyCreate = true
		c.LazyMaxWait = 20 * time.Second
	})
	// fsx 級の遅いバックエンドでは lazy create は無効(仕様 15-3)
	if _, err := m.RouteBranch(context.Background(), "pr-9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestWakeStartsStoppedBranch(t *testing.T) {
	st := &mockStorage{}
	eng := &mockEngine{}
	m := newTestManager(t, st, eng, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	_ = m.db.SetState("pr-1", state.StateSleeping, "")
	started := len(eng.started)
	info, err := m.Wake(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if info.State != state.StateRunning {
		t.Errorf("state = %s", info.State)
	}
	if len(eng.started) != started+1 {
		t.Errorf("engine should be started once more")
	}
}

func TestRouteBranchWaitsForCreating(t *testing.T) {
	m := newTestManagerCfg(t, &mockStorage{}, &mockEngine{}, "", func(c *Config) {
		c.LazyCreate = true
		c.LazyMaxWait = 2 * time.Second
	})
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	// creating 状態に巻き戻して、別 goroutine が完了させるのを待つ挙動を再現
	_ = m.db.SetState("pr-1", state.StateCreating, "")
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = m.db.SetState("pr-1", state.StateRunning, "")
	}()
	port, err := m.RouteBranch(context.Background(), "pr-1")
	if err != nil {
		t.Fatalf("should wait for creating branch: %v", err)
	}
	if port != 3401 {
		t.Errorf("port = %d", port)
	}
}

func TestWakeRejectsErrorAndDeleting(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{state.StateError, state.StateDeleting, state.StateCreating} {
		_ = m.db.SetState("pr-1", st, "")
		if _, err := m.Wake(context.Background(), "pr-1"); err == nil {
			t.Errorf("Wake should reject state %s", st)
		}
	}
}

func TestReapIdleStopAndTTLDelete(t *testing.T) {
	st := &mockStorage{}
	eng := &mockEngine{}
	m := newTestManagerCfg(t, st, eng, "", func(c *Config) {
		c.IdleStopAfter = 10 * time.Millisecond
		c.DeleteAfterIdle = time.Hour
	})
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := m.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, _ := m.Get(context.Background(), "pr-1")
	if info.State != state.StateSleeping {
		t.Errorf("state = %s, want sleeping", info.State)
	}
	if len(eng.stopped) != 1 {
		t.Errorf("engine stopped %d times", len(eng.stopped))
	}

	// TTL: delete_after_idle を縮めて再パス → 削除される
	m.cfg.DeleteAfterIdle = 10 * time.Millisecond
	if err := m.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(context.Background(), "pr-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound after TTL delete", err)
	}
}

func TestReapStopsButDoesNotDeleteBaselineBackingBranch(t *testing.T) {
	ctx := context.Background()
	eng := &mockEngine{}
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}}
	m := newTestManagerCfg(t, st, eng, "", func(c *Config) {
		c.IdleStopAfter = time.Nanosecond
		c.DeleteAfterIdle = time.Nanosecond
	})
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PromoteBranch(ctx, "pr-1", PromoteOptions{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if err := m.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := m.Get(ctx, "pr-1")
	if err != nil {
		t.Fatalf("baseline backing branch must survive reaper: %v", err)
	}
	if info.State != state.StateSleeping {
		t.Errorf("state = %s, want sleeping", info.State)
	}
	if len(info.BackingBaselines) == 0 {
		t.Error("promote source should still retain its baseline")
	}
	// sleeping になった後の次 tick でも Delete を試さず保持する。
	if err := m.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(ctx, "pr-1"); err != nil {
		t.Fatalf("protected sleeping branch must survive subsequent reaper passes: %v", err)
	}
}

// error 状態は delete_after_idle では消さない(調査のため)。error_retention を
// 過ぎたら消す(#298)。
func TestReapKeepsErrorBranchesUntilRetention(t *testing.T) {
	ctx := context.Background()
	m := newTestManagerCfg(t, &mockStorage{}, &mockEngine{}, "", func(c *Config) {
		c.DeleteAfterIdle = time.Nanosecond
		c.ErrorRetention = time.Hour
	})
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	_ = m.db.SetState("pr-1", state.StateError, "boom")
	time.Sleep(5 * time.Millisecond)
	if err := m.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := m.db.GetBranch("pr-1")
	if err != nil {
		t.Fatalf("error branch within retention should be kept: %v", err)
	}
	if b.ErrorAt == nil {
		t.Fatal("error_at should be recorded when entering error")
	}
	if err := m.db.SetErrorAtForTest("pr-1", time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(ctx, "pr-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("error branch past retention should be deleted, got %v", err)
	}
}

// error_retention: 0 は従来どおり残す。
func TestReapKeepsErrorBranchesWhenRetentionDisabled(t *testing.T) {
	ctx := context.Background()
	m := newTestManagerCfg(t, &mockStorage{}, &mockEngine{}, "", func(c *Config) {
		c.DeleteAfterIdle = time.Nanosecond
	})
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	_ = m.db.SetState("pr-1", state.StateError, "boom")
	_ = m.db.SetErrorAtForTest("pr-1", time.Now().Add(-1000*time.Hour))
	if err := m.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(ctx, "pr-1"); err != nil {
		t.Errorf("error branch should be kept when error_retention is 0: %v", err)
	}
}

// lease 失効は error 状態でも削除する(#298)。
func TestReapDeletesExpiredLeaseInErrorState(t *testing.T) {
	ctx := context.Background()
	m := newTestManagerCfg(t, &mockStorage{}, &mockEngine{}, "", nil)
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	_ = m.db.SetError("pr-1", "create", CodeHookFailed, true, "hook failed", nil)
	if err := m.db.SetExpiresAt("pr-1", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := m.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(ctx, "pr-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired lease should delete an error branch, got %v", err)
	}
}

// error_at は error を抜けると消え、再度 error になった時刻で数え直す。
func TestErrorAtResetsWhenLeavingError(t *testing.T) {
	ctx := context.Background()
	m := newTestManagerCfg(t, &mockStorage{}, &mockEngine{}, "", nil)
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	_ = m.db.SetState("pr-1", state.StateError, "boom")
	_ = m.db.SetState("pr-1", state.StateRunning, "")
	if b, _ := m.db.GetBranch("pr-1"); b.ErrorAt != nil {
		t.Errorf("error_at should clear when leaving error, got %v", b.ErrorAt)
	}
}

func TestMemoryGuardRejectsCreate(t *testing.T) {
	m := newTestManagerCfg(t, &mockStorage{}, &mockEngine{}, "", func(c *Config) {
		c.BufferPoolBytes = 256 * 1024 * 1024
		c.AvailableMem = func() (int64, error) { return 100 * 1024 * 1024, nil } // 100MB
	})
	if _, err := m.Create(context.Background(), "pr-1", 0); !errors.Is(err, ErrLimitReached) {
		t.Errorf("err = %v, want ErrLimitReached", err)
	}
	// 十分な空きなら通る
	m.cfg.AvailableMem = func() (int64, error) { return 2 << 30, nil }
	if _, err := m.Create(context.Background(), "pr-2", 0); err != nil {
		t.Fatal(err)
	}
}

func TestReapSkipsBranchesWithActiveConnections(t *testing.T) {
	eng := &mockEngine{}
	m := newTestManagerCfg(t, &mockStorage{}, eng, "", func(c *Config) {
		c.IdleStopAfter = time.Nanosecond
		c.DeleteAfterIdle = time.Millisecond
	})
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	// 長寿命接続が 1 本張られている状態
	m.SetActiveConns(func(name string) int { return 1 })
	time.Sleep(5 * time.Millisecond)
	if err := m.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := m.Get(context.Background(), "pr-1")
	if err != nil {
		t.Fatalf("branch should survive while connected: %v", err)
	}
	if info.State != state.StateRunning {
		t.Errorf("state = %s, want running (in use)", info.State)
	}
	// 接続が切れたら回収される
	m.SetActiveConns(func(name string) int { return 0 })
	if err := m.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(context.Background(), "pr-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("should be reaped after disconnect: %v", err)
	}
}

func TestResetRecreateRerunsOnCreateHook(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "count")
	script := "#!/bin/sh\necho x >> " + marker + "\n"
	if err := os.WriteFile(filepath.Join(dir, "on-create.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	st := &mockStorage{caps: storage.Capabilities{FastRollback: false}}
	m := newTestManager(t, st, &mockEngine{}, dir)
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reset(context.Background(), "pr-1"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(marker)
	// create で1回 + reset(作り直し)で1回 = 2回(zfs の @init 契約と等価)
	if got := len(strings.Split(strings.TrimSpace(string(data)), "\n")); got != 2 {
		t.Errorf("on-create ran %d times, want 2", got)
	}
}

func TestRecreateFromCurrentBaseline(t *testing.T) {
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}}
	eng := &mockEngine{}
	m := newTestManager(t, st, eng, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	// current baseline を切り替える(baseline refresh 相当)
	if err := m.db.SetCurrentBaseline("pool/base@baseline-new"); err != nil {
		t.Fatal(err)
	}
	info, err := m.Recreate(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if info.State != state.StateRunning {
		t.Errorf("state = %s", info.State)
	}
	// origin が current baseline に更新される
	b, _ := m.db.GetBranch("pr-1")
	if b.OriginSnapshot != "pool/base@baseline-new" {
		t.Errorf("origin = %q, want current baseline", b.OriginSnapshot)
	}
	// 作り直し: clone 2回(create + recreate)、旧 volume は destroy
	if len(st.cloned) != 2 {
		t.Errorf("cloned %d times, want 2", len(st.cloned))
	}
	if len(st.destroyed) != 1 {
		t.Errorf("old volume should be destroyed: %v", st.destroyed)
	}
}

func TestResetUsesKillNotGraceful(t *testing.T) {
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}}
	eng := &mockEngine{}
	m := newTestManager(t, st, eng, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reset(context.Background(), "pr-1"); err != nil {
		t.Fatal(err)
	}
	// rollback 前は Kill(graceful Stop ではない)
	if len(eng.killed) != 1 {
		t.Errorf("reset should Kill (fast), killed=%v", eng.killed)
	}
}

func TestRecreatePrefersOnRecreateHook(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "which")
	if err := os.WriteFile(filepath.Join(dir, "on-recreate.sh"),
		[]byte("#!/bin/sh\necho recreate > "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "on-create.sh"),
		[]byte("#!/bin/sh\necho create > "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}}
	m := newTestManager(t, st, &mockEngine{}, dir)
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Recreate(context.Background(), "pr-1"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(marker)
	if strings.TrimSpace(string(data)) != "recreate" {
		t.Errorf("recreate should prefer on-recreate hook, got %q", data)
	}
}

func TestRecreateFixedNameUsesRenameSwap(t *testing.T) {
	// ClonesAreDistinct=false(ebs-zfs 相当)では旧を退避してから clone する
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true, ClonesAreDistinct: false}}
	m := newTestManager(t, st, &mockEngine{}, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if err := m.db.SetCurrentBaseline("pool/base@baseline-new"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Recreate(context.Background(), "pr-1"); err != nil {
		t.Fatal(err)
	}
	if len(st.renamed) == 0 {
		t.Error("fixed-name backend recreate should rename-swap the old volume")
	}
}

func TestCreateFailureRecordsDiagnostics(t *testing.T) {
	st := &mockStorage{cloneErr: errors.New("clone boom")}
	m := newTestManager(t, st, &mockEngine{}, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err == nil {
		t.Fatal("want error")
	}
	b, err := m.db.GetBranch("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.State != state.StateError {
		t.Errorf("state = %s", b.State)
	}
	if b.FailedOp != "create" || b.ErrorCode != CodeCloneFailed || !b.Recoverable {
		t.Errorf("diagnostics: op=%s code=%s recoverable=%v", b.FailedOp, b.ErrorCode, b.Recoverable)
	}
	if len(b.SuggestedActions) == 0 {
		t.Error("suggested_actions should be populated")
	}
}

func TestRetryRerunsFailedCreate(t *testing.T) {
	// clone が最初は失敗、retry 時は成功する mock
	st := &mockStorage{cloneErr: errors.New("transient")}
	m := newTestManager(t, st, &mockEngine{}, "")
	_, _ = m.Create(context.Background(), "pr-1", 0)
	// 復旧
	st.cloneErr = nil
	info, err := m.Retry(context.Background(), "pr-1")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if info.State != state.StateRunning {
		t.Errorf("state = %s, want running", info.State)
	}
}

func TestRetryRejectsNonRecoverable(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	// 手動で non-recoverable error に
	_ = m.db.SetError("pr-1", "create", CodeSnapshotError, false, "boom", nil)
	if _, err := m.Retry(context.Background(), "pr-1"); err == nil {
		t.Error("retry should reject non-recoverable error")
	}
}

func TestRetryRejectsNonErrorState(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Retry(context.Background(), "pr-1"); err == nil {
		t.Error("retry on running branch should fail")
	}
}

func TestCreateWithMetaStoresProvenance(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	meta := state.Meta{
		Profile: "preview", Owner: "alice", Purpose: "review",
		Source: `{"type":"github_pr","repository":"shop","ref":"123"}`,
	}
	if _, err := m.CreateWithMeta(context.Background(), "pr-1", 0, meta); err != nil {
		t.Fatal(err)
	}
	b, err := m.db.GetBranch("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Profile != "preview" || b.Owner != "alice" || b.Purpose != "review" {
		t.Errorf("provenance = %+v", b)
	}
	// core は source を解釈しない(そのまま保持)
	if b.Source != meta.Source {
		t.Errorf("source = %q, want opaque passthrough", b.Source)
	}
}

func TestBaselineGCRespectsRefsAndCurrent(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	// 3 世代の baseline を登録
	for _, tag := range []string{"pool/base@b1", "pool/base@b2", "pool/base@b3"} {
		if err := m.db.RegisterBaseline(tag, state.BaselineProvenance{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	// b3 を current に
	if err := m.db.SetCurrentBaseline("pool/base@b3"); err != nil {
		t.Fatal(err)
	}
	// b1 を参照する branch を作る(手動で origin を設定)
	if err := m.db.CreateBranch("pr-1", 3401, "pool/base@b1"); err != nil {
		t.Fatal(err)
	}
	// keep_last=1 で GC: b3(current)と b1(参照中)は残る、b2 は消える
	res, err := m.GCBaselines(context.Background(), GCConfig{KeepLast: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deleted) != 1 || res.Deleted[0] != "pool/base@b2" {
		t.Errorf("deleted = %v, want [b2]", res.Deleted)
	}
}

func TestSetBaselineRequiresRegistered(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	if err := m.SetBaseline(context.Background(), "pool/base@unknown"); err == nil {
		t.Error("set to unregistered baseline should fail")
	}
	_ = m.db.RegisterBaseline("pool/base@known", state.BaselineProvenance{})
	if err := m.SetBaseline(context.Background(), "pool/base@known"); err != nil {
		t.Errorf("set to registered baseline should work: %v", err)
	}
}

func TestMaxRunningLimitsConcurrentEngines(t *testing.T) {
	m := newTestManagerCfg(t, &mockStorage{}, &mockEngine{}, "", func(c *Config) {
		c.MaxRunning = 2
		c.MaxBranches = 10
	})
	for i := 1; i <= 2; i++ {
		if _, err := m.Create(context.Background(), fmt.Sprintf("pr-%d", i), 0); err != nil {
			t.Fatal(err)
		}
	}
	// 3本目は max_running で拒否(volume 数 MaxBranches とは別)
	_, err := m.Create(context.Background(), "pr-3", 0)
	if !errors.Is(err, ErrLimitReached) {
		t.Errorf("err = %v, want ErrLimitReached (max_running)", err)
	}
	if err != nil && !strings.Contains(err.Error(), "max_running") {
		t.Errorf("error should mention max_running: %v", err)
	}
}

func TestExpectedRSSAdmission(t *testing.T) {
	m := newTestManagerCfg(t, &mockStorage{}, &mockEngine{}, "", func(c *Config) {
		c.ExpectedRSSBytes = 400 * 1024 * 1024                                   // 400MB/本
		c.AvailableMem = func() (int64, error) { return 500 * 1024 * 1024, nil } // 500MB
	})
	// 必要 = 400(rss) + 400(headroom=rss) = 800MB > 500 → 拒否
	if _, err := m.Create(context.Background(), "pr-1", 0); !errors.Is(err, ErrLimitReached) {
		t.Errorf("err = %v, want ErrLimitReached (memory)", err)
	}
	// 空きを増やせば通る
	m.cfg.AvailableMem = func() (int64, error) { return 2 << 30, nil }
	if _, err := m.Create(context.Background(), "pr-2", 0); err != nil {
		t.Fatal(err)
	}
}

func TestWakeChecksAdmission(t *testing.T) {
	m := newTestManagerCfg(t, &mockStorage{}, &mockEngine{}, "", func(c *Config) {
		c.ExpectedRSSBytes = 400 * 1024 * 1024
		c.AvailableMem = func() (int64, error) { return 10 << 30, nil } // 潤沢
	})
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	_ = m.db.SetState("pr-1", state.StateSleeping, "")
	// メモリ枯渇に切り替え
	m.cfg.AvailableMem = func() (int64, error) { return 100 * 1024 * 1024, nil }
	if _, err := m.Wake(context.Background(), "pr-1"); !errors.Is(err, ErrLimitReached) {
		t.Errorf("wake should check admission: %v", err)
	}
}

func TestAdmitStorageRejectsAboveCriticalWatermark(t *testing.T) {
	st := &mockStorage{poolUsed: 95, poolTotal: 100} // 95%
	m := newTestManagerCfg(t, st, &mockEngine{}, "", func(c *Config) {
		c.CriticalWatermark = 0.9
	})
	if _, err := m.Create(context.Background(), "pr-1", 0); !errors.Is(err, ErrLimitReached) {
		t.Errorf("err = %v, want ErrLimitReached (storage)", err)
	}
	if err := (func() error { _, e := m.Create(context.Background(), "pr-2", 0); return e })(); err != nil && !strings.Contains(err.Error(), "storage") {
		t.Errorf("error should mention storage: %v", err)
	}
	// 使用率を下げれば通る
	st.poolUsed = 50
	if _, err := m.Create(context.Background(), "pr-ok", 0); err != nil {
		t.Fatal(err)
	}
}

func TestCapacityReportsPoolAndPorts(t *testing.T) {
	st := &mockStorage{poolUsed: 30, poolTotal: 100}
	m := newTestManagerCfg(t, st, &mockEngine{}, "", func(c *Config) {
		c.HighWatermark = 0.8
		c.CriticalWatermark = 0.95
	})
	_, _ = m.Create(context.Background(), "pr-1", 0)
	cap, err := m.Capacity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cap.PoolUsedRatio != 0.3 {
		t.Errorf("pool ratio = %v, want 0.3", cap.PoolUsedRatio)
	}
	if cap.PortsUsed != 1 {
		t.Errorf("ports used = %d, want 1", cap.PortsUsed)
	}
	if cap.Running != 1 {
		t.Errorf("running = %d", cap.Running)
	}
}

func TestInfoReportsLogicalAndPrivate(t *testing.T) {
	st := &mockStorage{}
	m := newTestManager(t, st, &mockEngine{}, "")
	info, err := m.Create(context.Background(), "pr-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if info.LogicalBytes <= info.UsedBytes {
		t.Errorf("logical (%d) should exceed private (%d) for CoW clone", info.LogicalBytes, info.UsedBytes)
	}
}

func TestReconcileDemotesDeadRunningBranch(t *testing.T) {
	st := &mockStorage{}
	// IsRunning が false を返す mockEngine(既定)
	m := newTestManager(t, st, &mockEngine{}, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	// state は running のまま、プロセスは死んでいる想定 → reconcile で sleeping
	rep, err := m.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Demoted) != 1 || rep.Demoted[0] != "pr-1" {
		t.Errorf("demoted = %v, want [pr-1]", rep.Demoted)
	}
	b, _ := m.db.GetBranch("pr-1")
	if b.State != state.StateSleeping {
		t.Errorf("state = %s, want sleeping", b.State)
	}
}

func reconcileHas(list []string, name string) bool {
	for _, s := range list {
		if s == name {
			return true
		}
	}
	return false
}

// 再起動で creating のまま残ったブランチは reconcile で error(recoverable, op=create)
// に回収され、retry で再駆動できる(#reconcile-interrupted)。
func TestReconcileRecoversInterruptedCreating(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	_ = m.db.SetState("pr-1", state.StateCreating, "") // クラッシュ残骸を模す
	rep, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reconcileHas(rep.Interrupted, "pr-1") {
		t.Errorf("interrupted = %v, want [pr-1]", rep.Interrupted)
	}
	b, _ := m.db.GetBranch("pr-1")
	if b.State != state.StateError || b.FailedOp != "create" || !b.Recoverable {
		t.Errorf("branch = state:%s op:%s recoverable:%v, want error/create/true", b.State, b.FailedOp, b.Recoverable)
	}
	if _, err := m.Retry(ctx, "pr-1"); err != nil {
		t.Errorf("retry after interrupted create should work: %v", err)
	}
}

// resetting のまま残ったブランチは error(op=reset)に回収される。
func TestReconcileRecoversInterruptedResetting(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	_ = m.db.SetState("pr-1", state.StateResetting, "")
	rep, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reconcileHas(rep.Interrupted, "pr-1") {
		t.Errorf("interrupted = %v, want [pr-1]", rep.Interrupted)
	}
	b, _ := m.db.GetBranch("pr-1")
	if b.State != state.StateError || b.FailedOp != "reset" || !b.Recoverable {
		t.Errorf("branch = state:%s op:%s recoverable:%v, want error/reset/true", b.State, b.FailedOp, b.Recoverable)
	}
}

// deleting のまま残ったブランチは reconcile で削除を完了する(行が消える)。
func TestReconcileResumesInterruptedDelete(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	_ = m.db.SetState("pr-1", state.StateDeleting, "")
	rep, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reconcileHas(rep.Interrupted, "pr-1") {
		t.Errorf("interrupted = %v, want [pr-1]", rep.Interrupted)
	}
	if _, err := m.db.GetBranch("pr-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("interrupted-deleting branch should be removed, got err=%v", err)
	}
}

func TestReconcileDetectsOrphans(t *testing.T) {
	st := &mockStorage{volumes: []string{"pr-1", "pr-orphan"}}
	m := newTestManager(t, st, &mockEngine{}, "")
	// pr-1 だけ state.db に作る。pr-orphan は dataset のみ
	if err := m.db.CreateBranch("pr-1", 3401, "pool/base@baseline"); err != nil {
		t.Fatal(err)
	}
	_ = m.db.SetState("pr-1", state.StateSleeping, "")
	rep, err := m.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Orphans) != 1 || rep.Orphans[0] != "pr-orphan" {
		t.Errorf("orphans = %v, want [pr-orphan]", rep.Orphans)
	}
	// gc --orphans で削除される
	deleted, err := m.GCOrphans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "pr-orphan" {
		t.Errorf("deleted = %v", deleted)
	}
}

func TestDoctorReportsIssues(t *testing.T) {
	st := &mockStorage{poolUsed: 50, poolTotal: 100, volumes: []string{"pr-1"}}
	eng := &mockEngine{exposed: []string{"pr-1 (10.0.0.10:3401)"}}
	m := newTestManager(t, st, eng, "")
	if err := m.db.CreateBranch("pr-1", 3401, "pool/base@b1"); err != nil {
		t.Fatal(err)
	}
	_ = m.db.SetState("pr-1", state.StateRunning, "")
	if err := m.db.CreateBranch("pr-sleep", 3402, "pool/base@b1"); err != nil {
		t.Fatal(err)
	}
	_ = m.db.SetState("pr-sleep", state.StateSleeping, "")
	_ = m.db.RegisterBaseline("pool/base@b1", state.BaselineProvenance{})
	_ = m.db.SetCurrentBaseline("pool/base@b1")
	d, err := m.Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !d.PoolHealthy || d.CurrentBaseline == "" {
		t.Errorf("doctor = %+v", d)
	}
	// #88: state.db writable / checks 整形 / baseline provenance
	if !d.StateDBWritable {
		t.Error("state.db should be writable in tests")
	}
	if len(d.Checks) == 0 {
		t.Error("doctor should populate structured checks")
	}
	status := map[string]string{}
	for _, c := range d.Checks {
		status[c.Name] = c.Status
	}
	if status["state.db writable"] != checkOK {
		t.Errorf("state.db writable check = %q, want ok", status["state.db writable"])
	}
	if status["current baseline"] != checkOK {
		t.Errorf("current baseline check = %q, want ok", status["current baseline"])
	}
	// provenance 空なので masked / validated は warn
	if status["baseline masked"] != checkWarn {
		t.Errorf("baseline masked check = %q, want warn (empty provenance)", status["baseline masked"])
	}
	if status["baseline validated"] != checkWarn {
		t.Errorf("baseline validated check = %q, want warn (empty provenance)", status["baseline validated"])
	}
	if status["branch listener exposure"] != checkWarn {
		t.Errorf("branch listener exposure check = %q, want warn", status["branch listener exposure"])
	}
	if len(d.ExposedListeners) != 1 || !strings.Contains(d.ExposedListeners[0], "10.0.0.10:3401") {
		t.Errorf("exposed listeners = %v", d.ExposedListeners)
	}
	if len(eng.exposureChecked) != 1 || eng.exposureChecked[0].Branch != "pr-1" {
		t.Errorf("listener exposure should check running branches only, got %+v", eng.exposureChecked)
	}
}

func TestResetWorksAfterRecreate(t *testing.T) {
	// recreate が @init を取っていないと、後続の reset が Rollback 先を失う(Fix 2)。
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}}
	m := newTestManager(t, st, &mockEngine{}, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if err := m.db.SetCurrentBaseline("pool/base@baseline-new"); err != nil {
		t.Fatal(err)
	}
	snapsBefore := len(st.snapshots)
	if _, err := m.Recreate(context.Background(), "pr-1"); err != nil {
		t.Fatal(err)
	}
	// recreate は新しい @init を取得しているはず
	if len(st.snapshots) <= snapsBefore {
		t.Error("recreate should take a fresh @init snapshot")
	}
	// その @init に対して reset が成功する
	if _, err := m.Reset(context.Background(), "pr-1"); err != nil {
		t.Fatalf("reset after recreate should work (valid @init): %v", err)
	}
}

// --- #28 drain ---

func TestDrainSleepsRunningBranches(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(context.Background(), "pr-2", 0); err != nil {
		t.Fatal(err)
	}
	// pr-2 は既に sleeping
	_ = m.db.SetState("pr-2", state.StateSleeping, "")

	res, err := m.Drain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Slept) != 1 || res.Slept[0] != "pr-1" {
		t.Errorf("slept = %v, want [pr-1]", res.Slept)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "pr-2" {
		t.Errorf("skipped = %v, want [pr-2]", res.Skipped)
	}
	if len(res.Failed) != 0 {
		t.Errorf("failed = %v", res.Failed)
	}
	info, _ := m.Get(context.Background(), "pr-1")
	if info.State != state.StateSleeping {
		t.Errorf("pr-1 state = %s, want sleeping", info.State)
	}
}

// --- #34 profile / lease ---

func withProfiles(c *Config) {
	c.DefaultProfile = "preview"
	c.Profiles = map[string]ProfilePolicy{
		"preview": {IdleStopAfter: 30 * time.Minute, DeleteAfterIdle: 168 * time.Hour},
		"ci":      {IdleStopAfter: 10 * time.Millisecond, DeleteAfterIdle: time.Hour},
	}
}

func TestCreateResolvesProfile(t *testing.T) {
	m := newTestManagerCfg(t, &mockStorage{}, &mockEngine{}, "", withProfiles)

	// profile 未指定 → DefaultProfile("preview")が永続化される
	info, err := m.CreateWithMeta(context.Background(), "pr-1", 0, state.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Profile != "preview" {
		t.Errorf("default profile = %q, want preview", info.Profile)
	}

	// 明示 profile("ci")はそのまま
	info, err = m.CreateWithMeta(context.Background(), "pr-2", 0, state.Meta{Profile: "ci"})
	if err != nil {
		t.Fatal(err)
	}
	if info.Profile != "ci" {
		t.Errorf("profile = %q, want ci", info.Profile)
	}

	// 未知 profile は ErrUnknownProfile
	if _, err := m.CreateWithMeta(context.Background(), "pr-3", 0, state.Meta{Profile: "nope"}); !errors.Is(err, ErrUnknownProfile) {
		t.Errorf("err = %v, want ErrUnknownProfile", err)
	}
}

func TestReapUsesProfileIdle(t *testing.T) {
	eng := &mockEngine{}
	m := newTestManagerCfg(t, &mockStorage{}, eng, "", func(c *Config) {
		withProfiles(c)
		// global は長い。ci profile の 10ms が使われることを検証する。
		c.IdleStopAfter = time.Hour
		c.DeleteAfterIdle = time.Hour
	})
	if _, err := m.CreateWithMeta(context.Background(), "pr-1", 0, state.Meta{Profile: "ci"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(15 * time.Millisecond)
	if err := m.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, _ := m.Get(context.Background(), "pr-1")
	if info.State != state.StateSleeping {
		t.Errorf("state = %s, want sleeping (ci profile idle_stop=10ms)", info.State)
	}
}

func TestLeaseSetsExpiresAt(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	exp, err := m.Lease(context.Background(), "pr-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := m.Get(context.Background(), "pr-1")
	if info.ExpiresAt == nil {
		t.Fatal("expires_at not set")
	}
	if d := info.ExpiresAt.Sub(exp); d > time.Second || d < -time.Second {
		t.Errorf("expires_at = %v, want ~%v", *info.ExpiresAt, exp)
	}
	// 負の期間は拒否
	if _, err := m.Lease(context.Background(), "pr-1", 0); err == nil {
		t.Error("Lease(0) should error")
	}
}

func TestReapDeletesExpiredLeaseEvenIfActive(t *testing.T) {
	st := &mockStorage{}
	m := newTestManagerCfg(t, st, &mockEngine{}, "", func(c *Config) {
		// アイドル回収は起きない設定。だが lease 失効は使用中でも回収する。
		c.IdleStopAfter = time.Hour
		c.DeleteAfterIdle = time.Hour
		c.ActiveConns = func(string) int { return 1 } // 使用中
	})
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	// lease を過去に設定 → 失効
	if err := m.db.SetExpiresAt("pr-1", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := m.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(context.Background(), "pr-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired lease branch should be deleted even if active, got err=%v", err)
	}
}

// --- #41 connpoll (engine ポーリングで last_conn_at 更新) ---

func TestConnPollerTouchesLastConn(t *testing.T) {
	eng := &mockEngine{connCount: 2}
	m := newTestManager(t, &mockStorage{}, eng, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	before, _ := m.Get(context.Background(), "pr-1")
	if before.LastConnAt != nil {
		t.Fatal("last_conn_at should be nil before poll")
	}
	m.pollConnsOnce(context.Background(), eng)
	after, _ := m.Get(context.Background(), "pr-1")
	if after.LastConnAt == nil {
		t.Error("connpoll should set last_conn_at when connections > 0")
	}
	if got := m.activeConns("pr-1"); got != 2 {
		t.Errorf("activeConns = %d, want 2 (from poll)", got)
	}
}

func TestConnPollerProtectsOnError(t *testing.T) {
	eng := &mockEngine{connErr: errors.New("connection refused")}
	m := newTestManager(t, &mockStorage{}, eng, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	m.pollConnsOnce(context.Background(), eng)
	// 判定不能 → 使用中(1)として保護。last_conn_at は触らない。
	if got := m.activeConns("pr-1"); got != 1 {
		t.Errorf("activeConns = %d, want 1 (protected on error)", got)
	}
	after, _ := m.Get(context.Background(), "pr-1")
	if after.LastConnAt != nil {
		t.Error("last_conn_at should NOT be touched on ConnCount error")
	}
}

func TestReaperSkipsBranchWithPolledConns(t *testing.T) {
	eng := &mockEngine{connCount: 1} // 常に接続あり
	m := newTestManagerCfg(t, &mockStorage{}, eng, "", func(c *Config) {
		c.IdleStopAfter = 10 * time.Millisecond
		c.DeleteAfterIdle = time.Hour
	})
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	// poll が接続を検出 → activeConns>0 で reaper の対象外になる。
	m.pollConnsOnce(context.Background(), eng)
	time.Sleep(15 * time.Millisecond)
	if err := m.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, _ := m.Get(context.Background(), "pr-1")
	if info.State != state.StateRunning {
		t.Errorf("state = %s, want running (active conn protects from idle stop)", info.State)
	}
}

// connpoll が直接接続をまだキャッシュしていない瞬間でも、reaper は停止直前に
// engine の現在値を再確認して active branch を保護する。
func TestReaperChecksEngineBeforeIdleStop(t *testing.T) {
	eng := &mockEngine{connCount: 1}
	m := newTestManagerCfg(t, &mockStorage{}, eng, "", func(c *Config) {
		c.IdleStopAfter = 10 * time.Millisecond
		c.DeleteAfterIdle = time.Hour
	})
	if _, err := m.Create(context.Background(), "pr-race", 0); err != nil {
		t.Fatal(err)
	}
	// pollConnsOnce は意図的に呼ばない。cached count=0 のまま閾値を超える。
	time.Sleep(15 * time.Millisecond)
	if err := m.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := m.Get(context.Background(), "pr-race")
	if err != nil {
		t.Fatal(err)
	}
	if info.State != state.StateRunning {
		t.Errorf("state = %s, want running (synchronous engine check protects active conn)", info.State)
	}
	if info.LastConnAt == nil {
		t.Error("synchronous engine check should refresh last_conn_at")
	}
}

func TestReaperDoesNotProbeSleepingBranchBeforeDelete(t *testing.T) {
	eng := &mockEngine{connErr: errors.New("listener is stopped")}
	m := newTestManagerCfg(t, &mockStorage{}, eng, "", func(c *Config) {
		c.IdleStopAfter = time.Hour
		c.DeleteAfterIdle = 10 * time.Millisecond
	})
	if _, err := m.Create(context.Background(), "pr-sleeping", 0); err != nil {
		t.Fatal(err)
	}
	if err := m.db.SetState("pr-sleeping", state.StateSleeping, ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(15 * time.Millisecond)
	if err := m.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(context.Background(), "pr-sleeping"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("sleeping branch should be deleted without probing stopped listener, got %v", err)
	}
}

func TestConnectionChecksAreSerialized(t *testing.T) {
	m := &Manager{}
	counter := &overlapCounter{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.checkedConnCount(context.Background(), counter, engine.Instance{Branch: "pr-1", Port: 3401}); err != nil {
				t.Errorf("checkedConnCount: %v", err)
			}
		}()
	}
	wg.Wait()
	counter.mu.Lock()
	defer counter.mu.Unlock()
	if counter.max != 1 {
		t.Fatalf("concurrent connection checks = %d, want 1", counter.max)
	}
}

// --- #82 create --baseline ---

func TestCreateFromBaseline(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	if err := m.db.RegisterBaseline("pool/base@baseline-x", state.BaselineProvenance{}); err != nil {
		t.Fatal(err)
	}
	info, err := m.CreateWithMetaFrom(context.Background(), "pr-1", 0, state.Meta{}, "pool/base@baseline-x")
	if err != nil {
		t.Fatal(err)
	}
	if info.OriginSnapshot != "pool/base@baseline-x" {
		t.Errorf("origin = %q, want pool/base@baseline-x", info.OriginSnapshot)
	}
	// 未登録 baseline は ErrBaselineNotFound(branch 行も残さない)
	if _, err := m.CreateWithMetaFrom(context.Background(), "pr-2", 0, state.Meta{}, "nope@nope"); !errors.Is(err, ErrBaselineNotFound) {
		t.Errorf("err = %v, want ErrBaselineNotFound", err)
	}
	if _, err := m.Get(context.Background(), "pr-2"); !errors.Is(err, ErrNotFound) {
		t.Error("failed create must not leave a branch row")
	}
}

func TestCreateRecordsProvision(t *testing.T) {
	st := &mockStorage{}
	m := newTestManager(t, st, &mockEngine{}, "")
	if _, err := m.Create(context.Background(), "pr-p", 0); err != nil {
		t.Fatal(err)
	}
	b, err := m.db.GetBranch("pr-p")
	if err != nil {
		t.Fatal(err)
	}
	if b.VolumeRef == "" || b.InitSnapshot == "" {
		t.Errorf("provision should be recorded: volume_ref=%q init_snapshot=%q", b.VolumeRef, b.InitSnapshot)
	}
	if b.EngineState != "running" {
		t.Errorf("engine_state = %q, want running", b.EngineState)
	}
}

func TestResetPrefersStoredInitSnapshot(t *testing.T) {
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}}
	m := newTestManager(t, st, &mockEngine{}, "")
	if _, err := m.Create(context.Background(), "pr-r", 0); err != nil {
		t.Fatal(err)
	}
	// 記録済み @init を差し替えて、Reset がそれを使うことを確認(#90)
	if err := m.db.SetProvision("pr-r", "p/b/pr-r", "p/b/pr-r@init-gen2"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reset(context.Background(), "pr-r"); err != nil {
		t.Fatal(err)
	}
	if len(st.rollbacks) == 0 || st.rollbacks[len(st.rollbacks)-1] != "p/b/pr-r@init-gen2" {
		t.Errorf("rollback should use stored init snapshot, got %v", st.rollbacks)
	}
}

func TestSetBaselineErrorClassification(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	m.SetBaselinePolicy(RefreshConfig{RequireMasked: true})

	if err := m.SetBaseline(context.Background(), "pool/base@nope"); !errors.Is(err, ErrBaselineNotFound) {
		t.Errorf("unregistered should wrap ErrBaselineNotFound, got %v", err)
	}
	_ = m.db.RegisterBaseline("pool/base@x", state.BaselineProvenance{})
	if err := m.SetBaseline(context.Background(), "pool/base@x"); !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("unmasked should wrap ErrPreconditionFailed, got %v", err)
	}
}

// --- #85 storage quota (refquota) ---

func TestCreateAppliesQuota(t *testing.T) {
	st := &mockStorage{}
	m := newTestManagerCfg(t, st, &mockEngine{}, "", func(c *Config) {
		c.DefaultStorageQuotaBytes = 10 << 20 // 10MiB
	})
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if got := st.quota["pool/branches/pr-1"]; got != 10<<20 {
		t.Errorf("refquota = %d, want %d", got, 10<<20)
	}
}

func TestCreateNoQuotaWhenUnset(t *testing.T) {
	st := &mockStorage{}
	m := newTestManager(t, st, &mockEngine{}, "") // quota 未設定
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if len(st.quota) != 0 {
		t.Errorf("quota should not be set when DefaultStorageQuotaBytes=0, got %v", st.quota)
	}
}

// #130: baseline 更新後、origin が current より古い branch は Stale=true。
func TestInfoStaleAfterBaselineChange(t *testing.T) {
	ctx := context.Background()
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}}
	m := newTestManager(t, st, &mockEngine{}, "")
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if i, _ := m.Get(ctx, "pr-1"); i.Stale {
		t.Error("作成直後の branch は stale でないはず")
	}
	// current baseline を別 snapshot に更新
	if err := m.db.RegisterBaseline("pool/base@new", state.BaselineProvenance{}); err != nil {
		t.Fatal(err)
	}
	if err := m.db.SetCurrentBaseline("pool/base@new"); err != nil {
		t.Fatal(err)
	}
	if i, _ := m.Get(ctx, "pr-1"); !i.Stale {
		t.Error("baseline 更新後は origin が古いので stale=true のはず")
	}
}

// #129: promote は branch の datadir を新 baseline にし、current を切り替える。
func TestPromoteBranch(t *testing.T) {
	ctx := context.Background()
	eng := &mockEngine{}
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}}
	m := newTestManager(t, st, eng, "")
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	snap, err := m.PromoteBranch(ctx, "pr-1", PromoteOptions{})
	if err != nil {
		t.Fatalf("PromoteBranch: %v", err)
	}
	// mockStorage.PromoteBranch は vol.Dataset@tag を返す
	if snap == "" || !strings.Contains(snap, "pr-1@") {
		t.Errorf("promoted snapshot = %q", snap)
	}
	// current baseline が promote 先に切り替わっている
	if got := string(m.currentBaseline()); got != snap {
		t.Errorf("current baseline = %q, want %q", got, snap)
	}
	// snapshot 不変条件: 昇格前に graceful stop している
	if len(eng.stopped) == 0 {
		t.Error("promote は snapshot 前に branch を停止するはず")
	}
	// branch は使用可能な状態へ再起動される
	if len(eng.started) < 2 {
		t.Errorf("promote 後に branch を再起動するはず (started=%v)", eng.started)
	}
}

// promote 済みブランチの delete は拒否する(#179)。ebs-zfs では baseline
// snapshot が branch dataset 上にあり、zfs destroy -r が巻き込んで current
// baseline を宙吊りにするため。
func TestDeleteRefusesBranchBackingBaseline(t *testing.T) {
	ctx := context.Background()
	eng := &mockEngine{}
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}}
	m := newTestManager(t, st, eng, "")
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PromoteBranch(ctx, "pr-1", PromoteOptions{}); err != nil {
		t.Fatal(err)
	}
	// pr-1 の dataset 上に baseline があるので delete は precondition で拒否。
	err := m.Delete(ctx, "pr-1")
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("delete of promote-source branch should be refused, got %v", err)
	}
	// 拒否されても branch は残っている(状態を壊さない)。
	if _, err := m.db.GetBranch("pr-1"); err != nil {
		t.Errorf("branch should still exist after refused delete: %v", err)
	}
	// baseline を持たない別ブランチは普通に削除できる。
	if _, err := m.Create(ctx, "pr-2", 0); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, "pr-2"); err != nil {
		t.Errorf("non-backing branch should delete cleanly: %v", err)
	}
}

// promote 元 dataset 上の baseline を reset/recreate が破壊しないこと(#289)。
// precondition は状態変更や storage 操作より前に判定する。
func TestResetAndRecreateRefuseBranchBackingBaseline(t *testing.T) {
	ctx := context.Background()
	eng := &mockEngine{}
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}}
	m := newTestManager(t, st, eng, "")
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	snap, err := m.PromoteBranch(ctx, "pr-1", PromoteOptions{})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{name: "reset", run: func() error { _, err := m.Reset(ctx, "pr-1"); return err }},
		{name: "recreate", run: func() error { _, err := m.Recreate(ctx, "pr-1"); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); !errors.Is(err, ErrPreconditionFailed) {
				t.Fatalf("%s should refuse baseline-backing branch, got %v", tc.name, err)
			}
		})
	}
	if len(st.rollbacks) != 0 || len(st.renamed) != 0 {
		t.Fatalf("refused operations mutated storage: rollbacks=%v renamed=%v", st.rollbacks, st.renamed)
	}
	b, err := m.db.GetBranch("pr-1")
	if err != nil || b.State != state.StateRunning {
		t.Fatalf("branch changed after refused operations: branch=%+v err=%v", b, err)
	}
	info, err := m.Get(ctx, "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(info.BackingBaselines) != 1 || info.BackingBaselines[0] != snap {
		t.Fatalf("backing baselines=%v, want [%s]", info.BackingBaselines, snap)
	}
}

// promote が途中で失敗しても branch は使用可能な状態に戻す(#156)。以前は
// snapshot 後の baseline 登録が失敗すると mysqld を停止したまま抜けていた。
func TestPromoteBranchRestartsOnFailure(t *testing.T) {
	ctx := context.Background()
	eng := &mockEngine{}
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}, promoteErr: errors.New("boom")}
	m := newTestManager(t, st, eng, "")
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	startsBefore := len(eng.started)

	if _, err := m.PromoteBranch(ctx, "pr-1", PromoteOptions{}); err == nil {
		t.Fatal("PromoteBranch はエラーを返すはず")
	}
	// snapshot のため一度停止し、失敗後も再起動していること。
	if len(eng.stopped) == 0 {
		t.Error("promote は snapshot 前に branch を停止するはず")
	}
	if len(eng.started) <= startsBefore {
		t.Errorf("promote 失敗後も branch を再起動するはず (started=%v)", eng.started)
	}
	// current baseline は切り替わっていない。
	if got := string(m.currentBaseline()); strings.Contains(got, "pr-1@") {
		t.Errorf("promote 失敗時は current を変えないはず (current=%q)", got)
	}
}
