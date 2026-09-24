// Package zfs はローカル OpenZFS バックエンド。zfs/zpool コマンドを実行する。
// sashikid の実行ユーザーには sudoers で /usr/sbin/zfs のみを許可する想定
// (deploy/systemd/README 参照)。
package ebszfs

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/rikukadev/sashiki/internal/storage"
)

// Config は zfs バックエンドの設定。
type Config struct {
	Pool             string // dbpool
	BaseDataset      string // dbpool/base
	BranchParent     string // dbpool/branches
	BaselineSnapshot string // baseline (dbpool/base@baseline)
	ZfsBin           string // 既定 "zfs"。テストで差し替える
	Sudo             bool   // true なら sudo 経由で実行
	// RootHelper が設定されていれば `sudo -n <helper> zfs|zpool ...` で実行する(#276)。
	// Sudo より優先。helper 側が引数を allowlist で検証する。
	RootHelper string
}

// Backend は storage.Storage の zfs 実装。
type Backend struct {
	cfg Config
	run runner
}

type runner func(ctx context.Context, args ...string) (string, error)

// New は zfs バックエンドを作る。
func New(cfg Config) *Backend {
	if cfg.ZfsBin == "" {
		cfg.ZfsBin = "zfs"
	}
	b := &Backend{cfg: cfg}
	b.run = b.execZfs
	return b
}

