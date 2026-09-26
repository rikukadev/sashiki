package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rikukadev/sashiki/internal/storage"
)

func writeRefreshScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "refresh.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitRefreshDone(t *testing.T) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if !RefreshInProgress() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("refresh did not finish")
}

func quiesceOK(ctx context.Context) error   { return nil }
func quiesceBusy(ctx context.Context) error { return errors.New("mysqld still running") }

// mockStorageWithBase は BasePath を提供する mock(ebszfs 相当)。
// SnapshotBase 時点で auto.cnf が残っていたかを記録し、
// 「削除は snapshot 取得前」の順序を検証できるようにする(#80)。
type mockStorageWithBase struct {
	mockStorage
	base              string
	autoCnfAtSnapshot bool
}

func (m *mockStorageWithBase) BasePath(ctx context.Context) (string, error) {
	return m.base, nil
}

func (m *mockStorageWithBase) SnapshotBase(ctx context.Context, tag string) (storage.SnapshotRef, error) {
	m.autoCnfAtSnapshot = fileExists(filepath.Join(m.base, "data", "auto.cnf"))
	return m.mockStorage.SnapshotBase(ctx, tag)
}

// refresh は snapshot 取得前に base の auto.cnf を削除する(#80)。
func TestRefreshRemovesAutoCnfBeforeSnapshot(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	autoCnf := filepath.Join(dataDir, "auto.cnf")
	if err := os.WriteFile(autoCnf, []byte("[auto]\nserver-uuid=deadbeef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := &mockStorageWithBase{base: base}
	m := newTestManager(t, st, &mockEngine{}, "")
	script := writeRefreshScript(t, "exit 0")
	if _, err := m.RefreshBaseline(context.Background(), RefreshConfig{
		Script: script, CheckQuiesced: quiesceOK, SkipValidate: true,
	}); err != nil {
		t.Fatal(err)
	}
	waitRefreshDone(t)
	if RefreshLastError() != "" {
		t.Fatalf("refresh error: %s", RefreshLastError())
	}
	if _, err := os.Stat(autoCnf); !os.IsNotExist(err) {
		t.Error("auto.cnf should be removed")
	}
	if st.autoCnfAtSnapshot {
		t.Error("auto.cnf must be removed before SnapshotBase (snapshot 取得前)")
	}
}

// auto.cnf が最初から無くても refresh は成功する(削除はベストエフォートでなく
// 冪等: 不存在はエラーにしない)。
func TestRefreshSucceedsWhenAutoCnfAbsent(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := &mockStorageWithBase{base: base}
	m := newTestManager(t, st, &mockEngine{}, "")
	script := writeRefreshScript(t, "exit 0")
	if _, err := m.RefreshBaseline(context.Background(), RefreshConfig{
		Script: script, CheckQuiesced: quiesceOK, SkipValidate: true,
	}); err != nil {
		t.Fatal(err)
	}
	waitRefreshDone(t)
	if RefreshLastError() != "" {
		t.Fatalf("refresh error: %s", RefreshLastError())
	}
}

func TestRefreshSuccessRotatesBaseline(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	script := writeRefreshScript(t, "exit 0")
	tag, err := m.RefreshBaseline(context.Background(), RefreshConfig{
		Script: script, CheckQuiesced: quiesceOK, SkipValidate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitRefreshDone(t)
	if RefreshLastError() != "" {
		t.Fatalf("refresh error: %s", RefreshLastError())
	}
	// current が新 snapshot(pool/base@tag)に切り替わっている
	info, err := m.Baseline(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := "pool/base@" + tag
	if info.Current != want {
		t.Errorf("current = %q, want %q", info.Current, want)
	}
	// 以降の Create は新 baseline を origin にする
	created, err := m.Create(context.Background(), "pr-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if created.OriginSnapshot != want {
		t.Errorf("origin = %q, want %q", created.OriginSnapshot, want)
	}
}

func TestRefreshScriptFailureDoesNotSnapshot(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	before, _ := m.Baseline(context.Background())
	script := writeRefreshScript(t, "echo boom >&2; exit 3")
	if _, err := m.RefreshBaseline(context.Background(), RefreshConfig{
		Script: script, CheckQuiesced: quiesceOK, SkipValidate: true,
	}); err != nil {
		t.Fatal(err)
	}
	waitRefreshDone(t)
	if RefreshLastError() == "" {
		t.Error("last error should be recorded")
	}
	after, _ := m.Baseline(context.Background())
	if after.Current != before.Current {
		t.Errorf("baseline should not rotate on failure: %q -> %q", before.Current, after.Current)
	}
}

func TestRefreshAbortsWhenNotQuiesced(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	before, _ := m.Baseline(context.Background())
	// スクリプトは exit 0 だが quiesce 検証が失敗し続ける → snapshot 取得しない
	script := writeRefreshScript(t, "exit 0")
	if _, err := m.RefreshBaseline(context.Background(), RefreshConfig{
		Script: script, CheckQuiesced: quiesceBusy, SkipValidate: true,
	}); err != nil {
		t.Fatal(err)
	}
	waitRefreshDone(t)
	if RefreshLastError() == "" {
		t.Error("quiesce failure should be recorded")
	}
	after, _ := m.Baseline(context.Background())
	if after.Current != before.Current {
		t.Error("baseline must not rotate when base is not quiesced")
	}
}

func TestRefreshRejectsConcurrentRun(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	script := writeRefreshScript(t, "sleep 0.5")
	if _, err := m.RefreshBaseline(context.Background(), RefreshConfig{
		Script: script, CheckQuiesced: quiesceOK, SkipValidate: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RefreshBaseline(context.Background(), RefreshConfig{
		Script: script, CheckQuiesced: quiesceOK, SkipValidate: true,
	}); !errors.Is(err, ErrRefreshRunning) {
		t.Errorf("err = %v, want ErrRefreshRunning", err)
	}
	waitRefreshDone(t)
}

func TestRefreshValidatesCandidateBeforePublish(t *testing.T) {
	st := &mockStorage{}
	eng := &mockEngine{}
	m := newTestManager(t, st, eng, "")
	script := writeRefreshScript(t, "exit 0")
	// validate 有効(mock なので clone+engine は成功)
	tag, err := m.RefreshBaseline(context.Background(), RefreshConfig{
		Script: script, CheckQuiesced: quiesceOK,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitRefreshDone(t)
	if RefreshLastError() != "" {
		t.Fatalf("refresh error: %s", RefreshLastError())
	}
	// validate 用の一時 clone が作られ、破棄された
	found := false
	for _, c := range st.cloned {
		if c == "_validate" {
			found = true
		}
	}
	if !found {
		t.Error("validate should clone a temp _validate branch")
	}
	// baseline が validated=true で登録され current になっている
	b, err := m.db.GetBaseline("pool/base@" + tag)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Prov.Validated {
		t.Error("baseline should be marked validated after validation")
	}
}

func TestRefreshRejectsUnvalidatedPublishWhenRequired(t *testing.T) {
	st := &mockStorage{}
	// engine 起動が失敗 → validate 失敗
	m := newTestManager(t, st, &mockEngine{startErr: errors.New("start fail")}, "")
	before, _ := m.Baseline(context.Background())
	script := writeRefreshScript(t, "exit 0")
	if _, err := m.RefreshBaseline(context.Background(), RefreshConfig{
		Script: script, CheckQuiesced: quiesceOK, RequireValidated: true,
	}); err != nil {
		t.Fatal(err)
	}
	waitRefreshDone(t)
	if RefreshLastError() == "" {
		t.Error("validate failure should be recorded")
	}
	after, _ := m.Baseline(context.Background())
	if after.Current != before.Current {
		t.Error("unvalidated baseline must not be published")
	}
}

func TestRefreshRejectsUnmaskedWhenRequired(t *testing.T) {
	st := &mockStorage{}
	m := newTestManager(t, st, &mockEngine{}, "")
	before, _ := m.Baseline(context.Background())
	script := writeRefreshScript(t, "exit 0")
	// masked sentinel を指定しない(=masked false)+ require_masked
	if _, err := m.RefreshBaseline(context.Background(), RefreshConfig{
		Script: script, CheckQuiesced: quiesceOK, RequireMasked: true,
		MaskedSentinel: filepath.Join(t.TempDir(), "nonexistent"),
	}); err != nil {
		t.Fatal(err)
	}
	waitRefreshDone(t)
	if RefreshLastError() == "" {
		t.Error("unmasked publish should be rejected")
	}
	after, _ := m.Baseline(context.Background())
	if after.Current != before.Current {
		t.Error("unmasked baseline must not be published")
	}
}

// script が無く SourceDir がある場合は組み込みローダー経路(#101)。
func TestRefreshUsesSourceLoaderWhenScriptAbsent(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := &mockStorageWithBase{base: base}
	m := newTestManager(t, st, &mockEngine{}, "")
	called := false
	if _, err := m.RefreshBaseline(context.Background(), RefreshConfig{
		Script:        filepath.Join(t.TempDir(), "no-such-script.sh"),
		SourceDir:     t.TempDir(),
		RunSource:     func(ctx context.Context) error { called = true; return nil },
		CheckQuiesced: quiesceOK, SkipValidate: true,
	}); err != nil {
		t.Fatal(err)
	}
	waitRefreshDone(t)
	if RefreshLastError() != "" {
		t.Fatalf("refresh error: %s", RefreshLastError())
	}
	if !called {
		t.Error("source loader should be invoked when script is absent")
	}
}

// script が存在する場合は SourceDir があっても従来どおり script を優先する。
func TestRefreshPrefersScriptOverSourceDir(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := &mockStorageWithBase{base: base}
	m := newTestManager(t, st, &mockEngine{}, "")
	marker := filepath.Join(t.TempDir(), "ran")
	script := writeRefreshScript(t, "touch "+marker)
	if _, err := m.RefreshBaseline(context.Background(), RefreshConfig{
		Script:        script,
		SourceDir:     t.TempDir(),
		CheckQuiesced: quiesceOK, SkipValidate: true,
	}); err != nil {
		t.Fatal(err)
	}
	waitRefreshDone(t)
	if RefreshLastError() != "" {
		t.Fatalf("refresh error: %s", RefreshLastError())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("script should run when it exists (script takes precedence)")
	}
}

// script も SourceDir も無ければ従来どおりエラー。
func TestRefreshFailsWithoutScriptAndSourceDir(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	if _, err := m.RefreshBaseline(context.Background(), RefreshConfig{
		Script: filepath.Join(t.TempDir(), "missing.sh"),
	}); err == nil {
		t.Fatal("missing script without source_dir should fail")
	}
}

// 前回の build で残った masked sentinel を、今回マスクしていないのに
// masked 扱いしてはいけない(build 実行前に消す。#58 security invariant)。
func TestRefreshDoesNotTrustStaleSentinel(t *testing.T) {
	st := &mockStorage{}
	m := newTestManager(t, st, &mockEngine{}, "")
	before, _ := m.Baseline(context.Background())

	// 前回の残留を模して、sentinel を事前に作っておく
	sentinel := filepath.Join(t.TempDir(), "baseline-masked")
	if err := os.WriteFile(sentinel, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 今回の build script は sentinel を作らない(= マスクしていない)
	script := writeRefreshScript(t, "exit 0")
	if _, err := m.RefreshBaseline(context.Background(), RefreshConfig{
		Script: script, CheckQuiesced: quiesceOK, RequireMasked: true,
		MaskedSentinel: sentinel,
	}); err != nil {
		t.Fatal(err)
	}
	waitRefreshDone(t)
	if RefreshLastError() == "" {
		t.Error("stale sentinel must not be trusted; unmasked publish should be rejected")
	}
	after, _ := m.Baseline(context.Background())
	if after.Current != before.Current {
		t.Error("baseline must not be published based on a stale sentinel")
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Error("build should remove the sentinel before running")
	}
}

// refresh_script も source_dir も無いときは、選択肢を示して落ちる(#353)。
func TestRefreshWithoutScriptOrSourceDirExplainsOptions(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	_, err := m.RefreshBaseline(context.Background(), RefreshConfig{
		Script: filepath.Join(t.TempDir(), "missing.sh"), CheckQuiesced: quiesceOK, SkipValidate: true,
	})
	if err == nil {
		t.Fatal("refresh without script / source_dir should fail")
	}
	for _, want := range []string{"source_dir", "refresh_script", "--app-user-only", "baseline import"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("should be a precondition error (412), got %v", err)
	}
}

// --app-user-only は script が無くてもローダー経路で動く(#355)。
func TestRefreshAppUserOnlySkipsScript(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	called := false
	if _, err := m.RefreshBaseline(context.Background(), RefreshConfig{
		Script: filepath.Join(t.TempDir(), "missing.sh"), CheckQuiesced: quiesceOK, SkipValidate: true,
		AppUserOnly: true,
		RunSource:   func(context.Context) error { called = true; return nil },
	}); err != nil {
		t.Fatal(err)
	}
	waitRefreshDone(t)
	if !called {
		t.Error("app-user-only refresh should run the loader path")
	}
}
