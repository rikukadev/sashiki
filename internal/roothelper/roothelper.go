// Package roothelper は sashiki-root-helper の中身。sashikid(User=sashiki)が要る
// root 操作(zfs / zpool / systemctl)を、型付きの allowlist で検証してから実行する
// (仕様 20-3、#276)。
//
// 以前は sudoers に `zfs clone <base>@* <branches>/*` のようなパターン行を並べていた
// が、sudoers の `*` は空白をまたぐため `-o mountpoint=/etc` のような追加引数を
// 挟めてしまい、閉じ込めが完全ではなかった。helper は引数を 1 個ずつ検証し、
// 許可した形以外は一切実行しない。sudoers は helper 1 行だけになる。
//
// 検証は Validate(純関数)に集約し、negative test で「許可外 dataset・追加フラグ・
// 任意 property・任意 unit」を拒否することを確かめる。
package roothelper

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConfigPath は helper が読む設定。root 所有 0600 で sashiki init が書く。
// 引数や環境変数で場所を変えられると sashiki ユーザーが差し替えられるので固定。
const ConfigPath = "/etc/sashiki/root-helper.yaml"

// Config は helper が許可する対象。sashikid の config と同じ pool / dataset 名を
// sashiki init が書き出す。
type Config struct {
	Pool         string   `yaml:"pool"`
	BaseDataset  string   `yaml:"base_dataset"`
	BranchParent string   `yaml:"branch_parent"`
	Units        []string `yaml:"units"` // systemctl の template unit 名(mysqld / postgres-sashiki)
}

// DefaultUnits は sashiki が起動するインスタンスユニットの template 名。
var DefaultUnits = []string{"mysqld", "postgres-sashiki"}

// Load は ConfigPath を読む。
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c.normalize()
}

func (c Config) normalize() (Config, error) {
	if c.Pool == "" || !nameRe.MatchString(c.Pool) {
		return c, fmt.Errorf("pool が不正です: %q", c.Pool)
	}
	if c.BaseDataset == "" {
		c.BaseDataset = c.Pool + "/base"
	}
	if c.BranchParent == "" {
		c.BranchParent = c.Pool + "/branches"
	}
	for _, ds := range []string{c.BaseDataset, c.BranchParent} {
		if !strings.HasPrefix(ds, c.Pool+"/") || !datasetRe.MatchString(ds) {
			return c, fmt.Errorf("dataset %q は pool %q 配下ではありません", ds, c.Pool)
		}
	}
	if len(c.Units) == 0 {
		c.Units = DefaultUnits
	}
	for _, u := range c.Units {
		if !nameRe.MatchString(u) {
			return c, fmt.Errorf("unit 名が不正です: %q", u)
		}
	}
	return c, nil
}

// Binaries は実行する実体。PATH に頼らず絶対パスで exec する。
var Binaries = map[string]string{
	"zfs":       "/usr/sbin/zfs",
	"zpool":     "/usr/sbin/zpool",
	"systemctl": "/usr/bin/systemctl",
}

// name は branch 名・tag・unit 名に許す文字。先頭に `-` を許さない(フラグと
// 誤認させない)。`_validate` や `<name>-recreating` のような内部名は通す。
const namePat = `[A-Za-z0-9_][A-Za-z0-9._-]{0,79}`

var (
	nameRe    = regexp.MustCompile(`^` + namePat + `$`)
	datasetRe = regexp.MustCompile(`^` + namePat + `(/` + namePat + `)*$`)
	quotaRe   = regexp.MustCompile(`^refquota=(none|[0-9]{1,20})$`)
)

// Validate は argv(先頭がコマンド名)が許可した形かを判定する。エラーは拒否理由。
func (c Config) Validate(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("コマンドがありません")
	}
	for _, a := range args {
		if strings.ContainsAny(a, "\x00\n") {
			return fmt.Errorf("引数に不正な文字が含まれています")
		}
	}
	switch args[0] {
	case "zfs":
		return c.validateZFS(args[1:])
	case "zpool":
		return c.validateZpool(args[1:])
	case "systemctl":
		return c.validateSystemctl(args[1:])
	default:
		return fmt.Errorf("コマンド %q は許可されていません", args[0])
	}
}

// --- dataset の形 ---

func (c Config) isBranchDS(s string) bool {
	rest, ok := strings.CutPrefix(s, c.BranchParent+"/")
	return ok && nameRe.MatchString(rest)
}

func (c Config) isBaseSnap(s string) bool {
	rest, ok := strings.CutPrefix(s, c.BaseDataset+"@")
	return ok && nameRe.MatchString(rest)
}

func (c Config) isBranchInitSnap(s string) bool {
	ds, snap, ok := strings.Cut(s, "@")
	return ok && snap == "init" && c.isBranchDS(ds)
}

