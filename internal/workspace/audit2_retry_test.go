package workspace

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/rikukadev/sashiki/internal/state"
	"github.com/rikukadev/sashiki/internal/storage"
)

// 退避名 <name>-recreating が残ったまま再起動した resetting は recreate の中断(#322)。
// reset として retry すると新 clone に @init が無く rollback が失敗し続ける。
func TestReconcileDetectsInterruptedRecreate(t *testing.T) {
	ctx := context.Background()
	st := &mockStorage{volumes: []string{"pr-1", "pr-1" + RecreatingSuffix}}
	m := newTestManager(t, st, &mockEngine{}, "")
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	_ = m.db.SetState("pr-1", state.StateResetting, "")
	if _, err := m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ := m.db.GetBranch("pr-1")
	if b.State != state.StateError || b.FailedOp != "recreate" || !b.Recoverable {
		t.Errorf("state=%s op=%q recoverable=%v, want error/recreate/true", b.State, b.FailedOp, b.Recoverable)
	}
}

// 元ブランチがやり直し待ちでない <name>-recreating は残骸として orphan に出し、
// gc --orphans で消せる(#322: 予約名として永久にリークしていた)。
func TestStaleRecreatingStashIsOrphan(t *testing.T) {
	ctx := context.Background()
	st := &mockStorage{volumes: []string{"pr-1", "pr-1" + RecreatingSuffix, "gone" + RecreatingSuffix}}
	m := newTestManager(t, st, &mockEngine{}, "")
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	rep, err := m.inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pr-1" + RecreatingSuffix, "gone" + RecreatingSuffix} {
		if !slices.Contains(rep.Orphans, want) {
			t.Errorf("%s should be reported as orphan, got %v", want, rep.Orphans)
		}
	}
	// 元ブランチが resetting / error(やり直し待ち)なら残骸ではない。
	_ = m.db.SetState("pr-1", state.StateResetting, "")
	rep, _ = m.inspect(ctx)
	if slices.Contains(rep.Orphans, "pr-1"+RecreatingSuffix) {
		t.Error("stash of a resetting branch must not be treated as orphan")
	}
	_ = m.db.SetState("pr-1", state.StateRunning, "")
	deleted, err := m.GCOrphans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(deleted, "pr-1"+RecreatingSuffix) {
		t.Errorf("gc --orphans should remove the stale stash, deleted=%v", deleted)
	}
}

// retry の create 経路も promote-guard を通る(#322)。
func TestRetryCreateRefusesPromoteSource(t *testing.T) {
	ctx := context.Background()
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}}
	m := newTestManager(t, st, &mockEngine{}, "")
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PromoteBranch(ctx, "pr-1", PromoteOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = m.db.SetError("pr-1", "create", CodeHookFailed, true, "hook failed", nil)
	renamed := len(st.renamed)
	if _, err := m.Retry(ctx, "pr-1"); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("retry of a promote source must be refused, got %v", err)
	}
	if len(st.renamed) != renamed {
		t.Error("refusal must happen before the dataset is renamed away")
	}
}

// error / 遷移中のブランチは promote できない(#322)。
func TestPromoteRefusesNonQuiescentStates(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, &mockStorage{caps: storage.Capabilities{FastRollback: true}}, &mockEngine{}, "")
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{state.StateError, state.StateResetting, state.StateDeleting, state.StateCreating} {
		_ = m.db.SetState("pr-1", s, "")
		if _, err := m.PromoteBranch(ctx, "pr-1", PromoteOptions{}); !errors.Is(err, ErrPreconditionFailed) {
			t.Errorf("promote in state %s should be refused, got %v", s, err)
		}
	}
}

// wake の失敗は failed_operation=wake で残り、retry できる(#322: README の
// 「wake も retry」が到達不能だった)。
func TestWakeFailureIsRetryable(t *testing.T) {
	ctx := context.Background()
	eng := &mockEngine{}
	m := newTestManager(t, &mockStorage{}, eng, "")
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if err := m.Sleep(ctx, "pr-1"); err != nil {
		t.Fatal(err)
	}
	eng.startErr = errors.New("mysqld failed to start")
	if _, err := m.Wake(ctx, "pr-1"); err == nil {
		t.Fatal("wake should fail")
	}
	b, _ := m.db.GetBranch("pr-1")
	if b.State != state.StateError || b.FailedOp != "wake" || !b.Recoverable {
		t.Fatalf("state=%s op=%q recoverable=%v, want error/wake/true", b.State, b.FailedOp, b.Recoverable)
	}
	eng.startErr = nil
	if _, err := m.Retry(ctx, "pr-1"); err != nil {
		t.Fatalf("retry after wake failure: %v", err)
	}
	if b, _ := m.db.GetBranch("pr-1"); b.State != state.StateRunning {
		t.Errorf("state = %s, want running", b.State)
	}
}

// refresh_script には API トークンを渡さない(#321)。
func TestRefreshScriptDoesNotSeeAPIToken(t *testing.T) {
	t.Setenv("SASHIKI_API_TOKEN", "sashiki_secret")
	st := &mockStorage{}
	m := newTestManager(t, st, &mockEngine{}, "")
	marker := t.TempDir() + "/env.txt"
	script := writeRefreshScript(t, "printenv > "+marker+" || true\nexit 0")
	if _, err := m.RefreshBaseline(context.Background(), RefreshConfig{Script: script, CheckQuiesced: quiesceOK}); err != nil {
		t.Fatal(err)
	}
	waitRefreshDone(t)
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	env := string(b)
	if contains(env, "SASHIKI_API_TOKEN=") {
		t.Error("refresh script must not see SASHIKI_API_TOKEN")
	}
	if !contains(env, "SASHIKI_BASELINE_TAG=") {
		t.Error("refresh script should still get SASHIKI_BASELINE_TAG")
	}
}

func contains(s, sub string) bool { return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Wake は idle の起点(last_conn_at)を進める。running へ遷移してから engine が
// 上がるまでの間に reaper が「running なのに idle」と判定して止めないため。
func TestWakeRefreshesIdleClock(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if err := m.Sleep(ctx, "pr-1"); err != nil {
		t.Fatal(err)
	}
	if b, _ := m.db.GetBranch("pr-1"); b.LastConnAt != nil {
		t.Fatalf("precondition: last_conn_at should be unset, got %v", b.LastConnAt)
	}
	before := time.Now()
	if _, err := m.Wake(ctx, "pr-1"); err != nil {
		t.Fatal(err)
	}
	b, _ := m.db.GetBranch("pr-1")
	if b.LastConnAt == nil || b.LastConnAt.Before(before.Add(-time.Second)) {
		t.Errorf("wake should refresh last_conn_at, got %v", b.LastConnAt)
	}
}
