package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rikukadev/sashiki/internal/config"
)

// renderConfig の出力が config.Load でそのまま読める YAML であること。
func TestRenderConfigIsLoadable(t *testing.T) {
	data, err := renderConfig("mypool")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("generated config is not loadable: %v", err)
	}
	if cfg.Storage.Zfs.Pool != "mypool" {
		t.Errorf("pool = %q, want mypool", cfg.Storage.Zfs.Pool)
	}
	if cfg.Storage.Zfs.BaseDataset != "mypool/base" {
		t.Errorf("base_dataset = %q", cfg.Storage.Zfs.BaseDataset)
	}
	// systemd デプロイは sashikid を User=sashiki で動かし、zfs/systemctl を
	// sudoers 経由で叩くため sudo: true(#176)。
	if !cfg.Storage.Zfs.Sudo || !cfg.Engine.Mysql.Sudo {
		t.Error("generated config should have sudo: true (systemd/User=sashiki path)")
	}
	// per-branch env は /run/sashiki(RuntimeDirectory、#177)。/etc は root 所有。
	if cfg.Engine.Mysql.EnvDir != "/run/sashiki" {
		t.Errorf("env_dir = %q, want /run/sashiki", cfg.Engine.Mysql.EnvDir)
	}
}

// 埋め込みユニットファイルが仕様どおりの内容を持つこと。
func TestEmbeddedUnitFile(t *testing.T) {
	s := string(mysqldUnit)
	for _, want := range []string{
		"EnvironmentFile=/run/sashiki/%i.env",
		"--datadir=${DATADIR}",
		"--bind-address=127.0.0.1",
		"--port=${PORT}",
		"User=mysql",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("unit file missing %q", want)
		}
	}
}

// ステップ構成: device なしでも定義でき、パッケージスキップが効くこと。
func TestInitStepsComposition(t *testing.T) {
	withPkg := initSteps(initOpts{pool: "p"})
	withoutPkg := initSteps(initOpts{pool: "p", skipPackages: true})
	if len(withPkg) != len(withoutPkg)+1 {
		t.Errorf("skipPackages should remove exactly one step: %d vs %d", len(withPkg), len(withoutPkg))
	}
	for _, s := range withPkg {
		if s.name == "" || s.run == nil {
			t.Errorf("step %+v is incomplete", s)
		}
	}
}
