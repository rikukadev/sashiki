// Package fsx は FSx for OpenZFS バックエンド。snapshot/clone を AWS API で
// 行い、ボリュームを NFS でマウントして使う。実測(RESULTS-FSX.md):
// clone 52〜71秒 / restore 10分超 / delete 6分 — コントロールプレーンは
// 「分」の世界なので、Capabilities でそれを宣言しコアに挙動を切り替えさせる。
//
// 完了判定は Volume の Lifecycle ではなく AdministrativeActions を見る
// (restore/clone 中も Lifecycle は AVAILABLE のまま — 実測で datadir を
// 壊した教訓)。
package fsxzfs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	awsfsx "github.com/aws/aws-sdk-go-v2/service/fsx"
	"github.com/aws/aws-sdk-go-v2/service/fsx/types"
	"github.com/rikukadev/sashiki/internal/storage"
)

// Config は fsx バックエンドの設定。
type Config struct {
	FileSystemID     string // fs-xxxx
	BaseVolumeID     string // fsvol-xxxx (base)
	ParentVolumeID   string // fsvol-xxxx (root)。空なら filesystem から自動発見
	BaselineSnapshot string // baseline (base ボリューム上の snapshot 名)
	DNSName          string // fs-xxxx.fsx.<region>.amazonaws.com
	MountRoot        string // /mnt/sashiki
	PollInterval     time.Duration
}

// API は fsx バックエンドが使う AWS API のサブセット(テストでモックする)。
type API interface {
	CreateVolume(ctx context.Context, in *awsfsx.CreateVolumeInput, opts ...func(*awsfsx.Options)) (*awsfsx.CreateVolumeOutput, error)
	DeleteVolume(ctx context.Context, in *awsfsx.DeleteVolumeInput, opts ...func(*awsfsx.Options)) (*awsfsx.DeleteVolumeOutput, error)
	DescribeVolumes(ctx context.Context, in *awsfsx.DescribeVolumesInput, opts ...func(*awsfsx.Options)) (*awsfsx.DescribeVolumesOutput, error)
	CreateSnapshot(ctx context.Context, in *awsfsx.CreateSnapshotInput, opts ...func(*awsfsx.Options)) (*awsfsx.CreateSnapshotOutput, error)
	DescribeSnapshots(ctx context.Context, in *awsfsx.DescribeSnapshotsInput, opts ...func(*awsfsx.Options)) (*awsfsx.DescribeSnapshotsOutput, error)
	DescribeFileSystems(ctx context.Context, in *awsfsx.DescribeFileSystemsInput, opts ...func(*awsfsx.Options)) (*awsfsx.DescribeFileSystemsOutput, error)
	DeleteSnapshot(ctx context.Context, in *awsfsx.DeleteSnapshotInput, opts ...func(*awsfsx.Options)) (*awsfsx.DeleteSnapshotOutput, error)
	UpdateVolume(ctx context.Context, in *awsfsx.UpdateVolumeInput, opts ...func(*awsfsx.Options)) (*awsfsx.UpdateVolumeOutput, error)
}

// Backend は storage.Storage の FSx 実装。
type Backend struct {
	cfg Config
	api API
	// mount/umount 実行(テストで差し替え)
	mount  func(ctx context.Context, source, target string) error
	umount func(ctx context.Context, target string) error
	// statfs / isMount は NFS マウント上の使用量取得(テストで差し替え、#278)
	statfs  func(path string) (used, avail int64, err error)
	isMount func(path string) (bool, error)

	baseMu sync.Mutex // baseMount の遅延マウントを直列化
}

// New は fsx バックエンドを作る。
func New(cfg Config, api API) *Backend {
	if cfg.MountRoot == "" {
		cfg.MountRoot = "/mnt/sashiki"
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 5 * time.Second
	}
	b := &Backend{cfg: cfg, api: api}
	b.mount = b.execMount
	b.umount = b.execUmount
	b.statfs = statfsUsage
	b.isMount = isMountPoint
	return b
}