// promote 済み baseline(branch dataset 上の @baseline-<tag>、#129 / #178)。
func (c Config) isBranchBaselineSnap(s string) bool {
	ds, snap, ok := strings.Cut(s, "@")
	if !ok || !c.isBranchDS(ds) {
		return false
	}
	tag, ok := strings.CutPrefix(snap, "baseline-")
	return ok && nameRe.MatchString(tag)
}

// isInPool は参照系(get / list)に許す対象: pool 配下の dataset か、その snapshot。
func (c Config) isInPool(s string) bool {
	ds, snap, hasSnap := strings.Cut(s, "@")
	if hasSnap && !nameRe.MatchString(snap) {
		return false
	}
	if ds == c.Pool {
		return true
	}
	rest, ok := strings.CutPrefix(ds, c.Pool+"/")
	return ok && datasetRe.MatchString(rest)
}

func (c Config) validateZFS(a []string) error {
	if len(a) == 0 {
		return fmt.Errorf("zfs: サブコマンドがありません")
	}
	sub, rest := a[0], a[1:]
	switch sub {
	case "clone": // clone <base@tag | branch@baseline-tag> <branch>
		if len(rest) == 2 && (c.isBaseSnap(rest[0]) || c.isBranchBaselineSnap(rest[0])) && c.isBranchDS(rest[1]) {
			return nil
		}
	case "snapshot": // snapshot <branch@init | base@tag | branch@baseline-tag>
		if len(rest) == 1 && (c.isBranchInitSnap(rest[0]) || c.isBaseSnap(rest[0]) || c.isBranchBaselineSnap(rest[0])) {
			return nil
		}
	case "rollback": // rollback -r <branch@init>
		if len(rest) == 2 && rest[0] == "-r" && c.isBranchInitSnap(rest[1]) {
			return nil
		}
	case "destroy": // destroy -r <branch> | destroy <base@tag | branch@baseline-tag>
		if len(rest) == 2 && rest[0] == "-r" && c.isBranchDS(rest[1]) {
			return nil
		}
		if len(rest) == 1 && (c.isBaseSnap(rest[0]) || c.isBranchBaselineSnap(rest[0])) {
			return nil
		}
	case "rename": // rename <branch> <branch>
		if len(rest) == 2 && c.isBranchDS(rest[0]) && c.isBranchDS(rest[1]) {
			return nil
		}
	case "set": // set refquota=<none|bytes> <branch>(他の property は不可)
		if len(rest) == 2 && quotaRe.MatchString(rest[0]) && c.isBranchDS(rest[1]) {
			return nil
		}
	case "get": // get -H [-p] -o value <mountpoint|used|referenced> <pool 配下>
		props := map[string]bool{"mountpoint": true, "used": true, "referenced": true}
		switch {
		case len(rest) == 5 && rest[0] == "-H" && rest[1] == "-o" && rest[2] == "value" && props[rest[3]] && c.isInPool(rest[4]):
			return nil
		case len(rest) == 6 && rest[0] == "-H" && rest[1] == "-p" && rest[2] == "-o" && rest[3] == "value" && props[rest[4]] && c.isInPool(rest[5]):
			return nil
		}
	case "list": // list -H -o name -r <pool 配下> | list -H -t snapshot -o name -r <pool 配下>
		switch {
		case len(rest) == 5 && rest[0] == "-H" && rest[1] == "-o" && rest[2] == "name" && rest[3] == "-r" && c.isInPool(rest[4]):
			return nil
		case len(rest) == 7 && rest[0] == "-H" && rest[1] == "-t" && rest[2] == "snapshot" && rest[3] == "-o" && rest[4] == "name" && rest[5] == "-r" && c.isInPool(rest[6]):
			return nil
		}
	default:
		return fmt.Errorf("zfs %s は許可されていません", sub)
	}
	return fmt.Errorf("zfs %s: 引数の形が許可されていません: %q", sub, rest)
}

func (c Config) validateZpool(a []string) error {
	switch {
	case len(a) == 5 && a[0] == "list" && a[1] == "-Hp" && a[2] == "-o" && a[3] == "alloc,size" && a[4] == c.Pool:
		return nil
	case len(a) == 3 && a[0] == "status" && a[1] == "-x" && a[2] == c.Pool:
		return nil
	}
	return fmt.Errorf("zpool: 引数の形が許可されていません: %q", a)
}

func (c Config) isUnit(s string) bool {
	tmpl, inst, ok := strings.Cut(s, "@")
	if !ok || !nameRe.MatchString(inst) {
		return false
	}
	for _, u := range c.Units {
		if u == tmpl {
			return true
		}
	}
	return false
}

func (c Config) validateSystemctl(a []string) error {
	switch {
	case len(a) == 2 && (a[0] == "start" || a[0] == "stop" || a[0] == "is-active") && c.isUnit(a[1]):
		return nil
	case len(a) == 4 && a[0] == "kill" && a[1] == "-s" && a[2] == "SIGKILL" && c.isUnit(a[3]):
		return nil
	}
	return fmt.Errorf("systemctl: 引数の形が許可されていません: %q", a)
}
