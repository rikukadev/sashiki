package ops

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rikukadev/sashiki/internal/oplog"
	"github.com/rikukadev/sashiki/internal/state"
)

// operation の fn に渡る ctx は operation_id / type / branch を持つ(#284)。
func TestOperationContextCarriesCorrelation(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	r := New(db)
	var seen oplog.Operation
	id, err := r.RunSync("create", "pr-1", func(ctx context.Context) error {
		seen, _ = oplog.FromContext(ctx)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen.ID != id || seen.Type != "create" || seen.Branch != "pr-1" {
		t.Errorf("ctx operation = %+v, want id=%s type=create branch=pr-1", seen, id)
	}
	done := make(chan oplog.Operation, 1)
	aid, err := r.Start("delete", "pr-2", func(ctx context.Context) error {
		op, _ := oplog.FromContext(ctx)
		done <- op
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := <-done; got.ID != aid || got.Type != "delete" || got.Branch != "pr-2" {
		t.Errorf("async ctx operation = %+v", got)
	}
	r.Drain(0)
}
