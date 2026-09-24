package roothelper

import (
	"strings"
	"testing"
)

func testCfg() Config {
	c, err := Config{Pool: "tpool"}.normalize()
	if err != nil {
		panic(err)
	}
	return c
}

// sashikid が実際に発行する形(internal/storage/ebszfs / internal/engine)は全部通る。
func TestValidateAllowsRealInvocations(t *testing.T) {
	c := testCfg()
	for _, args := range [][]string{
		{"zfs", "clone", "tpool/base@baseline", "tpool/branches/pr-1"},
		{"zfs", "clone", "tpool/base@baseline-20260901T000000Z", "tpool/branches/_validate"},
		{"zfs", "clone", "tpool/branches/pr-1@baseline-20260901T000000Z", "tpool/branches/pr-2"},
		{"zfs", "snapshot", "tpool/branches/pr-1@init"},
		{"zfs", "snapshot", "tpool/base@baseline-20260901T000000Z"},
		{"zfs", "snapshot", "tpool/branches/pr-1@baseline-20260901T000000Z"},
		{"zfs", "rollback", "-r", "tpool/branches/pr-1@init"},
		{"zfs", "destroy", "-r", "tpool/branches/pr-1"},
		{"zfs", "destroy", "-r", "tpool/branches/pr-1-recreating"},
		{"zfs", "destroy", "tpool/base@baseline-20260901T000000Z"},
		{"zfs", "destroy", "tpool/branches/pr-1@baseline-20260901T000000Z"},
		{"zfs", "rename", "tpool/branches/pr-1", "tpool/branches/pr-1-recreating"},
		{"zfs", "set", "refquota=10737418240", "tpool/branches/pr-1"},
		{"zfs", "set", "refquota=none", "tpool/branches/pr-1"},
		{"zfs", "get", "-H", "-o", "value", "mountpoint", "tpool/branches/pr-1"},
		{"zfs", "get", "-H", "-o", "value", "mountpoint", "tpool/base"},
		{"zfs", "get", "-H", "-p", "-o", "value", "used", "tpool/branches/pr-1"},
		{"zfs", "get", "-H", "-p", "-o", "value", "referenced", "tpool/branches/pr-1"},
		{"zfs", "list", "-H", "-o", "name", "-r", "tpool/branches"},
		{"zfs", "list", "-H", "-t", "snapshot", "-o", "name", "-r", "tpool/base"},
		{"zpool", "list", "-Hp", "-o", "alloc,size", "tpool"},
		{"zpool", "status", "-x", "tpool"},
		{"systemctl", "start", "mysqld@pr-1"},
		{"systemctl", "stop", "mysqld@pr-1"},
		{"systemctl", "is-active", "mysqld@pr-1"},
		{"systemctl", "kill", "-s", "SIGKILL", "mysqld@pr-1"},
		{"systemctl", "start", "postgres-sashiki@pg-1"},
		{"systemctl", "kill", "-s", "SIGKILL", "postgres-sashiki@pg-1"},
	} {
		if err := c.Validate(args); err != nil {
			t.Errorf("%q should be allowed: %v", strings.Join(args, " "), err)
		}
	}
}

// 許可外 dataset・追加フラグ・任意 property・任意 unit は拒否する(#276 の negative test)。
func TestValidateRejectsEscapes(t *testing.T) {
	c := testCfg()
	for _, args := range [][]string{
		// 許可外 dataset / pool
		{"zfs", "destroy", "-r", "tpool"},
		{"zfs", "destroy", "-r", "tpool/base"},
		{"zfs", "destroy", "-r", "rpool/ROOT/ubuntu"},
		{"zfs", "destroy", "tpool/branches/pr-1"},      // dataset 本体の非再帰 destroy 形は使わない
		{"zfs", "destroy", "tpool/branches/pr-1@init"}, // @init は rollback で使う。単独 destroy は不可
		{"zfs", "clone", "tpool/base@baseline", "tpool/base2"},
		{"zfs", "clone", "rpool/x@y", "tpool/branches/pr-1"},
		{"zfs", "rename", "tpool/branches/pr-1", "tpool/base"},
		{"zfs", "rollback", "-r", "tpool/base@baseline"},
		{"zfs", "snapshot", "tpool/branches/pr-1@evil"},
		{"zfs", "clone", "tpool/base@baseline", "tpool/branches/pr-1/child"},
		{"zfs", "clone", "tpool/base@baseline", "tpool/branches/../base"},
		// 追加フラグ / 任意 property(sudoers の * では防げなかった形)
		{"zfs", "clone", "tpool/base@baseline", "-o", "mountpoint=/etc", "tpool/branches/pr-1"},
		{"zfs", "clone", "-o", "mountpoint=/etc", "tpool/base@baseline", "tpool/branches/pr-1"},
		{"zfs", "set", "mountpoint=/etc", "tpool/branches/pr-1"},
		{"zfs", "set", "refquota=10G;", "tpool/branches/pr-1"},
		{"zfs", "set", "refquota=none", "tpool/base"},
		{"zfs", "destroy", "-r", "-f", "tpool/branches/pr-1"},
		{"zfs", "get", "-H", "-o", "value", "mountpoint", "rpool/ROOT"},
		{"zfs", "get", "-H", "-o", "value,source", "mountpoint", "tpool/base"},
		{"zfs", "get", "-H", "-o", "value", "canmount", "tpool/base"},
		{"zfs", "list", "-H", "-o", "name", "-r", "rpool"},
		{"zfs", "mount", "tpool/branches/pr-1"},
		{"zfs", "send", "tpool/base@baseline"},
		{"zpool", "destroy", "tpool"},
		{"zpool", "list", "-Hp", "-o", "alloc,size", "rpool"},
		{"zpool", "import", "-f", "tpool"},
		// 任意 unit / シグナル
		{"systemctl", "start", "sshd"},
		{"systemctl", "stop", "sashikid"},
		{"systemctl", "start", "mysqld@pr-1", "--now"},
		{"systemctl", "kill", "-s", "SIGTERM", "mysqld@pr-1"},
		{"systemctl", "kill", "mysqld@pr-1"},
		{"systemctl", "daemon-reload"},
		{"systemctl", "start", "mysqld@../evil"},
		{"systemctl", "start", "mysqld@"},
		// 別コマンド
		{"sh", "-c", "id"},
		{"/usr/sbin/zfs", "list"},
		{},
		{"zfs"},
		{"zfs", "clone", "tpool/base@baseline", "tpool/branches/pr-1\n"},
		{"zfs", "clone", "tpool/base@baseline", "tpool/branches/-evil"},
	} {
		if err := c.Validate(args); err == nil {
			t.Errorf("%q must be rejected", strings.Join(args, " "))
		}
	}
}

func TestConfigNormalize(t *testing.T) {
	if _, err := (Config{Pool: "tpool", BranchParent: "rpool/branches"}).normalize(); err == nil {
		t.Error("branch_parent outside the pool must be rejected")
	}
	if _, err := (Config{Pool: "../x"}).normalize(); err == nil {
		t.Error("invalid pool name must be rejected")
	}
	c, err := (Config{Pool: "p", Units: []string{"custom"}}).normalize()
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseDataset != "p/base" || c.BranchParent != "p/branches" || c.Units[0] != "custom" {
		t.Errorf("unexpected defaults: %+v", c)
	}
}
