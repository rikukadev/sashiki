package fsxzfs

import (
	"context"
	"fmt"
	"testing"
	"time"

	awsfsx "github.com/aws/aws-sdk-go-v2/service/fsx"
	"github.com/aws/aws-sdk-go-v2/service/fsx/types"
	"github.com/rikukadev/sashiki/internal/storage"
)

type mockAPI struct {
	volumes       map[string]*types.Volume // by id
	order         []string                 // 挿入順(ページネーションの決定性のため)
	createCalls   int
	describeCalls int
	pageSize      int
	// waitVolumeReady が最初の N 回 IN_PROGRESS を返すシミュレーション
	pendingUntil int
	// #278: snapshot 削除 / quota / 容量
	extraSnaps   []string        // base volume 上に "baseline" 以外で存在する snapshot 名
	deletedSnaps map[string]bool // DeleteSnapshot 済み(id)
	quotaGiB     map[string]int32
	storageGiB   int32
}

func newMockAPI() *mockAPI {
	return &mockAPI{volumes: map[string]*types.Volume{}, deletedSnaps: map[string]bool{}, quotaGiB: map[string]int32{}, storageGiB: 64}
}

func (m *mockAPI) DeleteSnapshot(ctx context.Context, in *awsfsx.DeleteSnapshotInput, _ ...func(*awsfsx.Options)) (*awsfsx.DeleteSnapshotOutput, error) {
	m.deletedSnaps[*in.SnapshotId] = true
	return &awsfsx.DeleteSnapshotOutput{SnapshotId: in.SnapshotId, Lifecycle: types.SnapshotLifecycleDeleting}, nil
}

func (m *mockAPI) UpdateVolume(ctx context.Context, in *awsfsx.UpdateVolumeInput, _ ...func(*awsfsx.Options)) (*awsfsx.UpdateVolumeOutput, error) {
	if in.OpenZFSConfiguration != nil && in.OpenZFSConfiguration.StorageCapacityQuotaGiB != nil {
		m.quotaGiB[*in.VolumeId] = *in.OpenZFSConfiguration.StorageCapacityQuotaGiB
	}
	v, ok := m.volumes[*in.VolumeId]
	if !ok {
		return nil, &types.VolumeNotFound{}
	}
	return &awsfsx.UpdateVolumeOutput{Volume: v}, nil
}

func strp(s string) *string { return &s }

func (m *mockAPI) CreateVolume(ctx context.Context, in *awsfsx.CreateVolumeInput, _ ...func(*awsfsx.Options)) (*awsfsx.CreateVolumeOutput, error) {
	m.createCalls++
	id := "fsvol-mock" + *in.Name
	now := time.Now()
	v := &types.Volume{
		VolumeId:     &id,
		Name:         in.Name,
		Lifecycle:    types.VolumeLifecycleAvailable,
		CreationTime: &now,
		Tags:         in.Tags,
		OpenZFSConfiguration: &types.OpenZFSVolumeConfiguration{
			VolumePath: strp("/fsx/" + *in.Name),
		},
	}
	m.volumes[id] = v
	m.order = append(m.order, id)
	return &awsfsx.CreateVolumeOutput{Volume: v}, nil
}

