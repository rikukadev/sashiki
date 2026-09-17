package state

import (
	"path/filepath"
	"testing"
	"time"
)

// 古い hook_runs は消すが、(branch, event) ごとの最新 1 件は LastHookStatus の
// ために残す(#295)。
func TestPruneHookRunsKeepsLatestPerEvent(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	for i := 0; i < 3; i++ {
		id, err := db.RecordHookStart("pr-1", "on-create")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.RecordHookFinish(id, i); err != nil {
			t.Fatal(err)
		}
	}
	// 全部「今」完了しているので、未来を境にすると最新以外が消える
	n, err := db.PruneHookRuns(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("pruned %d, want 2 (keep the latest)", n)
	}
	hs, err := db.LastHookStatus("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if hs["on-create"] == "" {
		t.Errorf("latest hook status must survive prune, got %v", hs)
	}
}
