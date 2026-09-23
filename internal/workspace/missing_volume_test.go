package workspace

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rikukadev/sashiki/internal/state"
)

// volume が無い error ブランチ(clone 失敗 / dataset_missing)は、API の事前検査でも
// reaper でも削除できる(#319)。以前は CheckBranchMutation が resolveVolume の失敗を
// 500 にし、reaper も「保護」に倒していたので、行が max_branches と port を
// 永久に占有していた。
func TestMissingVolumeBranchCanBeDeleted(t *testing.T) {
	ctx := context.Background()
	st := &mockStorage{missing: map[string]bool{}}
	m := newTestManager(t, st, &mockEngine{}, "")
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	st.missing["pr-1"] = true
	_ = m.db.SetError("pr-1", "reconcile", "dataset_missing", false, "dataset not found", nil)

	// API の事前検査は「promote 元ではない」として通す(500 にしない)
	if err := m.CheckBranchMutation(ctx, "pr-1", "削除"); err != nil {
		t.Fatalf("CheckBranchMutation on a volume-less branch must pass, got %v", err)
	}
	// Delete は行だけ片付ける
	if err := m.Delete(ctx, "pr-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := m.db.GetBranch("pr-1"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("row should be gone, got %v", err)
	}
}

func TestReaperDeletesMissingVolumeErrorBranch(t *testing.T) {
	ctx := context.Background()
	st := &mockStorage{missing: map[string]bool{}}
	m := newTestManagerCfg(t, st, &mockEngine{}, "", func(c *Config) { c.ErrorRetention = time.Hour })
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	st.missing["pr-1"] = true
	_ = m.db.SetError("pr-1", "create", CodeCloneFailed, true, "clone: no space", nil)
	_ = m.db.SetErrorAtForTest("pr-1", time.Now().Add(-2*time.Hour))
	if err := m.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.GetBranch("pr-1"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("error_retention should delete a volume-less error branch, got %v", err)
	}
}

func TestReaperDeletesMissingVolumeBranchOnLeaseExpiry(t *testing.T) {
	ctx := context.Background()
	st := &mockStorage{missing: map[string]bool{}}
	m := newTestManager(t, st, &mockEngine{}, "")
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	st.missing["pr-1"] = true
	_ = m.db.SetError("pr-1", "create", CodeCloneFailed, true, "clone: no space", nil)
	_ = m.db.SetExpiresAt("pr-1", time.Now().Add(-time.Minute))
	if err := m.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.GetBranch("pr-1"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("expired lease should delete a volume-less branch, got %v", err)
	}
}
