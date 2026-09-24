package fsxzfs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/fsx/types"
	"github.com/rikukadev/sashiki/internal/storage"
)

// #278: FSx でも baseline GC / delete が snapshot を消せる。ARN → SnapshotId を引き、
// DescribeSnapshots が not-found を返すまで待つ。
func TestDeleteBaselineSnapshotResolvesARN(t *testing.T) {
	m := newMockAPI()
	m.extraSnaps = []string{"baseline-old"}
	b := newTestBackend(m)
	if err := b.DeleteBaselineSnapshot(context.Background(), "arn:aws:fsx:::snapshot/baseline-old"); err != nil {
		t.Fatal(err)
	}
	if !m.deletedSnaps["fsvolsnap-baseline-old"] {
		t.Error("DeleteSnapshot should be called with the resolved snapshot id")
	}
	refs, _ := b.ListSnapshots(context.Background())
	for _, r := range refs {
		if strings.HasSuffix(string(r), "baseline-old") {
			t.Error("deleted snapshot must not be listed any more")
		}
	}
	if err := b.DeleteBaselineSnapshot(context.Background(), "arn:aws:fsx:::snapshot/nope"); err == nil {
		t.Error("unknown ARN should be an error, not a silent no-op")
	}
}

// #278: default_storage_quota は FSx では StorageCapacityQuotaGiB(GiB 切り上げ)。0 で解除(-1)。
func TestSetQuotaRoundsUpToGiB(t *testing.T) {
	m := newMockAPI()
	b := newTestBackend(m)
	vol, err := b.Clone(context.Background(), "arn:snap", "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SetQuota(context.Background(), vol, 3*gib/2); err != nil {
		t.Fatal(err)
	}
	if got := m.quotaGiB[vol.Dataset]; got != 2 {
		t.Errorf("1.5GiB quota should round up to 2 GiB, got %d", got)
	}
	if err := b.SetQuota(context.Background(), vol, 0); err != nil {
		t.Fatal(err)
	}
	if got := m.quotaGiB[vol.Dataset]; got != -1 {
		t.Errorf("quota 0 should unset (-1), got %d", got)
	}
}

// #278: 使用量は NFS マウントの statfs から。未マウントなら -1(不明)で 0 を実値に見せない。
func TestUsedBytesFromStatfs(t *testing.T) {
	b := newTestBackend(newMockAPI())
	b.isMount = func(path string) (bool, error) { return strings.HasSuffix(path, "/pr-1-mounted"), nil }
	b.statfs = func(path string) (int64, int64, error) { return 123 * 1024, 5 * gib, nil }
	used, err := b.UsedBytes(context.Background(), storage.Volume{Path: "/mnt/sashiki/pr-1-mounted"})
	if err != nil || used != 123*1024 {
		t.Errorf("used = %d, %v; want 123KiB", used, err)
	}
	used, err = b.UsedBytes(context.Background(), storage.Volume{Path: "/mnt/sashiki/pr-2-gone"})
	if err != nil || used != -1 {
		t.Errorf("unmounted volume should report -1 (unknown), got %d, %v", used, err)
	}
}

// #278: PoolCapacity は filesystem の StorageCapacity と、base volume を遅延マウントした
// statfs の avail から出す(watermark admission / capacity / doctor が使う)。
func TestPoolCapacityMountsBaseOnce(t *testing.T) {
	m := newMockAPI()
	m.storageGiB = 64
	base := "fsvol-base"
	m.volumes[base] = newMockVolume(base, "base", "/fsx/base")
	b := newTestBackend(m)
	mounted := map[string]bool{}
	mounts := 0
	b.mount = func(ctx context.Context, source, target string) error {
		mounts++
		if source != "fs-1.fsx.test:/fsx/base" {
			t.Errorf("should mount the base volume path, got %s", source)
		}
		mounted[target] = true
		return nil
	}
	b.isMount = func(path string) (bool, error) { return mounted[path], nil }
	b.statfs = func(path string) (int64, int64, error) {
		if !mounted[path] {
			return 0, 0, errors.New("not mounted")
		}
		return 0, 48 * gib, nil
	}
	for i := 0; i < 2; i++ {
		used, total, err := b.PoolCapacity(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if total != 64*gib || used != 16*gib {
			t.Errorf("used/total = %d/%d, want 16GiB/64GiB", used, total)
		}
	}
	if mounts != 1 {
		t.Errorf("base should be mounted once, got %d", mounts)
	}
}

func newMockVolume(id, name, path string) *types.Volume {
	return &types.Volume{
		VolumeId: strp(id), Name: strp(name), Lifecycle: types.VolumeLifecycleAvailable,
		OpenZFSConfiguration: &types.OpenZFSVolumeConfiguration{VolumePath: strp(path)},
	}
}
