package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rikukadev/sashiki/internal/state"
	"github.com/rikukadev/sashiki/internal/storage"
)

// reset の失敗は failed_operation=reset として残り、retry でやり直せる(#292)。
// 以前は state=error だけで failed_operation が空になり、retry が
// `cannot retry operation ""` で拒否されていた。
func TestRetryAfterResetFailure(t *testing.T) {
	ctx := context.Background()
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}}
	m := newTestManager(t, st, &mockEngine{}, "")
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	st.rollbackErr = errors.New("dataset is busy")
	if _, err := m.Reset(ctx, "pr-1"); err == nil {
		t.Fatal("reset should fail while rollback fails")
	}
	b, _ := m.db.GetBranch("pr-1")
	if b.State != state.StateError || b.FailedOp != "reset" || !b.Recoverable {
		t.Fatalf("after failed reset: state=%s failed_op=%q recoverable=%v", b.State, b.FailedOp, b.Recoverable)
	}
	st.rollbackErr = nil
	info, err := m.Retry(ctx, "pr-1")
	if err != nil {
		t.Fatalf("retry after reset failure: %v", err)
	}
	if info.State != state.StateRunning {
		t.Errorf("state = %s, want running", info.State)
	}
}

// recreate が退避(rename)後に失敗しても retry で通る。前回の退避名
// (<name>-recreating)が残っていれば片付けてから進む(#292)。
func TestRetryAfterRecreateFailureCleansStaleSwap(t *testing.T) {
	ctx := context.Background()
	eng := &mockEngine{}
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}} // ClonesAreDistinct=false → rename swap
	m := newTestManager(t, st, eng, "")
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	eng.startErr = errors.New("mysqld failed to start")
	if _, err := m.Recreate(ctx, "pr-1"); err == nil {
		t.Fatal("recreate should fail while the engine cannot start")
	}
	b, _ := m.db.GetBranch("pr-1")
	if b.State != state.StateError || b.FailedOp != "recreate" || !b.Recoverable {
		t.Fatalf("after failed recreate: state=%s failed_op=%q recoverable=%v", b.State, b.FailedOp, b.Recoverable)
	}
	// 退避は起きている(旧 → pr-1-recreating)
	if len(st.renamed) == 0 || !strings.HasSuffix(st.renamed[0], "->pr-1"+RecreatingSuffix) {
		t.Fatalf("expected stash rename, got %v", st.renamed)
	}

	eng.startErr = nil
	destroyedBefore := len(st.destroyed)
	info, err := m.Retry(ctx, "pr-1")
	if err != nil {
		t.Fatalf("retry after recreate failure: %v", err)
	}
	if info.State != state.StateRunning {
		t.Errorf("state = %s, want running", info.State)
	}
	// 残っていた退避名を先に消してから再度退避している
	cleaned := false
	for _, d := range st.destroyed[destroyedBefore:] {
		if strings.HasSuffix(d, "pr-1"+RecreatingSuffix) {
			cleaned = true
		}
	}
	if !cleaned {
		t.Errorf("stale %s volume should be removed before retrying, destroyed=%v", RecreatingSuffix, st.destroyed)
	}
}

// 退避名の接尾辞は利用者の branch 名として使えず、orphan 扱いもしない。
func TestRecreatingSuffixIsReserved(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	if _, err := m.Create(context.Background(), "pr-1"+RecreatingSuffix, 0); !errors.Is(err, ErrInvalidName) {
		t.Errorf("branch name with %s must be rejected, got %v", RecreatingSuffix, err)
	}
	if !IsReserved("pr-1" + RecreatingSuffix) {
		t.Error("IsReserved should cover the recreating suffix")
	}
}
