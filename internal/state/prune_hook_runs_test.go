package state

import (
	"path/filepath"
	"testing"
	"time"
)

// 削除済み branch の hook_runs は「(branch, event) の最新行」でも保持しない(#327)。
func TestPruneHookRunsDropsDeletedBranches(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.CreateBranch("alive", 3401, "base"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	for _, b := range []string{"alive", "gone"} {
		id, err := db.RecordHookStart(b, "on-create")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.RecordHookFinish(id, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := db.sql.Exec(`UPDATE hook_runs SET finished_at = ? WHERE id = ?`, old.UTC().Format(timeFmt), id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.PruneHookRuns(time.Now().Add(-24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	var n int
	for _, b := range []string{"alive", "gone"} {
		if err := db.sql.QueryRow(`SELECT COUNT(*) FROM hook_runs WHERE branch = ?`, b).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if want := map[string]int{"alive": 1, "gone": 0}[b]; n != want {
			t.Errorf("hook_runs for %s = %d, want %d", b, n, want)
		}
	}
}
