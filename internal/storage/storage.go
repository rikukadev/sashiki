// Package storage はブランチの実体(ボリューム)を作る・消す・巻き戻すための
// バックエンド抽象。zfs(ローカル)と fsx(AWS API)で操作レイテンシが2桁違うため、
// コアは速度を仮定せず Capabilities を見て挙動を切り替える。
package storage

import (
	"context"
	"time"
)

// Capabilities はバックエンドの性格の宣言。コアの挙動切り替えに使う。
type Capabilities struct {
	FastRollback  bool          // ebs-zfs: true / fsx-zfs: false
	TypicalCreate time.Duration // ebs-zfs: ~2s / fsx-zfs: ~70s
	// ClonesAreDistinct: Clone(name) が呼ぶたび別実体を作り、同名 branch の
	// 新旧が共存できるか。fsx-zfs=true(世代サフィックス)、ebs-zfs=false(固定名)。
	// false のバックエンドの recreate は「旧を退避→新 clone→旧削除」になる。
	ClonesAreDistinct bool
}

// SnapshotRef はスナップショットの完全修飾名(例: dbpool/base@baseline-20260901)。
type SnapshotRef string

// Volume はブランチ1つ分のデータセット/ボリューム。
type Volume struct {
	Name    string // ブランチ名
	Dataset string // 例: dbpool/branches/pr-123 / fsvol-xxxx
	Path    string // マウント済みパス(この下に data/ がある)
}

// JobID は非同期ジョブの識別子。
type JobID string

// JobStatus は非同期ジョブの状態。
type JobStatus string

const (
	JobRunning   JobStatus = "running"
	JobCompleted JobStatus = "completed"
	JobFailed    JobStatus = "failed"
)

// Storage はバックエンドが実装するインターフェース。
type Storage interface {
	Capabilities() Capabilities

	// Clone はベースラインの snapshot から新しいブランチ用ボリュームを作り、
	// マウント済みの状態で返す。完了まで待つ(fsx なら AdministrativeActions を監視)。
	Clone(ctx context.Context, baseline SnapshotRef, name string) (Volume, error)

	// SnapshotInit はブランチ作成直後(mysqld 起動前)の状態を保存する(reset 用)。
	// FastRollback == false のバックエンドは NopSnapshot を返してよい。
	SnapshotInit(ctx context.Context, vol Volume) (SnapshotRef, error)

	// Rollback は FastRollback == true のときだけ呼ばれる。
	Rollback(ctx context.Context, vol Volume, snap SnapshotRef) error

	// DeleteAsync は削除ジョブを投入して即返る。完了は Poll で確認する。
	DeleteAsync(ctx context.Context, vol Volume) (JobID, error)
	Poll(ctx context.Context, job JobID) (JobStatus, error)

	// SnapshotBase はベースライン更新用。現在の base から新スナップショットを取得する。
	SnapshotBase(ctx context.Context, tag string) (SnapshotRef, error)
	ListSnapshots(ctx context.Context) ([]SnapshotRef, error)

	// UsedBytes はボリュームの現在のディスク消費(CoW差分)。
	UsedBytes(ctx context.Context, vol Volume) (int64, error)
}

// Renamer は ClonesAreDistinct=false のバックエンドが実装する。recreate の
// 退避スワップに使う(旧 volume を一時名へ、失敗時に戻す)。
type Renamer interface {
	Rename(ctx context.Context, vol Volume, newName string) (Volume, error)
}

// CapacityReporter は pool の使用量を報告できるバックエンド(watermark 用)。
type CapacityReporter interface {
	PoolCapacity(ctx context.Context) (used, total int64, err error)
}

// PoolStatusChecker は pool の健全性(DEGRADED/FAULTED 等)を報告できる
// バックエンド(`zpool status -x` 相当、doctor 用 #88)。
type PoolStatusChecker interface {
	PoolStatus(ctx context.Context) (healthy bool, detail string, err error)
}

// BranchPromoter は既存ブランチの datadir を新しい baseline snapshot に昇格できる
// バックエンド(`baseline promote`、#129)。branch でマイグレーション済みの状態を
// そのまま次の baseline にする(git の branch→main 相当)。呼び出し側は事前に
// branch の mysqld を graceful stop 済みであることを保証する。
type BranchPromoter interface {
	PromoteBranch(ctx context.Context, branch Volume, tag string) (SnapshotRef, error)
}

// LogicalSizer は volume の logical(referenced)サイズを報告できるバックエンド。
// CoW では Logical(482GiB)と Private delta(18MiB)が大きく乖離する(仕様 14-4)。
type LogicalSizer interface {
	LogicalBytes(ctx context.Context, vol Volume) (int64, error)
}

// VolumeLister は branch volume を列挙できるバックエンド(reconciliation / orphan GC 用)。
type VolumeLister interface {
	ListBranchVolumes(ctx context.Context) ([]string, error) // branch 名の一覧
}

// Quota は branch volume に容量上限(refquota)を設定できるバックエンド(#85)。
// 1 ブランチの暴走(migration 失敗・大量 UPDATE)が pool を食い尽くすのを防ぐ。
// bytes<=0 は上限解除。fsx-zfs は StorageCapacityQuotaGiB(GiB 切り上げ)で同じ意味を実現する(#278)。
type Quota interface {
	SetQuota(ctx context.Context, vol Volume, bytes int64) error
}

// NopSnapshot は SnapshotInit を持たないバックエンドが返す番兵値。
const NopSnapshot SnapshotRef = ""
