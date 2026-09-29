package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, root, rel string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 同梱プロファイルが有効なまま = 次の再起動で全 mysqld が起動不能になる(#376)。
func TestApparmorStockConflict(t *testing.T) {
	t.Run("sashiki のプロファイルが無ければ対象外", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, apparmorStockProfile)
		if applicable, _ := apparmorStockConflict(root); applicable {
			t.Error("sashiki-mysqld が無いのに対象になった")
		}
	})
	t.Run("同梱プロファイルが無ければ対象外", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, apparmorOwnProfile)
		if applicable, _ := apparmorStockConflict(root); applicable {
			t.Error("同梱プロファイルが無いのに対象になった")
		}
	})
	t.Run("両方あって disable が無ければ競合", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, apparmorOwnProfile)
		writeFile(t, root, apparmorStockProfile)
		applicable, conflict := apparmorStockConflict(root)
		if !applicable || !conflict {
			t.Errorf("applicable=%v conflict=%v, want true true", applicable, conflict)
		}
	})
	t.Run("disable の symlink があれば競合しない", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, apparmorOwnProfile)
		writeFile(t, root, apparmorStockProfile)
		link := filepath.Join(root, apparmorStockDisableLink)
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/"+apparmorStockProfile, link); err != nil {
			t.Fatal(err)
		}
		applicable, conflict := apparmorStockConflict(root)
		if !applicable || conflict {
			t.Errorf("applicable=%v conflict=%v, want true false", applicable, conflict)
		}
	})
}
