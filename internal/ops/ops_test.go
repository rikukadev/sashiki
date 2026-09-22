package ops

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/rikukadev/sashiki/internal/state"
)

func newStore(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestRunSyncSuccess(t *testing.T) {
	r := New(newStore(t))
	id, err := r.RunSync("create", "pr-1", func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	op, err := r.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != state.OpCompleted || op.Type != "create" || op.Target != "pr-1" {
		t.Errorf("op = %+v", op)
	}
	if op.FinishedAt == nil {
		t.Error("finished_at should be set")
	}
}

func TestRunSyncFailureRecordsError(t *testing.T) {
	r := New(newStore(t))
	id, err := r.RunSync("delete", "pr-1", func(context.Context) error { return errors.New("boom") })
	if err == nil {
		t.Fatal("want error")
	}
	op, _ := r.Get(id)
	if op.State != state.OpFailed || op.Error != "boom" {
		t.Errorf("op = %+v", op)
	}
}

func TestStartIsAsyncAndWaitObservesCompletion(t *testing.T) {
	r := New(newStore(t))
	release := make(chan struct{})
	id, err := r.Start("reset", "pr-1", func(context.Context) error {
		<-release
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// まだ running
	op, _ := r.Get(id)
	if op.State != state.OpRunning {
		t.Errorf("state = %s, want running", op.State)
	}
	close(release)
	op, err = r.Wait(context.Background(), id, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != state.OpCompleted {
		t.Errorf("state = %s, want completed", op.State)
	}
}

func TestListNewestFirst(t *testing.T) {
	r := New(newStore(t))
	_, _ = r.RunSync("create", "pr-1", func(context.Context) error { return nil })
	time.Sleep(2 * time.Millisecond)
	_, _ = r.RunSync("delete", "pr-1", func(context.Context) error { return nil })
	list, err := r.List(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Type != "delete" {
		t.Errorf("list = %+v (newest should be first)", list)
	}
}

// codedError は Code() を持つエラー(workspace.stageErr 相当)。
type codedError struct {
	code string
	err  error
}

func (e codedError) Error() string { return e.err.Error() }
func (e codedError) Unwrap() error { return e.err }
func (e codedError) Code() string  { return e.code }

func TestRunSyncFailureRecordsErrorCode(t *testing.T) {
	r := New(newStore(t))
	id, _ := r.RunSync("create", "pr-1", func(context.Context) error {
		return codedError{code: "clone", err: errors.New("clone failed: boom")}
	})
	op, _ := r.Get(id)
	if op.State != state.OpFailed || op.Error != "clone failed: boom" || op.ErrorCode != "clone" {
		t.Errorf("op = %+v (error_code should be wired from Code())", op)
	}
}

// wrap されていても error_code を拾えること。
func TestRunSyncFailureErrorCodeUnwrapped(t *testing.T) {
	r := New(newStore(t))
	inner := codedError{code: "engine-start", err: errors.New("start fail")}
	id, _ := r.RunSync("reset", "pr-1", func(context.Context) error {
		return fmt.Errorf("reset: %w", inner)
	})
	op, _ := r.Get(id)
	if op.ErrorCode != "engine-start" {
		t.Errorf("error_code = %q, want engine-start (should unwrap)", op.ErrorCode)
	}
}

var errSentinel = errors.New("sentinel")

func TestRunSyncPreservesErrorType(t *testing.T) {
	r := New(newStore(t))
	_, err := r.RunSync("create", "pr-1", func(context.Context) error { return errSentinel })
	if !errors.Is(err, errSentinel) {
		t.Errorf("RunSync should return the original error unwrapped: %v", err)
	}
}

// 同じ対象の排他 operation は実行中なら ErrInProgress。終われば次を受け付ける(#303)。
func TestStartExclusiveRejectsConcurrentSameTarget(t *testing.T) {
	r := New(newStore(t))
	release := make(chan struct{})
	id, err := r.StartExclusive("reset", "pr-1", func(context.Context) error { <-release; return nil })
	if err != nil {
		t.Fatal(err)
	}
	busy, err := r.StartExclusive("delete", "pr-1", func(context.Context) error { return nil })
	if !errors.Is(err, ErrInProgress) || busy != id {
		t.Fatalf("second op on same target: id=%q err=%v, want ErrInProgress with %q", busy, err, id)
	}
	if _, err := r.StartExclusive("reset", "pr-2", func(context.Context) error { return nil }); err != nil {
		t.Errorf("other target must not be blocked: %v", err)
	}
	close(release)
	if _, err := r.Wait(context.Background(), id, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	// Wait が completed を返した直後(Drain を挟まない)でも次の操作を受け付ける。
	// CLI の --wait → 次のコマンド、の間に 409 が挟まらないこと。
	if _, err := r.StartExclusive("delete", "pr-1", func(context.Context) error { return nil }); err != nil {
		t.Errorf("right after completion the target should accept a new op: %v", err)
	}
}

// Drain は実行中の operation を待ち、timeout で諦める。
func TestDrainWaitsForRunningOps(t *testing.T) {
	r := New(newStore(t))
	release := make(chan struct{})
	if _, err := r.Start("create", "pr-1", func(context.Context) error { <-release; return nil }); err != nil {
		t.Fatal(err)
	}
	if r.Drain(20 * time.Millisecond) {
		t.Error("drain must not report done while an op is running")
	}
	close(release)
	if !r.Drain(time.Second) {
		t.Error("drain should finish after the op completes")
	}
}