// Capabilities: 実測値の宣言。lazy create は無効化され、reset は
// 「新クローン + 付け替え」の共通経路になる(仕様 15-3)。
func (b *Backend) Capabilities() storage.Capabilities {
	return storage.Capabilities{
		FastRollback:      false,
		TypicalCreate:     70 * time.Second,
		ClonesAreDistinct: true, // 世代サフィックス name-g<hex>
	}
}

func (b *Backend) execMount(ctx context.Context, source, target string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "mount", "-t", "nfs",
		"-o", "nfsvers=4.1,rsize=1048576,wsize=1048576,hard,noatime",
		source, target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount %s: %w: %s", source, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (b *Backend) execUmount(ctx context.Context, target string) error {
	out, err := exec.CommandContext(ctx, "umount", "-l", target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("umount %s: %w: %s", target, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// CurrentBaseline は base ボリューム上の baseline snapshot の ARN。
func (b *Backend) CurrentBaseline() storage.SnapshotRef {
	arn, err := b.findSnapshotARN(context.Background(), b.cfg.BaselineSnapshot)
	if err != nil {
		return storage.NopSnapshot
	}
	return storage.SnapshotRef(arn)
}

func (b *Backend) findSnapshotARN(ctx context.Context, name string) (string, error) {
	out, err := b.api.DescribeSnapshots(ctx, &awsfsx.DescribeSnapshotsInput{
		Filters: []types.SnapshotFilter{{
			Name:   types.SnapshotFilterNameVolumeId,
			Values: []string{b.cfg.BaseVolumeID},
		}},
	})
	if err != nil {
		return "", err
	}
	for _, s := range out.Snapshots {
		if s.Name != nil && *s.Name == name && s.ResourceARN != nil {
			return *s.ResourceARN, nil
		}
	}
	return "", fmt.Errorf("snapshot %q not found on %s", name, b.cfg.BaseVolumeID)
}

// waitVolumeReady は Lifecycle AVAILABLE かつ AdministrativeActions 全完了を待つ。
func (b *Backend) waitVolumeReady(ctx context.Context, volID string) error {
	for {
		out, err := b.api.DescribeVolumes(ctx, &awsfsx.DescribeVolumesInput{VolumeIds: []string{volID}})
		if err != nil {
			return err
		}
		if len(out.Volumes) == 0 {
			return fmt.Errorf("volume %s not found", volID)
		}
		v := out.Volumes[0]
		ready := v.Lifecycle == types.VolumeLifecycleAvailable
		for _, a := range v.AdministrativeActions {
			if a.Status == types.StatusInProgress || a.Status == types.StatusPending {
				ready = false
			}
			if a.Status == types.StatusFailed {
				return fmt.Errorf("volume %s: administrative action %s failed", volID, a.AdministrativeActionType)
			}
		}
		if v.Lifecycle == types.VolumeLifecycleFailed || v.Lifecycle == types.VolumeLifecycleMisconfigured {
			return fmt.Errorf("volume %s lifecycle: %s", volID, v.Lifecycle)
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.cfg.PollInterval):
		}
	}
}

// Clone はクローンボリュームを作成し、NFS マウントして返す。
// FSx のボリューム名はパス衝突を避けるため世代サフィックス付き
// (name-g<hex>)にし、sashiki-branch タグで元の名前に紐づける。
// reset の「作り直し + 付け替え」で新旧が一時的に共存できる。
// parentVolume は クローンのぶら下げ先(root volume)。設定が空なら
// filesystem から自動発見してキャッシュする。
func (b *Backend) parentVolume(ctx context.Context) (string, error) {
	if b.cfg.ParentVolumeID != "" {
		return b.cfg.ParentVolumeID, nil
	}
	out, err := b.api.DescribeFileSystems(ctx, &awsfsx.DescribeFileSystemsInput{
		FileSystemIds: []string{b.cfg.FileSystemID},
	})
	if err != nil {
		return "", fmt.Errorf("describe filesystem: %w", err)
	}
	if len(out.FileSystems) == 0 || out.FileSystems[0].OpenZFSConfiguration == nil ||
		out.FileSystems[0].OpenZFSConfiguration.RootVolumeId == nil {
		return "", fmt.Errorf("root volume of %s not found", b.cfg.FileSystemID)
	}
	b.cfg.ParentVolumeID = *out.FileSystems[0].OpenZFSConfiguration.RootVolumeId
	return b.cfg.ParentVolumeID, nil
}

func (b *Backend) Clone(ctx context.Context, baseline storage.SnapshotRef, name string) (storage.Volume, error) {
	parent, err := b.parentVolume(ctx)
	if err != nil {
		return storage.Volume{}, err
	}
	gen := make([]byte, 3)
	_, _ = rand.Read(gen)
	fsxName := fmt.Sprintf("%s-g%s", name, hex.EncodeToString(gen))
	out, err := b.api.CreateVolume(ctx, &awsfsx.CreateVolumeInput{
		VolumeType: types.VolumeTypeOpenzfs,
		Name:       &fsxName,
		OpenZFSConfiguration: &types.CreateOpenZFSVolumeConfiguration{
			ParentVolumeId: &parent,
			OriginSnapshot: &types.CreateOpenZFSOriginSnapshotConfiguration{
				SnapshotARN:  (*string)(&baseline),
				CopyStrategy: types.OpenZFSCopyStrategyClone,
			},
			NfsExports: []types.OpenZFSNfsExport{{
				ClientConfigurations: []types.OpenZFSClientConfiguration{{
					Clients: strPtr("*"),
					Options: []string{"rw", "crossmnt", "no_root_squash"},
				}},
			}},
		},
		Tags: []types.Tag{
			{Key: strPtr("sashiki"), Value: strPtr("true")},
			{Key: strPtr("sashiki-branch"), Value: &name},
		},
	})
	if err != nil {
		return storage.Volume{}, fmt.Errorf("fsx create-volume: %w", err)
	}
	volID := *out.Volume.VolumeId
	if err := b.waitVolumeReady(ctx, volID); err != nil {
		return storage.Volume{}, err
	}
	volPath := "/fsx/" + fsxName
	if out.Volume.OpenZFSConfiguration != nil && out.Volume.OpenZFSConfiguration.VolumePath != nil {
		volPath = *out.Volume.OpenZFSConfiguration.VolumePath
	}
	target := filepath.Join(b.cfg.MountRoot, fsxName)
	if err := b.mount(ctx, b.cfg.DNSName+":"+volPath, target); err != nil {
		// マウント失敗でボリュームをリークさせない(best-effort で削除)
		_, _ = b.api.DeleteVolume(ctx, &awsfsx.DeleteVolumeInput{
			VolumeId: &volID,
			OpenZFSConfiguration: &types.DeleteVolumeOpenZFSConfiguration{
				Options: []types.DeleteOpenZFSVolumeOption{
					types.DeleteOpenZFSVolumeOptionDeleteChildVolumesAndSnapshots,
				},
			},
		})
		return storage.Volume{}, err
	}
	return storage.Volume{Name: name, Dataset: volID, Path: target}, nil
}

// listAllVolumes はファイルシステム内の全ボリューム(ページネーション対応)。
func (b *Backend) listAllVolumes(ctx context.Context) ([]types.Volume, error) {
	var all []types.Volume
	var token *string
	for {
		out, err := b.api.DescribeVolumes(ctx, &awsfsx.DescribeVolumesInput{
			Filters: []types.VolumeFilter{{
				Name:   types.VolumeFilterNameFileSystemId,
				Values: []string{b.cfg.FileSystemID},
			}},
			NextToken: token,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, out.Volumes...)
		if out.NextToken == nil {
			return all, nil
		}
		token = out.NextToken
	}
}

// genSuffix は世代サフィックス(-g + 6 hex)。
var genSuffix = regexp.MustCompile(`-g[0-9a-f]{6}$`)

// ResolveVolume はボリューム名の世代サフィックスを剥がしてブランチ名と照合する。
// (DescribeVolumes は Tags を返さない — 実機検証で判明。タグは課金属性用に
// 付けるだけで、解決には使わない。)
// reset の付け替え中は複数世代が共存し得るため、作成が最新の AVAILABLE を採用する。
func (b *Backend) ResolveVolume(ctx context.Context, name string) (storage.Volume, error) {
	vols, err := b.listAllVolumes(ctx)
	if err != nil {
		return storage.Volume{}, err
	}
	var candidates []types.Volume
	for _, v := range vols {
		if v.Lifecycle != types.VolumeLifecycleAvailable || v.Name == nil {
			continue
		}
		if genSuffix.ReplaceAllString(*v.Name, "") == name {
			candidates = append(candidates, v)
		}
	}
	if len(candidates) == 0 {
		return storage.Volume{}, fmt.Errorf("volume for branch %q not found", name)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].CreationTime.After(*candidates[j].CreationTime)
	})
	v := candidates[0]
	return storage.Volume{
		Name:    name,
		Dataset: *v.VolumeId,
		Path:    filepath.Join(b.cfg.MountRoot, *v.Name),
	}, nil
}

// SnapshotInit: fsx は FastRollback を持たないため @init は取得しない
// (reset は「新クローン + 付け替え」)。
func (b *Backend) SnapshotInit(ctx context.Context, vol storage.Volume) (storage.SnapshotRef, error) {
	return storage.NopSnapshot, nil
}

// Rollback は FastRollback=false のため呼ばれない。
func (b *Backend) Rollback(ctx context.Context, vol storage.Volume, snap storage.SnapshotRef) error {
	return errors.New("fsx backend does not support rollback (use recreate)")
}

// DeleteAsync はアンマウントして削除ジョブを投入する(完了まで約6分、Poll で確認)。
func (b *Backend) DeleteAsync(ctx context.Context, vol storage.Volume) (storage.JobID, error) {
	_ = b.umount(ctx, vol.Path)
	_, err := b.api.DeleteVolume(ctx, &awsfsx.DeleteVolumeInput{
		VolumeId: &vol.Dataset,
		OpenZFSConfiguration: &types.DeleteVolumeOpenZFSConfiguration{
			Options: []types.DeleteOpenZFSVolumeOption{
				types.DeleteOpenZFSVolumeOptionDeleteChildVolumesAndSnapshots,
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("fsx delete-volume: %w", err)
	}
	return storage.JobID(vol.Dataset), nil
}

// Poll は削除ジョブの完了確認(ボリュームが消えたら completed)。
func (b *Backend) Poll(ctx context.Context, job storage.JobID) (storage.JobStatus, error) {
	out, err := b.api.DescribeVolumes(ctx, &awsfsx.DescribeVolumesInput{VolumeIds: []string{string(job)}})
	if err != nil {
		var nf *types.VolumeNotFound
		if errors.As(err, &nf) {
			return storage.JobCompleted, nil
		}
		return storage.JobFailed, err
	}
	if len(out.Volumes) == 0 {
		return storage.JobCompleted, nil
	}
	if out.Volumes[0].Lifecycle == types.VolumeLifecycleFailed {
		return storage.JobFailed, nil
	}
	return storage.JobRunning, nil
}

// SnapshotBase は base ボリュームの snapshot を取得して ARN を返す。
func (b *Backend) SnapshotBase(ctx context.Context, tag string) (storage.SnapshotRef, error) {
	out, err := b.api.CreateSnapshot(ctx, &awsfsx.CreateSnapshotInput{
		Name:     &tag,
		VolumeId: &b.cfg.BaseVolumeID,
	})
	if err != nil {
		return storage.NopSnapshot, err
	}
	// snapshot の AVAILABLE を待つ
	snapID := *out.Snapshot.SnapshotId
	for {
		ds, err := b.api.DescribeSnapshots(ctx, &awsfsx.DescribeSnapshotsInput{SnapshotIds: []string{snapID}})
		if err != nil {
			return storage.NopSnapshot, err
		}
		if len(ds.Snapshots) > 0 && ds.Snapshots[0].Lifecycle == types.SnapshotLifecycleAvailable {
			return storage.SnapshotRef(*ds.Snapshots[0].ResourceARN), nil
		}
		select {
		case <-ctx.Done():
			return storage.NopSnapshot, ctx.Err()
		case <-time.After(b.cfg.PollInterval):
		}
	}
}

// ListSnapshots は base ボリュームの snapshot 一覧(ARN)。
func (b *Backend) ListSnapshots(ctx context.Context) ([]storage.SnapshotRef, error) {
	out, err := b.api.DescribeSnapshots(ctx, &awsfsx.DescribeSnapshotsInput{
		Filters: []types.SnapshotFilter{{
			Name:   types.SnapshotFilterNameVolumeId,
			Values: []string{b.cfg.BaseVolumeID},
		}},
	})
	if err != nil {
		return nil, err
	}
	var refs []storage.SnapshotRef
	for _, s := range out.Snapshots {
		if s.ResourceARN != nil {
			refs = append(refs, storage.SnapshotRef(*s.ResourceARN))
		}
	}
	return refs, nil
}

// UsedBytes は branch volume の使用量(clone なら CoW 差分)。FSx の API は
// volume 単位の使用量を返さないが、NFS マウントの statfs は dataset の `used` /
// `avail` を映すので、マウント先から読む(#278: 以前は常に 0 で実値に見えていた)。
// マウントされていなければ -1(不明)を返し、表示側は「-」にする。
func (b *Backend) UsedBytes(ctx context.Context, vol storage.Volume) (int64, error) {
	if mounted, err := b.isMount(vol.Path); err != nil || !mounted {
		return -1, nil
	}
	used, _, err := b.statfs(vol.Path)
	if err != nil {
		return -1, nil
	}
	return used, nil
}

// PoolCapacity は filesystem の容量と使用量(watermark 判定用、#278)。総量は
// DescribeFileSystems の StorageCapacity(GiB)、空きは base volume を NFS で
// マウントした statfs の avail(base には quota が無いので pool 全体の空きを映す。
// branch volume は default_storage_quota で avail が頭打ちになるため使わない)。
func (b *Backend) PoolCapacity(ctx context.Context) (used, total int64, err error) {
	out, err := b.api.DescribeFileSystems(ctx, &awsfsx.DescribeFileSystemsInput{
		FileSystemIds: []string{b.cfg.FileSystemID},
	})
	if err != nil {
		return 0, 0, fmt.Errorf("describe filesystem: %w", err)
	}
	if len(out.FileSystems) == 0 || out.FileSystems[0].StorageCapacity == nil {
		return 0, 0, fmt.Errorf("storage capacity of %s not found", b.cfg.FileSystemID)
	}
	total = int64(*out.FileSystems[0].StorageCapacity) * gib
	base, err := b.baseMount(ctx)
	if err != nil {
		return 0, 0, err
	}
	_, avail, err := b.statfs(base)
	if err != nil {
		return 0, 0, fmt.Errorf("statfs %s: %w", base, err)
	}
	if avail > total {
		avail = total
	}
	return total - avail, total, nil
}

const gib = int64(1) << 30

// baseMount は base volume を <MountRoot>/base にマウントしてパスを返す(冪等)。
// 容量の観測にだけ使い、mysqld は載せない。
func (b *Backend) baseMount(ctx context.Context) (string, error) {
	b.baseMu.Lock()
	defer b.baseMu.Unlock()
	target := filepath.Join(b.cfg.MountRoot, "base")
	if mounted, err := b.isMount(target); err == nil && mounted {
		return target, nil
	}
	out, err := b.api.DescribeVolumes(ctx, &awsfsx.DescribeVolumesInput{VolumeIds: []string{b.cfg.BaseVolumeID}})
	if err != nil {
		return "", fmt.Errorf("describe base volume: %w", err)
	}
	if len(out.Volumes) == 0 || out.Volumes[0].OpenZFSConfiguration == nil ||
		out.Volumes[0].OpenZFSConfiguration.VolumePath == nil {
		return "", fmt.Errorf("base volume %s has no volume path", b.cfg.BaseVolumeID)
	}
	if err := b.mount(ctx, b.cfg.DNSName+":"+*out.Volumes[0].OpenZFSConfiguration.VolumePath, target); err != nil {
		return "", err
	}
	return target, nil
}

// SetQuota は volume の StorageCapacityQuotaGiB(ZFS の quota 相当)を設定する(#278)。
// FSx の粒度は GiB なので切り上げる。bytes<=0 は -1 で解除。UpdateVolume は
// administrative action になるので完了を待つ。
func (b *Backend) SetQuota(ctx context.Context, vol storage.Volume, bytes int64) error {
	q := int32(-1)
	if bytes > 0 {
		q = int32((bytes + gib - 1) / gib)
	}
	_, err := b.api.UpdateVolume(ctx, &awsfsx.UpdateVolumeInput{
		VolumeId:             &vol.Dataset,
		OpenZFSConfiguration: &types.UpdateOpenZFSVolumeConfiguration{StorageCapacityQuotaGiB: &q},
	})
	if err != nil {
		return fmt.Errorf("fsx update-volume quota: %w", err)
	}
	return b.waitVolumeReady(ctx, vol.Dataset)
}

func strPtr(s string) *string { return &s }

// DeleteBaselineSnapshot は base volume 上の snapshot(ARN)を削除する(baseline GC /
// baseline delete、#278)。DeleteSnapshot は SnapshotId を取るので ARN から引き直し、
// 消えるまで待つ(clone が参照中なら FSx 側が拒否し、そのエラーを返す)。
func (b *Backend) DeleteBaselineSnapshot(ctx context.Context, snap storage.SnapshotRef) error {
	out, err := b.api.DescribeSnapshots(ctx, &awsfsx.DescribeSnapshotsInput{
		Filters: []types.SnapshotFilter{{
			Name:   types.SnapshotFilterNameVolumeId,
			Values: []string{b.cfg.BaseVolumeID},
		}},
	})
	if err != nil {
		return err
	}
	var id string
	for _, s := range out.Snapshots {
		if s.ResourceARN != nil && *s.ResourceARN == string(snap) && s.SnapshotId != nil {
			id = *s.SnapshotId
			break
		}
	}
	if id == "" {
		return fmt.Errorf("snapshot %s not found on %s", snap, b.cfg.BaseVolumeID)
	}
	if _, err := b.api.DeleteSnapshot(ctx, &awsfsx.DeleteSnapshotInput{SnapshotId: &id}); err != nil {
		return fmt.Errorf("fsx delete-snapshot: %w", err)
	}
	for {
		ds, err := b.api.DescribeSnapshots(ctx, &awsfsx.DescribeSnapshotsInput{SnapshotIds: []string{id}})
		if err != nil {
			var nf *types.SnapshotNotFound
			if errors.As(err, &nf) {
				return nil
			}
			return err
		}
		if len(ds.Snapshots) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.cfg.PollInterval):
		}
	}
}

// ListBranchVolumes: fsx の branch volume 列挙は DescribeVolumes で可能。
// sashiki-branch タグから branch 名を復元する(reconcile / orphan 検出用)。
func (b *Backend) ListBranchVolumes(ctx context.Context) ([]string, error) {
	vols, err := b.listAllVolumes(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var names []string
	for _, v := range vols {
		if v.Name == nil {
			continue
		}
		name := genSuffix.ReplaceAllString(*v.Name, "")
		if name == "base" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names, nil
}