func (m *mockAPI) DeleteVolume(ctx context.Context, in *awsfsx.DeleteVolumeInput, _ ...func(*awsfsx.Options)) (*awsfsx.DeleteVolumeOutput, error) {
	delete(m.volumes, *in.VolumeId)
	for i, id := range m.order {
		if id == *in.VolumeId {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	return &awsfsx.DeleteVolumeOutput{}, nil
}

func (m *mockAPI) DescribeVolumes(ctx context.Context, in *awsfsx.DescribeVolumesInput, _ ...func(*awsfsx.Options)) (*awsfsx.DescribeVolumesOutput, error) {
	m.describeCalls++
	// pageSize > 0 ならフィルタ検索を 1 件ずつページングする(ページネーション検証用)
	if m.pageSize > 0 && len(in.VolumeIds) == 0 {
		var all []types.Volume
		for _, id := range m.order {
			all = append(all, *m.volumes[id])
		}
		start := 0
		if in.NextToken != nil {
			_, _ = fmt.Sscanf(*in.NextToken, "%d", &start)
		}
		end := start + m.pageSize
		if end > len(all) {
			end = len(all)
		}
		out := &awsfsx.DescribeVolumesOutput{Volumes: all[start:end]}
		if end < len(all) {
			tok := fmt.Sprintf("%d", end)
			out.NextToken = &tok
		}
		return out, nil
	}
	var out []types.Volume
	if len(in.VolumeIds) > 0 {
		for _, id := range in.VolumeIds {
			if v, ok := m.volumes[id]; ok {
				vv := *v
				// pendingUntil 回目までは admin action IN_PROGRESS
				if m.describeCalls <= m.pendingUntil {
					vv.AdministrativeActions = []types.AdministrativeAction{{
						AdministrativeActionType: types.AdministrativeActionTypeVolumeInitializeWithSnapshot,
						Status:                   types.StatusInProgress,
					}}
				}
				out = append(out, vv)
			}
		}
	} else {
		for _, id := range m.order {
			out = append(out, *m.volumes[id])
		}
	}
	return &awsfsx.DescribeVolumesOutput{Volumes: out}, nil
}

func (m *mockAPI) CreateSnapshot(ctx context.Context, in *awsfsx.CreateSnapshotInput, _ ...func(*awsfsx.Options)) (*awsfsx.CreateSnapshotOutput, error) {
	id := "fsvolsnap-" + *in.Name
	arn := "arn:aws:fsx:::snapshot/" + *in.Name
	return &awsfsx.CreateSnapshotOutput{Snapshot: &types.Snapshot{
		SnapshotId: &id, Name: in.Name, ResourceARN: &arn,
		Lifecycle: types.SnapshotLifecycleAvailable,
	}}, nil
}

func (m *mockAPI) DescribeSnapshots(ctx context.Context, in *awsfsx.DescribeSnapshotsInput, _ ...func(*awsfsx.Options)) (*awsfsx.DescribeSnapshotsOutput, error) {
	if len(in.SnapshotIds) > 0 {
		if m.deletedSnaps[in.SnapshotIds[0]] {
			return nil, &types.SnapshotNotFound{}
		}
		name := in.SnapshotIds[0][len("fsvolsnap-"):]
		arn := "arn:aws:fsx:::snapshot/" + name
		return &awsfsx.DescribeSnapshotsOutput{Snapshots: []types.Snapshot{{
			SnapshotId: &in.SnapshotIds[0], Name: &name, ResourceARN: &arn,
			Lifecycle: types.SnapshotLifecycleAvailable,
		}}}, nil
	}
	name := "baseline"
	arn := "arn:aws:fsx:::snapshot/baseline"
	out := &awsfsx.DescribeSnapshotsOutput{Snapshots: []types.Snapshot{{
		Name: &name, ResourceARN: &arn, Lifecycle: types.SnapshotLifecycleAvailable,
	}}}
	for _, n := range m.extraSnaps {
		id, nn, a := "fsvolsnap-"+n, n, "arn:aws:fsx:::snapshot/"+n
		if m.deletedSnaps[id] {
			continue
		}
		out.Snapshots = append(out.Snapshots, types.Snapshot{
			SnapshotId: &id, Name: &nn, ResourceARN: &a, Lifecycle: types.SnapshotLifecycleAvailable,
		})
	}
	return out, nil
}

func (m *mockAPI) DescribeFileSystems(ctx context.Context, in *awsfsx.DescribeFileSystemsInput, _ ...func(*awsfsx.Options)) (*awsfsx.DescribeFileSystemsOutput, error) {
	root := "fsvol-root-discovered"
	cap := m.storageGiB
	return &awsfsx.DescribeFileSystemsOutput{FileSystems: []types.FileSystem{{
		StorageCapacity:      &cap,
		OpenZFSConfiguration: &types.OpenZFSFileSystemConfiguration{RootVolumeId: &root},
	}}}, nil
}

func newTestBackend(m *mockAPI) *Backend {
	b := New(Config{
		FileSystemID:     "fs-1",
		BaseVolumeID:     "fsvol-base",
		ParentVolumeID:   "fsvol-root",
		BaselineSnapshot: "baseline",
		DNSName:          "fs-1.fsx.test",
		MountRoot:        "/mnt/sashiki",
		PollInterval:     time.Millisecond,
	}, m)
	b.mount = func(ctx context.Context, source, target string) error { return nil }
	b.umount = func(ctx context.Context, target string) error { return nil }
	return b
}

func TestCloneWaitsForAdminActions(t *testing.T) {
	m := newMockAPI()
	m.pendingUntil = 3 // 3 回目の Describe までは IN_PROGRESS
	b := newTestBackend(m)

	vol, err := b.Clone(context.Background(), "arn:snap", "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if vol.Name != "pr-1" {
		t.Errorf("name = %s", vol.Name)
	}
	if m.describeCalls < 4 {
		t.Errorf("should poll until admin actions complete: %d describes", m.describeCalls)
	}
	// 世代サフィックス付きの FSx 名でマウントパスが決まる
	if vol.Path == "/mnt/sashiki/pr-1" {
		t.Errorf("path should include generation suffix: %s", vol.Path)
	}
}

func TestResolveVolumePicksNewestGeneration(t *testing.T) {
	m := newMockAPI()
	b := newTestBackend(m)
	// 2 世代作る(reset の付け替え中を再現)
	v1, err := b.Clone(context.Background(), "arn:snap", "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	v2, err := b.Clone(context.Background(), "arn:snap", "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.ResolveVolume(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Dataset != v2.Dataset {
		t.Errorf("resolved %s, want newest %s (old %s)", got.Dataset, v2.Dataset, v1.Dataset)
	}
}

func TestDeleteAsyncAndPoll(t *testing.T) {
	m := newMockAPI()
	b := newTestBackend(m)
	vol, _ := b.Clone(context.Background(), "arn:snap", "pr-1")
	job, err := b.DeleteAsync(context.Background(), vol)
	if err != nil {
		t.Fatal(err)
	}
	st, err := b.Poll(context.Background(), job)
	if err != nil || st != storage.JobCompleted {
		t.Errorf("poll = %v, %v", st, err)
	}
}

func TestCapabilitiesDeclareSlowControlPlane(t *testing.T) {
	b := newTestBackend(newMockAPI())
	c := b.Capabilities()
	if c.FastRollback {
		t.Error("fsx must not claim FastRollback")
	}
	if c.TypicalCreate < 30*time.Second {
		t.Error("TypicalCreate should reflect measured ~70s")
	}
}

func TestResolveVolumePaginates(t *testing.T) {
	m := newMockAPI()
	m.pageSize = 1
	b := newTestBackend(m)
	// 3 ブランチ分作って、ページを跨いでも解決できること
	for _, n := range []string{"pr-a", "pr-b", "pr-c"} {
		if _, err := b.Clone(context.Background(), "arn:snap", n); err != nil {
			t.Fatal(err)
		}
	}
	got, err := b.ResolveVolume(context.Background(), "pr-c")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "pr-c" {
		t.Errorf("resolved %q", got.Name)
	}
}

func TestCloneMountFailureCleansUpVolume(t *testing.T) {
	m := newMockAPI()
	b := newTestBackend(m)
	b.mount = func(ctx context.Context, source, target string) error {
		return fmt.Errorf("mount failed")
	}
	if _, err := b.Clone(context.Background(), "arn:snap", "pr-1"); err == nil {
		t.Fatal("want error")
	}
	if len(m.volumes) != 0 {
		t.Errorf("volume should be cleaned up on mount failure: %d left", len(m.volumes))
	}
}
