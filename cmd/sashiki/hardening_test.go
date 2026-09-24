package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rikukadev/sashiki/internal/roothelper"
)

// sudoers 生成(root-helper 方式、#276): helper 1 行だけで、zfs / systemctl を直接許可しない。
func TestSudoersContentIsHelperOnly(t *testing.T) {
	got := sudoersContent("tpool")
	if !strings.Contains(got, "sashiki ALL=(root) NOPASSWD: /usr/local/bin/sashiki-root-helper *") {
		t.Errorf("sudoers should allow only the root helper:\n%s", got)
	}
	for _, deny := range []string{"/usr/sbin/zfs", "/usr/sbin/zpool", "/usr/bin/systemctl", "NOPASSWD: ALL"} {
		if strings.Contains(got, deny) {
			t.Errorf("helper-mode sudoers must not contain %q", deny)
		}
	}
	if n := strings.Count(got, "NOPASSWD"); n != 1 {
		t.Errorf("exactly one NOPASSWD line expected, got %d", n)
	}
	if sudoersFor("tpool", true) != got || sudoersFor("tpool", false) != sudoersLegacyContent("tpool") {
		t.Error("sudoersFor should select helper / legacy content")
	}
}

// root-helper.yaml は helper がそのまま読めて、pool 配下の dataset を指す。
func TestRootHelperConfigContentLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "root-helper.yaml")
	if err := os.WriteFile(path, []byte(rootHelperConfigContent("tpool")), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := roothelper.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool != "tpool" || c.BranchParent != "tpool/branches" || c.BaseDataset != "tpool/base" || len(c.Units) != 2 {
		t.Errorf("unexpected helper config: %+v", c)
	}
	if err := c.Validate([]string{"zfs", "clone", "tpool/base@baseline", "tpool/branches/pr-1"}); err != nil {
		t.Errorf("generated config should allow the real clone form: %v", err)
	}
}

// 旧方式 sudoers 生成: 必要な許可行が揃っていること(#78)。root_helper の無い既存 config 向け。
func TestSudoersLegacyContentRequiredLines(t *testing.T) {
	got := sudoersLegacyContent("tpool")
	for _, want := range []string{
		// zfs: 実呼び出し形(internal/storage/ebszfs/zfs.go)に対応する行
		"/usr/sbin/zfs clone tpool/base@* tpool/branches/*",
		"/usr/sbin/zfs snapshot tpool/branches/*@init",
		"/usr/sbin/zfs snapshot tpool/base@*",
		"/usr/sbin/zfs rollback -r tpool/branches/*@init",
		"/usr/sbin/zfs destroy -r tpool/branches/*",
		"/usr/sbin/zfs destroy tpool/base@*",
		"/usr/sbin/zfs rename tpool/branches/* tpool/branches/*",
		"/usr/sbin/zfs get *",
		"/usr/sbin/zfs list *",
		"/usr/sbin/zpool list *",
		// systemctl: mysqld@ / postgres-sashiki@ に限定
		"/usr/bin/systemctl start mysqld@*",
		"/usr/bin/systemctl stop mysqld@*",
		"/usr/bin/systemctl kill -s SIGKILL mysqld@*",
		"/usr/bin/systemctl is-active mysqld@*",
		"/usr/bin/systemctl start postgres-sashiki@*",
		"/usr/bin/systemctl kill -s SIGKILL postgres-sashiki@*",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sudoers missing %q", want)
		}
	}
}

// 旧方式 sudoers 生成: 危険形(無制限ワイルドカード等)が含まれないこと(#78)。
func TestSudoersLegacyContentNoDangerousForms(t *testing.T) {
	got := sudoersLegacyContent("tpool")
	for _, deny := range []string{
		// 旧形式の無制限許可
		"zfs destroy *",
		"zfs clone *",
		"zfs snapshot *",
		"zfs rollback *",
		"zfs rename *",
		// 任意プロパティ変更(set)は許可しない(mountpoint 書き換え等が可能なため)
		"zfs set",
		// kill はユニット・シグナル無指定を許可しない
		"systemctl kill *",
		// 全許可
		"NOPASSWD: ALL",
		"/usr/sbin/zfs,",
	} {
		if strings.Contains(got, deny) {
			t.Errorf("sudoers must not contain dangerous form %q", deny)
		}
	}
	// destroy 行は必ず branches/ 配下の再帰破棄か base の snapshot(@ 付き)に限る。
	// pool 本体(tpool)や base データセット本体を破棄できる行があってはならない。
	for _, line := range strings.Split(got, "\n") {
		if !strings.Contains(line, "destroy") {
			continue
		}
		if !strings.Contains(line, "destroy -r tpool/branches/*") && !strings.Contains(line, "destroy tpool/base@*") {
			t.Errorf("unexpected destroy form: %q", line)
		}
	}
}

// 旧方式 sudoers 生成: pool 名が反映されること。
func TestSudoersLegacyContentPoolPropagation(t *testing.T) {
	got := sudoersLegacyContent("mypool")
	if !strings.Contains(got, "mypool/branches/*") || !strings.Contains(got, "mypool/base@*") {
		t.Errorf("pool name should propagate into dataset patterns:\n%s", got)
	}
	if strings.Contains(got, "tpool") || strings.Contains(got, "dbpool") {
		t.Errorf("sudoers should not contain hardcoded pool names:\n%s", got)
	}
}

// AppArmor プロファイル生成: 完全プロファイル形式で必要な許可が揃うこと(#79)。
func TestApparmorProfileRequiredRules(t *testing.T) {
	got := apparmorProfile("tpool")
	for _, want := range []string{
		// 完全プロファイル(local override ではなく profile ブロックを持つ)
		"profile sashiki-mysqld /usr/sbin/mysqld",
		"#include <abstractions/base>",
		"#include <abstractions/mysql>",
		// datadir(pool 連動)
		"/tpool/branches/** rwk,",
		"/tpool/base/** rwk,",
		// ログ・実行時ファイル
		"/var/log/sashiki/** rw,",
		"/run/sashiki/** rw,",
		// ソケット・pid(mysqld@<branch> / baseline import / refresh)
		"/tmp/mysql-*.sock* rwk,",
		"/tmp/mysql-*.pid rwk,",
		"/tmp/sashiki-*.sock* rwk,",
		"/tmp/sashiki-*.pid rwk,",
		// mysqld 本体とプラグイン
		"/usr/sbin/mysqld mr,",
		"/usr/lib/mysql/plugin/** mr,",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("apparmor profile missing %q", want)
		}
	}
}

// AppArmor プロファイル生成: pool 連動と datadir 外許可の不在(#79)。
func TestApparmorProfilePoolPropagation(t *testing.T) {
	got := apparmorProfile("mypool")
	for _, want := range []string{"/mypool/branches/** rwk,", "/mypool/base/** rwk,"} {
		if !strings.Contains(got, want) {
			t.Errorf("apparmor profile missing pool-linked rule %q", want)
		}
	}
	// datadir 外への書込を許す行があってはならない(負のテストの前提)。
	// 各ルール行の先頭トークン(パス)で判定する。
	denyPaths := map[string]bool{"/**": true, "/var/lib/mysql/": true, "/var/lib/mysql/**": true, "/etc/**": true}
	for _, line := range strings.Split(got, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "/") {
			continue
		}
		if denyPaths[fields[0]] {
			t.Errorf("apparmor profile must not have a rule for %q: %q", fields[0], line)
		}
	}
}
