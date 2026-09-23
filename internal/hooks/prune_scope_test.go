package hooks

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pruneLogs は自分が書いた <branch>-<event>-<時刻>.log だけを消す(#327:
// hooks.log_dir を log_dir と同じにすると postgres のログまで消えていた)。
func TestPruneLogsOnlyTouchesHookLogs(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	for _, n := range []string{"pr-1-on-create-20240101T000000Z.log", "pr-1.err.log", "postgres-pr-1.log", "sashikid.out.log"} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, old, old)
	}
	r := NewRunner(t.TempDir(), dir, time.Minute)
	r.LogRetention = 24 * time.Hour
	r.pruneLogs()
	if _, err := os.Stat(filepath.Join(dir, "pr-1-on-create-20240101T000000Z.log")); !os.IsNotExist(err) {
		t.Error("expired hook log should be removed")
	}
	for _, n := range []string{"pr-1.err.log", "postgres-pr-1.log", "sashikid.out.log"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s is not a hook log and must be kept: %v", n, err)
		}
	}
}