func (b *Backend) execZfs(ctx context.Context, args ...string) (string, error) {
	out, err := b.privCmd(ctx, b.cfg.ZfsBin, "zfs", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("zfs %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// privCmd は root 操作のコマンドを組む。root-helper があれば
// `sudo -n <helper> <tool> args...`(helper 側で検証)、無ければ sudo の有無だけ切り替える。
// tool は helper に渡すコマンド名(zfs / zpool)、bin は直接実行するときのパス。
func (b *Backend) privCmd(ctx context.Context, bin, tool string, args ...string) *exec.Cmd {
	switch {
	case b.cfg.RootHelper != "":
		return exec.CommandContext(ctx, "sudo", append([]string{"-n", b.cfg.RootHelper, tool}, args...)...)
	case b.cfg.Sudo:
		return exec.CommandContext(ctx, "sudo", append([]string{"-n", bin}, args...)...)
	default:
		return exec.CommandContext(ctx, bin, args...)
	}
}

// Capabilities はローカル zfs の性格: 全操作が速い。
func (b *Backend) Capabilities() storage.Capabilities {
	return storage.Capabilities{
		FastRollback:      true,
		TypicalCreate:     2 * time.Second,
		ClonesAreDistinct: false, // 固定名 branch_parent/<name>
	}
}

func (b *Backend) branchDataset(name string) string {
	return b.cfg.BranchParent + "/" + name
}

// BasePath は base データセットのマウントポイント(quiesce チェック用)。
func (b *Backend) BasePath(ctx context.Context) (string, error) {
	return b.run(ctx, "get", "-H", "-o", "value", "mountpoint", b.cfg.BaseDataset)
}

// CurrentBaseline は現在のベースライン snapshot の完全修飾名。
func (b *Backend) CurrentBaseline() storage.SnapshotRef {
	return storage.SnapshotRef(b.cfg.BaseDataset + "@" + b.cfg.BaselineSnapshot)
}

// Clone は zfs clone してマウントポイントを返す。
func (b *Backend) Clone(ctx context.Context, baseline storage.SnapshotRef, name string) (storage.Volume, error) {
	ds := b.branchDataset(name)
	if _, err := b.run(ctx, "clone", string(baseline), ds); err != nil {
		return storage.Volume{}, err
	}
	mp, err := b.run(ctx, "get", "-H", "-o", "value", "mountpoint", ds)
	if err != nil {
		return storage.Volume{}, err
	}
	return storage.Volume{Name: name, Dataset: ds, Path: mp}, nil
}

// ListBranchVolumes は branch_parent 配下の branch 名一覧(reconciliation 用)。
func (b *Backend) ListBranchVolumes(ctx context.Context) ([]string, error) {
	out, err := b.run(ctx, "list", "-H", "-o", "name", "-r", b.cfg.BranchParent)
	if err != nil {
		return nil, err
	}
	var names []string
	prefix := b.cfg.BranchParent + "/"
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == b.cfg.BranchParent {
			continue
		}
		if strings.HasPrefix(line, prefix) {
			rest := line[len(prefix):]
			// snapshot(@)やネストは除外し、直下の branch 名のみ
			if !strings.Contains(rest, "/") && !strings.Contains(rest, "@") {
				names = append(names, rest)
			}
		}
	}
	return names, nil
}

// ResolveVolume は既存ブランチのデータセットとマウントポイントを引く。
func (b *Backend) ResolveVolume(ctx context.Context, name string) (storage.Volume, error) {
	ds := b.branchDataset(name)
	mp, err := b.run(ctx, "get", "-H", "-o", "value", "mountpoint", ds)
	if err != nil {
		return storage.Volume{}, err
	}
	return storage.Volume{Name: name, Dataset: ds, Path: mp}, nil
}

// SnapshotInit は clone 直後(mysqld 起動前)に @init を取得する。
func (b *Backend) SnapshotInit(ctx context.Context, vol storage.Volume) (storage.SnapshotRef, error) {
	snap := vol.Dataset + "@init"
	if _, err := b.run(ctx, "snapshot", snap); err != nil {
		return storage.NopSnapshot, err
	}
	return storage.SnapshotRef(snap), nil
}

// SetQuota は branch dataset に refquota を設定する(#85)。bytes<=0 で解除。
// refquota は「その dataset 自身(スナップショット除く)」の上限。1 ブランチの
// 暴走が pool を食い尽くすのを防ぐ。超過時は書き込みが ENOSPC で失敗する
// (@init は create 時点の小さな snapshot なので影響しない)。
func (b *Backend) SetQuota(ctx context.Context, vol storage.Volume, bytes int64) error {
	val := "none"
	if bytes > 0 {
		val = strconv.FormatInt(bytes, 10)
	}
	_, err := b.run(ctx, "set", "refquota="+val, vol.Dataset)
	return err
}

// Rollback は @init への巻き戻し。-r で @init より後のスナップショットも破棄する。
func (b *Backend) Rollback(ctx context.Context, vol storage.Volume, snap storage.SnapshotRef) error {
	_, err := b.run(ctx, "rollback", "-r", string(snap))
	return err
}

// Rename は zfs rename で dataset 名を変える(recreate の退避スワップ用)。
func (b *Backend) Rename(ctx context.Context, vol storage.Volume, newName string) (storage.Volume, error) {
	newDS := b.branchDataset(newName)
	if _, err := b.run(ctx, "rename", vol.Dataset, newDS); err != nil {
		return storage.Volume{}, err
	}
	mp, err := b.run(ctx, "get", "-H", "-o", "value", "mountpoint", newDS)
	if err != nil {
		return storage.Volume{}, err
	}
	return storage.Volume{Name: newName, Dataset: newDS, Path: mp}, nil
}

// DeleteAsync は zfs destroy。ローカル zfs は同期で即完了するため、
// 完了済みジョブを返す。
func (b *Backend) DeleteAsync(ctx context.Context, vol storage.Volume) (storage.JobID, error) {
	if _, err := b.run(ctx, "destroy", "-r", vol.Dataset); err != nil {
		return "", err
	}
	return storage.JobID("done:" + vol.Dataset), nil
}

// Poll は DeleteAsync が同期完了しているため常に completed。
func (b *Backend) Poll(ctx context.Context, job storage.JobID) (storage.JobStatus, error) {
	return storage.JobCompleted, nil
}

// DeleteBaselineSnapshot は base の snapshot を破棄する(baseline GC 用)。
// 派生 clone が残っていれば zfs が拒否するため、GC は参照カウントで守る。
func (b *Backend) DeleteBaselineSnapshot(ctx context.Context, snap storage.SnapshotRef) error {
	_, err := b.run(ctx, "destroy", string(snap))
	return err
}

// SnapshotBase は base から新しいベースライン snapshot を取得する。
func (b *Backend) SnapshotBase(ctx context.Context, tag string) (storage.SnapshotRef, error) {
	snap := b.cfg.BaseDataset + "@" + tag
	if _, err := b.run(ctx, "snapshot", snap); err != nil {
		return storage.NopSnapshot, err
	}
	return storage.SnapshotRef(snap), nil
}

// ListSnapshots は base のスナップショット一覧。
// PromoteBranch は branch dataset の snapshot を新しい baseline とする(#129)。
// 新規ブランチはこの snapshot から clone される。呼び出し側が branch mysqld を
// 停止済みであることを前提とする。
func (b *Backend) PromoteBranch(ctx context.Context, branch storage.Volume, tag string) (storage.SnapshotRef, error) {
	ref := branch.Dataset + "@" + tag
	if _, err := b.run(ctx, "snapshot", ref); err != nil {
		return "", err
	}
	return storage.SnapshotRef(ref), nil
}

func (b *Backend) ListSnapshots(ctx context.Context) ([]storage.SnapshotRef, error) {
	out, err := b.run(ctx, "list", "-H", "-t", "snapshot", "-o", "name", "-r", b.cfg.BaseDataset)
	if err != nil {
		return nil, err
	}
	var refs []storage.SnapshotRef
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			refs = append(refs, storage.SnapshotRef(line))
		}
	}
	return refs, nil
}

