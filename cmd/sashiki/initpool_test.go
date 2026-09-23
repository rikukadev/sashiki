package main

import (
	"os"
	"path/filepath"
	"testing"
)

// --pool 省略時は既存 config の pool を使う(#320)。アップグレード前の config
// (strict では読めない未知キーを含む)でも pool だけは拾える。
func TestExistingPoolFromConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if got := existingPoolFromConfig(write("a.yaml", "storage:\n  backend: ebs-zfs\n  ebs-zfs:\n    pool: tank\n")); got != "tank" {
		t.Errorf("ebs-zfs.pool = %q, want tank", got)
	}
	if got := existingPoolFromConfig(write("b.yaml", "storage:\n  zfs:\n    pool: old\n  unknown_key: 1\nbaselines:\n  keep_last: 3\n")); got != "old" {
		t.Errorf("legacy zfs.pool with unknown keys = %q, want old", got)
	}
	if got := existingPoolFromConfig(filepath.Join(dir, "missing.yaml")); got != "" {
		t.Errorf("missing config = %q, want empty", got)
	}
}

// AppArmor / sudoers より前に zpool を確認する(#320)。pool 名を間違えたとき、
// プロファイルを書き換える前に止まる。
func TestInitStepsCheckPoolBeforeHardening(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []initStep
	}{
		{"mysql", initSteps(initOpts{pool: "p", skipPackages: true})},
		{"postgres", initStepsPostgres(initOpts{pool: "p", skipPackages: true})},
	} {
		zpool, hardening := -1, -1
		for i, st := range tc.steps {
			switch {
			case st.name == "zpool p" && zpool < 0:
				zpool = i
			case (st.name[:8] == "AppArmor" || st.name[:7] == "sudoers") && hardening < 0:
				hardening = i
			}
		}
		if zpool < 0 || hardening < 0 || zpool > hardening {
			t.Errorf("%s: zpool step (%d) must come before AppArmor/sudoers (%d)", tc.name, zpool, hardening)
		}
	}
}
