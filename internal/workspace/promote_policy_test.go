package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rikukadev/sashiki/internal/storage"
)

// promote は refresh と同じ publish ポリシーを通る(#296)。以前は無条件に
// validated=true / masked=false で登録して即 current にしていたので、
// require_masked / require_validated の抜け道になっていた。

// require_masked: --masked の宣言が無ければ、branch を止める前に拒否する。
func TestPromoteRefusesUnmaskedWhenRequired(t *testing.T) {
	ctx := context.Background()
	eng := &mockEngine{}
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}}
	m := newTestManager(t, st, eng, "")
	m.SetBaselinePolicy(RefreshConfig{RequireMasked: true})
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	before, _ := m.Baseline(ctx)
	stopped := len(eng.stopped)

	_, err := m.PromoteBranch(ctx, "pr-1", PromoteOptions{})
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("unmasked promote should be refused under require_masked, got %v", err)
	}
	if len(eng.stopped) != stopped {
		t.Error("refusal must happen before the branch is stopped")
	}
	if after, _ := m.Baseline(ctx); after.Current != before.Current {
		t.Error("current baseline must not move on refused promote")
	}

	// --masked を宣言すれば通り、masked=true で登録される。
	snap, err := m.PromoteBranch(ctx, "pr-1", PromoteOptions{Masked: true})
	if err != nil {
		t.Fatalf("masked promote: %v", err)
	}
	row, err := m.db.GetBaseline(snap)
	if err != nil {
		t.Fatal(err)
	}
	if !row.Prov.Masked || !row.Prov.Validated {
		t.Errorf("promoted baseline should be masked+validated, got %+v", row.Prov)
	}
	if got := string(m.currentBaseline()); got != snap {
		t.Errorf("current = %q, want %q", got, snap)
	}
}

// 検証(on-baseline-validate)に落ちたら、登録は残すが current にはしない。
func TestPromoteDoesNotPublishWhenValidationFails(t *testing.T) {
	ctx := context.Background()
	hooksDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hooksDir, "on-baseline-validate"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	eng := &mockEngine{}
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}}
	m := newTestManager(t, st, eng, hooksDir)
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	before, _ := m.Baseline(ctx)

	_, err := m.PromoteBranch(ctx, "pr-1", PromoteOptions{})
	if err == nil {
		t.Fatal("promote should fail when the validate hook fails")
	}
	if after, _ := m.Baseline(ctx); after.Current != before.Current {
		t.Error("failed validation must not publish")
	}
	// 登録は残っていて validated=false。
	rows, _ := m.db.ListBaselines()
	found := false
	for _, r := range rows {
		if filepath.Base(r.Snapshot) != "" && r.Prov.Validated == false && r.Snapshot != before.Current {
			found = true
		}
	}
	if !found {
		t.Errorf("candidate should stay registered with validated=false: %+v", rows)
	}
	// branch は使用可能な状態に戻っている(snapshot 後に再起動している)。
	if b, _ := m.db.GetBranch("pr-1"); b.State != "running" {
		t.Errorf("branch should be running after promote, got %s", b.State)
	}
}

// require_validated では --skip-validate を受け付けない。
func TestPromoteRefusesSkipValidateWhenRequired(t *testing.T) {
	ctx := context.Background()
	eng := &mockEngine{}
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}}
	m := newTestManager(t, st, eng, "")
	m.SetBaselinePolicy(RefreshConfig{RequireValidated: true})
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PromoteBranch(ctx, "pr-1", PromoteOptions{SkipValidate: true}); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("skip-validate under require_validated should be refused, got %v", err)
	}
	// 通常経路は検証を通って validated=true になる。
	snap, err := m.PromoteBranch(ctx, "pr-1", PromoteOptions{})
	if err != nil {
		t.Fatalf("promote with validation: %v", err)
	}
	if row, _ := m.db.GetBaseline(snap); !row.Prov.Validated {
		t.Error("promoted baseline should be validated")
	}
}