// UsedBytes は zfs get used の値。
func (b *Backend) UsedBytes(ctx context.Context, vol storage.Volume) (int64, error) {
	out, err := b.run(ctx, "get", "-H", "-p", "-o", "value", "used", vol.Dataset)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(out, 10, 64)
}

// LogicalBytes は volume の referenced(論理サイズ)。CoW の Private delta とは別。
func (b *Backend) LogicalBytes(ctx context.Context, vol storage.Volume) (int64, error) {
	out, err := b.run(ctx, "get", "-H", "-p", "-o", "value", "referenced", vol.Dataset)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(out, 10, 64)
}

// PoolCapacity は zpool の alloc / size(watermark 判定用)。zpool は zfs とは
// 別バイナリのため専用に実行する。
func (b *Backend) PoolCapacity(ctx context.Context) (used, total int64, err error) {
	args := []string{"list", "-Hp", "-o", "alloc,size", b.cfg.Pool}
	out, err := b.privCmd(ctx, "zpool", "zpool", args...).CombinedOutput()
	if err != nil {
		return 0, 0, fmt.Errorf("zpool list: %w: %s", err, strings.TrimSpace(string(out)))
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected zpool output: %q", string(out))
	}
	used, _ = strconv.ParseInt(fields[0], 10, 64)
	total, _ = strconv.ParseInt(fields[1], 10, 64)
	return used, total, nil
}

// PoolStatus は `zpool status -x` 相当の健全性を返す(仕様 20-2, #88)。
// PoolCapacity(容量が読めるか)とは別で、DEGRADED / FAULTED 等の異常を検出する。
// healthy=true なら detail は "healthy"、false なら zpool の診断出力を detail に入れる。
func (b *Backend) PoolStatus(ctx context.Context) (healthy bool, detail string, err error) {
	args := []string{"status", "-x", b.cfg.Pool}
	out, err := b.privCmd(ctx, "zpool", "zpool", args...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return false, text, fmt.Errorf("zpool status -x: %w: %s", err, text)
	}
	// 正常時は "pool '<pool>' is healthy" / "all pools are healthy"。
	if strings.Contains(text, "is healthy") || strings.Contains(text, "all pools are healthy") {
		return true, "healthy", nil
	}
	return false, text, nil
}
