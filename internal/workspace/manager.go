// Package branch はブランチのライフサイクル(create / reset / delete / list)を
// 司るコア。storage と engine はインターフェースで受け、速度の仮定を持たない。
package workspace

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rikukadev/sashiki/internal/engine"
	"github.com/rikukadev/sashiki/internal/hooks"
	"github.com/rikukadev/sashiki/internal/state"
	"github.com/rikukadev/sashiki/internal/storage"
)

// ReservedPrefix で始まる名前は sashiki 内部予約(validate 用の一時 branch 等)。
// name_pattern(^[a-z0-9-]+$)により利用者は作成できない。reconcile / orphan GC は
// 予約名を対象外にする。
const ReservedPrefix = "_"

// IsReserved は内部予約名かどうか。
func IsReserved(name string) bool {
	return len(name) > 0 && name[0] == ReservedPrefix[0]
}

// エラー種別(API 層で HTTP ステータスに写像する)。
var (
	ErrInvalidName      = errors.New("invalid branch name")
	ErrExists           = errors.New("branch already exists")
	ErrNotFound         = state.ErrNotFound
	ErrLimitReached     = errors.New("branch limit reached")
	ErrNoFreePort       = errors.New("no free port in range")
	ErrUnknownProfile   = errors.New("unknown profile")
	ErrBaselineNotFound = errors.New("baseline not found")
	// ErrHookFailed / ErrEngineFailed は failStage の分類(#89)。errors.Is で判定する。
	ErrHookFailed   = errors.New("hook failed")
	ErrEngineFailed = errors.New("engine failed")
	// ErrPreconditionFailed はポリシー未達(未 masked / 未 validated)の操作(#89)。
	ErrPreconditionFailed = errors.New("precondition failed")
)

// ProfilePolicy は profile ごとの idle lifecycle(仕様 11-3)。0 のフィールドは
// global(Config.IdleStopAfter/DeleteAfterIdle)にフォールバックする。
type ProfilePolicy struct {
	IdleStopAfter   time.Duration
	DeleteAfterIdle time.Duration
}

// BaselineProvider は現在のベースライン snapshot を返す。
type BaselineProvider interface {
	CurrentBaseline() storage.SnapshotRef
}

// Config は Manager の設定。
type Config struct {
	NamePattern string
	MaxBranches int
	PortLow     int
	PortHigh    int
	EngineType  string
	// MysqldBin / MysqlExtraCnf は baseline refresh の一時 mysqld に使う(#127)。
	// 空なら /usr/sbin/mysqld / 既定 my.cnf。
	MysqldBin     string
	PgBinDir      string
	PgRunUser     string
	MysqlExtraCnf string
	StateDir      string // hook 用の作業ディレクトリの親(/var/lib/sashiki/branches)

	// LazyCreate: プロキシに未知のブランチ名で接続が来たとき自動作成する。
	// バックエンドの TypicalCreate が LazyMaxWait を超える場合は無効(仕様 15-3)。
	LazyCreate  bool
	LazyMaxWait time.Duration

	// リーパー(reaper.go)。profile 未指定 branch と、profile で未設定の
	// フィールドのフォールバックに使う。
	IdleStopAfter   time.Duration // 0 = アイドル停止しない
	DeleteAfterIdle time.Duration // 0 = 自動削除しない

	// OperationRetention: 完了/失敗した operation を finished 後この期間で削除する
	// (operations テーブルの無限成長を防ぐ、#83)。0 = 削除しない。
	OperationRetention time.Duration

	// profile(仕様 11-3): 名前 → idle lifecycle。branch は create 時に
	// profile を1つ持ち、reaper はその profile の閾値を使う。空なら全 branch が
	// global(上の IdleStopAfter/DeleteAfterIdle)を使う。
	Profiles       map[string]ProfilePolicy
	DefaultProfile string

	// ActiveConns はブランチの現在の接続数(プロキシが提供)。nil なら常に 0 扱い。
	// last_conn_at は接続開始時刻しか進まないため、長寿命接続を張ったまま
	// 使用中のブランチをリーパーが停止・削除しないための判定に使う。
	ActiveConns func(name string) int

	// メモリ admission(仕様 14-1): create/wake/recreate/lazy に適用。
	// OOM killer は新しいブランチではなく既存の無関係な mysqld を殺す(PoC 実測)ため、
	// 事後の監視ではなく事前の拒否で守る。AvailableMem が nil なら無効(best-effort)。
	AvailableMem        func() (int64, error)
	ExpectedRSSBytes    int64 // mysqld 1 本の想定 RSS。0 なら BufferPoolBytes+300MB
	MemoryHeadroomBytes int64 // 0 なら ExpectedRSS を headroom に使う
	BufferPoolBytes     int64
	MaxRunning          int // 同時稼働 mysqld 数の上限(volume 数の MaxBranches とは別)

	// storage watermark(仕様 14-2): pool 使用率が critical を超えたら
	// create/wake/recreate を拒否。high は warning(メトリクス)。
	HighWatermark     float64 // 0.0-1.0(0=無効)
	CriticalWatermark float64

	// baseline GC(仕様 12-5, #86)。keep_last=直近 N を残す、retention=期間で残す。
	BaselineKeepLast  int
	BaselineRetention time.Duration

	// DefaultStorageQuotaBytes: branch ごとの refquota(#85)。0 = 無制限。
	// clone 直後に適用し、1 ブランチの暴走が pool を食い尽くすのを防ぐ。
	// backend が storage.Quota 未実装(fsx)なら適用されない。
	DefaultStorageQuotaBytes int64
}

// applyQuota は refquota を branch volume に設定する(#85)。quota 未設定または
// backend が Quota 未対応なら何もしない。
func (m *Manager) applyQuota(ctx context.Context, vol storage.Volume) error {
	if m.cfg.DefaultStorageQuotaBytes <= 0 {
		return nil
	}
	q, ok := m.st.(storage.Quota)
	if !ok {
		return nil
	}
	return q.SetQuota(ctx, vol, m.cfg.DefaultStorageQuotaBytes)
}

// BaselineGCConfig は config 由来の GC 既定を返す(API/CLI が override する)。
func (m *Manager) BaselineGCConfig() GCConfig {
	return GCConfig{KeepLast: m.cfg.BaselineKeepLast, Retention: m.cfg.BaselineRetention}
}

// Manager はブランチライフサイクルの実装。
type Manager struct {
	cfg      Config
	st       storage.Storage
	baseline BaselineProvider
	eng      engine.Engine
	hooks    *hooks.Runner
	db       *state.DB
	nameRe   *regexp.Regexp

	// ブランチ名ごとの直列化(同名の同時 create/delete を防ぐ)
	mu    sync.Mutex
	locks map[string]*sync.Mutex

	// TouchConn のスロットリング
	touchMu   sync.Mutex
	lastTouch map[string]time.Time

	// #41: connpoll が最後に観測した branch ごとのクライアント接続数(proxy 非依存)。
	// activeConns にマージして reaper の使用中判定に使う。
	pollMu    sync.Mutex
	pollConns map[string]int
	// connpoll と reaper の回収直前チェックを直列化する。両者が同時にDBへ
	// 接続すると、監視接続同士をクライアント接続として数えてしまう。
	connCheckMu sync.Mutex

	// baseline publish ポリシー(#38)
	baselinePolicy RefreshConfig
	// baseline の set / GC / publish を直列化する(Fix 3)。
	baselineMu sync.Mutex
}

// SetBaselinePolicy は refresh の publish ポリシーを設定する(sashikid 起動時)。
func (m *Manager) SetBaselinePolicy(rc RefreshConfig) {
	m.baselinePolicy = rc
}

// New は Manager を作る。
func New(cfg Config, st storage.Storage, bp BaselineProvider, eng engine.Engine, hr *hooks.Runner, db *state.DB) (*Manager, error) {
	re, err := regexp.Compile(cfg.NamePattern)
	if err != nil {
		return nil, fmt.Errorf("name pattern: %w", err)
	}
	return &Manager{
		cfg: cfg, st: st, baseline: bp, eng: eng, hooks: hr, db: db,
		nameRe: re, locks: map[string]*sync.Mutex{},
		lastTouch: map[string]time.Time{},
		pollConns: map[string]int{},
	}, nil
}

func (m *Manager) lock(name string) func() {
	m.mu.Lock()
	l, ok := m.locks[name]
	if !ok {
		l = &sync.Mutex{}
		m.locks[name] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Info は API に返すブランチ情報。
type Info struct {
	state.Branch
	UsedBytes    int64 // Private delta(zfs used)。CoW 差分
	LogicalBytes int64 // Logical(zfs referenced)
	HookStatus   map[string]string
	// Stale: origin snapshot が current baseline と異なる(= baseline が更新された後)。
	// reset は origin(古い baseline)に戻すため、最新化したいなら recreate を使う(#130)。
	Stale bool
	// BackingBaselines: このブランチの dataset 上に実体を持つ登録済み baseline
	// (promote 元)。空でなければ reset / recreate / delete は拒否される(#179 / #289)。
	BackingBaselines []string
}

func (m *Manager) instance(b state.Branch, vol storage.Volume) engine.Instance {
	return engine.Instance{Branch: b.Name, DataDir: vol.Path + "/data", Port: b.Port}
}

func (m *Manager) hookEnv(b state.Branch, vol storage.Volume) hooks.Env {
	// origin baseline の schema_revision(baselines テーブルに登録があれば)。
	// on-create で migration リポジトリの ref 決定などに使う(#81)。
	schemaRev := ""
	if row, err := m.db.GetBaseline(b.OriginSnapshot); err == nil {
		schemaRev = row.Prov.SchemaRevision
	}
	return hooks.Env{
		Branch:         b.Name,
		Port:           b.Port,
		Socket:         "/tmp/mysql-" + b.Name + ".sock",
		DataDir:        vol.Path + "/data",
		EngineType:     m.cfg.EngineType,
		AdminUser:      "root",
		OriginSnapshot: b.OriginSnapshot,
		StateDir:       m.cfg.StateDir + "/" + b.Name,
		// provenance(仕様 11-2): core は解釈せず state.db の値を素通しする。
		SourceJSON:             b.Source,
		Owner:                  b.Owner,
		Purpose:                b.Purpose,
		Profile:                b.Profile,
		BaselineSchemaRevision: schemaRev,
	}
}

func (m *Manager) allocPort(requested int) (int, error) {
	used, err := m.db.UsedPorts()
	if err != nil {
		return 0, err
	}
	if requested != 0 {
		if requested < m.cfg.PortLow || requested > m.cfg.PortHigh {
			return 0, fmt.Errorf("port %d out of range [%d, %d]", requested, m.cfg.PortLow, m.cfg.PortHigh)
		}
		if used[requested] {
			return 0, fmt.Errorf("port %d already in use", requested)
		}
		return requested, nil
	}
	for p := m.cfg.PortLow; p <= m.cfg.PortHigh; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, ErrNoFreePort
}

// Create はブランチを作成する(仕様 14-1 / 14-2)。
//
// hook がある場合の順序: clone → 起動 → ready → on-create → 正常終了 →
// @init snapshot → 再起動。hook の結果(マイグレーション適用済み)を
// reset の戻り先にしつつ、@init を必ずクリーンな状態でのみ取得するため。
// hook がない場合: clone → @init snapshot → 起動(最速経路)。
// Create は provenance なしで作成する(後方互換)。
func (m *Manager) Create(ctx context.Context, name string, port int) (Info, error) {
	return m.CreateWithMeta(ctx, name, port, state.Meta{})
}

// ValidName は branch 名が name_pattern に合致するかを返す(API の同期事前検証用)。
func (m *Manager) ValidName(name string) bool { return m.nameRe.MatchString(name) }

// CreateWithMeta は current baseline から provenance 付きで branch を作成する(仕様 11-2)。
func (m *Manager) CreateWithMeta(ctx context.Context, name string, port int, meta state.Meta) (Info, error) {
	return m.CreateWithMetaFrom(ctx, name, port, meta, "")
}

// CreateWithMetaFrom は baseline を指定して branch を作成する(#82: create --baseline)。
// baseline が空なら current を使う。指定した baseline が未登録なら ErrBaselineNotFound。
func (m *Manager) CreateWithMetaFrom(ctx context.Context, name string, port int, meta state.Meta, baseline string) (Info, error) {
	if !m.nameRe.MatchString(name) {
		return Info{}, ErrInvalidName
	}
	// profile を確定(空→DefaultProfile、未知→ErrUnknownProfile)して永続化する。
	pname, err := m.resolveProfileName(meta.Profile)
	if err != nil {
		return Info{}, err
	}
	meta.Profile = pname
	unlock := m.lock(name)
	defer unlock()

	if _, err := m.db.GetBranch(name); err == nil {
		return Info{}, ErrExists
	}
	all, err := m.db.ListBranches()
	if err != nil {
		return Info{}, err
	}
	if m.cfg.MaxBranches > 0 && len(all) >= m.cfg.MaxBranches {
		return Info{}, ErrLimitReached
	}
	p, err := m.allocPort(port)
	if err != nil {
		return Info{}, err
	}
	if err := m.admitMemory("create"); err != nil {
		return Info{}, err
	}
	if err := m.admitStorage(ctx, "create"); err != nil {
		return Info{}, err
	}

	origin := m.currentBaseline()
	if baseline != "" {
		if _, gerr := m.db.GetBaseline(baseline); gerr != nil {
			return Info{}, fmt.Errorf("%w: %s", ErrBaselineNotFound, baseline)
		}
		origin = storage.SnapshotRef(baseline)
	}
	if err := m.db.CreateBranch(name, p, string(origin)); err != nil {
		return Info{}, err
	}
	if meta != (state.Meta{}) {
		_ = m.db.SetMeta(name, meta)
	}
	failStage := func(stage string, cause error) (Info, error) {
		// error 状態+診断で残す(ログ確認のため自動削除しない)。仕様 11-1/14-2。
		_ = m.failOp(name, "create", stage, cause)
		// ステージを保ったまま返し、API 層が hook_failed / engine_error に分類
		// できるようにする(#89。従来のメッセージ文字列マッチを置き換える)。
		return Info{}, &stageErr{stage: stage, err: cause}
	}

	vol, err := m.st.Clone(ctx, origin, name)
	if err != nil {
		return failStage("clone", fmt.Errorf("clone: %w", err))
	}
	// clone 直後に refquota を適用(#85)。1 ブランチの暴走で pool を食い潰さない。
	if err := m.applyQuota(ctx, vol); err != nil {
		return failStage("quota", fmt.Errorf("set quota: %w", err))
	}
	b, _ := m.db.GetBranch(name)
	ins := m.instance(b, vol)

	if _, hasHook := m.hookExists(hooks.OnCreate); hasHook {
		if err := m.eng.Start(ctx, ins); err != nil {
			return failStage("engine-start", fmt.Errorf("engine start: %w", err))
		}
		if err := m.eng.WaitReady(ctx, ins); err != nil {
			return failStage("engine-ready", err)
		}
		if err := m.runHook(ctx, hooks.OnCreate, b, vol); err != nil {
			return failStage("hook", err)
		}
		// @init は必ず正常終了状態でのみ取得する(クラッシュリカバリ防止)。
		if err := m.eng.Stop(ctx, ins); err != nil {
			return failStage("engine-stop", fmt.Errorf("engine stop before @init: %w", err))
		}
	}
	initRef, err := m.st.SnapshotInit(ctx, vol)
	if err != nil {
		return failStage("snapshot", fmt.Errorf("snapshot @init: %w", err))
	}
	// 実体参照を記録(reset の戻り先と fsx 世代管理。仕様 19章 / #90)。
	// 失敗しても reset 側が <dataset>@init を組み立てるため非致命。
	_ = m.db.SetProvision(name, vol.Dataset, string(initRef))
	if err := m.eng.Start(ctx, ins); err != nil {
		return failStage("engine-start", fmt.Errorf("engine start: %w", err))
	}
	if err := m.eng.WaitReady(ctx, ins); err != nil {
		return failStage("engine-ready", err)
	}
	if err := m.db.SetState(name, state.StateRunning, ""); err != nil {
		return Info{}, err
	}
	return m.info(ctx, name)
}

// Reset はブランチを @init に巻き戻す。FastRollback バックエンド(zfs/apfs/reflink)
// は snapshot rollback、遅いバックエンド(fsx)は同じ origin からの「作り直し」で
// @init 相当を再現する(下の recreateFrom 分岐。docs/DECISIONS.md 参照)。
func (m *Manager) Reset(ctx context.Context, name string) (Info, error) {
	unlock := m.lock(name)
	defer unlock()

	b, err := m.db.GetBranch(name)
	if err != nil {
		return Info{}, err
	}
	// promote 済みブランチの保護(#289)。rollback / 作り直しのどちらも baseline
	// snapshot を失うので、状態を変える前に拒否する。
	vol, err := m.resolveVolume(ctx, b)
	if err != nil {
		return Info{}, err
	}
	if err := m.refuseIfBackingBaseline(name, vol, "reset"); err != nil {
		return Info{}, err
	}
	if !m.st.Capabilities().FastRollback {
		// 遅いバックエンド(fsx)の reset は @init 相当を「同じ origin から
		// 作り直し」で再現する。recreate と違い baseline は current でなく
		// branch の origin を使う。
		return m.recreateFrom(ctx, b, storage.SnapshotRef(b.OriginSnapshot), hooks.OnCreate)
	}
	if err := m.db.SetState(name, state.StateResetting, ""); err != nil {
		return Info{}, err
	}
	ins := m.instance(b, vol)

	// rollback で dirty state を捨てるため graceful は不要(Kill で高速化)。
	// PoC では reset 時間の大半が graceful shutdown だった。
	_ = m.eng.Kill(ctx, ins)
	// 記録済みの @init(仕様 19章 / #90)を優先し、無ければ従来どおり組み立てる
	initSnap := storage.SnapshotRef(vol.Dataset + "@init")
	if b.InitSnapshot != "" {
		initSnap = storage.SnapshotRef(b.InitSnapshot)
	}
	if err := m.st.Rollback(ctx, vol, initSnap); err != nil {
		_ = m.db.SetState(name, state.StateError, err.Error())
		return Info{}, fmt.Errorf("rollback: %w", err)
	}
	if err := m.eng.Start(ctx, ins); err != nil {
		_ = m.db.SetState(name, state.StateError, err.Error())
		return Info{}, err
	}
	if err := m.eng.WaitReady(ctx, ins); err != nil {
		_ = m.db.SetState(name, state.StateError, err.Error())
		return Info{}, err
	}
	// on-reset の失敗は記録して続行(仕様 14-2)。
	_ = m.runHook(ctx, hooks.OnReset, b, vol)
	if err := m.db.SetState(name, state.StateRunning, ""); err != nil {
		return Info{}, err
	}
	return m.info(ctx, name)
}

// Recreate は main が進んだ branch を最新 current baseline から作り直す。
// reset(同じ baseline の @init へ戻す)とは別操作。既存 branch の自動追従は
// しない(利用者が明示的に recreate する)。
func (m *Manager) Recreate(ctx context.Context, name string) (Info, error) {
	unlock := m.lock(name)
	defer unlock()

	b, err := m.db.GetBranch(name)
	if err != nil {
		return Info{}, err
	}
	// promote 済みブランチの保護(#289)。rename → destroy -r で baseline 実体を
	// 失うので、状態を変える前に拒否する。
	vol, err := m.resolveVolume(ctx, b)
	if err != nil {
		return Info{}, err
	}
	if err := m.refuseIfBackingBaseline(name, vol, "recreate"); err != nil {
		return Info{}, err
	}
	// on-recreate があればそれを、無ければ on-create を再適用する。
	ev := hooks.OnRecreate
	if _, ok := m.hookExists(hooks.OnRecreate); !ok {
		ev = hooks.OnCreate
	}
	return m.recreateFrom(ctx, b, m.currentBaseline(), ev)
}

// recreateFrom は origin から新クローンを作り、mysqld を新ボリュームへ付け替え、
// 旧ボリュームを非同期削除する共通経路(reset の fsx 版 / recreate が共有)。
// 切替(state.db の origin 更新・付け替え)はここに一度だけ書く(仕様 13章)。
func (m *Manager) recreateFrom(ctx context.Context, b state.Branch, origin storage.SnapshotRef, hookEv hooks.Event) (Info, error) {
	// recreate は新 mysqld を起動する。旧は Kill されるので純増ではないが、
	// 一時的に新旧が並ぶため admission を確認する。
	if err := m.admitMemory("recreate"); err != nil {
		return Info{}, err
	}
	if err := m.admitStorage(ctx, "recreate"); err != nil {
		return Info{}, err
	}
	if err := m.db.SetState(b.Name, state.StateResetting, ""); err != nil {
		return Info{}, err
	}
	oldVol, err := m.resolveVolume(ctx, b)
	if err != nil {
		_ = m.db.SetState(b.Name, state.StateError, err.Error())
		return Info{}, err
	}
	// 旧 mysqld は Kill(dirty state を捨てるので graceful 不要)。
	_ = m.eng.Kill(ctx, m.instance(b, oldVol))

	// 固定名バックエンド(ebs-zfs)は新旧が同名で共存できないため、旧を一時名へ
	// 退避してから clone する。clone 失敗時は退避を戻して原状復帰する。
	renamer, needSwap := m.st.(storage.Renamer)
	swapName := ""
	if !m.st.Capabilities().ClonesAreDistinct && needSwap {
		swapName = b.Name + "-recreating"
		stashed, rerr := renamer.Rename(ctx, oldVol, swapName)
		if rerr != nil {
			_ = m.db.SetState(b.Name, state.StateError, rerr.Error())
			return Info{}, fmt.Errorf("recreate stash: %w", rerr)
		}
		oldVol = stashed
	}
	newVol, err := m.st.Clone(ctx, origin, b.Name)
	if err != nil {
		// clone 失敗: 退避した旧を元の名前へ戻す(原状復帰)
		if swapName != "" {
			if restored, rerr := renamer.Rename(ctx, oldVol, b.Name); rerr == nil {
				_ = m.eng.Start(ctx, m.instance(b, restored))
			}
		}
		_ = m.db.SetState(b.Name, state.StateError, err.Error())
		return Info{}, fmt.Errorf("recreate clone: %w", err)
	}
	if err := m.applyQuota(ctx, newVol); err != nil {
		_ = m.db.SetState(b.Name, state.StateError, err.Error())
		return Info{}, fmt.Errorf("recreate set quota: %w", err)
	}
	newIns := m.instance(b, newVol)
	if err := m.eng.Start(ctx, newIns); err != nil {
		_ = m.db.SetState(b.Name, state.StateError, err.Error())
		return Info{}, err
	}
	if err := m.eng.WaitReady(ctx, newIns); err != nil {
		_ = m.db.SetState(b.Name, state.StateError, err.Error())
		return Info{}, err
	}
	// hook を再適用してから @init 契約を揃える。Create と同じく
	// 「hook → graceful stop → @init → start」を通し、recreate 後の branch にも
	// 有効な @init を用意する(これが無いと後続の reset が Rollback 先を失う)。
	if err := m.runHook(ctx, hookEv, b, newVol); err != nil {
		_ = m.db.SetState(b.Name, state.StateError, err.Error())
		return Info{}, err
	}
	if err := m.eng.Stop(ctx, newIns); err != nil { // graceful(snapshot 前は必須)
		_ = m.db.SetState(b.Name, state.StateError, err.Error())
		return Info{}, fmt.Errorf("recreate stop before @init: %w", err)
	}
	newInitRef, err := m.st.SnapshotInit(ctx, newVol)
	if err != nil {
		_ = m.db.SetState(b.Name, state.StateError, err.Error())
		return Info{}, fmt.Errorf("recreate @init: %w", err)
	}
	// 新しい実体参照を記録(仕様 19章 / #90)
	_ = m.db.SetProvision(b.Name, newVol.Dataset, string(newInitRef))
	if err := m.eng.Start(ctx, newIns); err != nil {
		_ = m.db.SetState(b.Name, state.StateError, err.Error())
		return Info{}, err
	}
	if err := m.eng.WaitReady(ctx, newIns); err != nil {
		_ = m.db.SetState(b.Name, state.StateError, err.Error())
		return Info{}, err
	}
	// origin を更新(recreate は current baseline に、reset は同じ origin)。
	if err := m.db.UpdateOrigin(b.Name, string(origin)); err != nil {
		_ = m.db.SetState(b.Name, state.StateError, err.Error())
		return Info{}, err
	}
	// 旧ボリュームは裏で削除(fsx は約6分)。
	if job, err := m.st.DeleteAsync(ctx, oldVol); err == nil {
		go m.pollDeletion(job)
	}
	_ = m.runHook(ctx, hooks.OnReset, b, newVol)
	if err := m.db.SetState(b.Name, state.StateRunning, ""); err != nil {
		return Info{}, err
	}
	return m.info(ctx, b.Name)
}

// pollDeletion は非同期削除の完了をバックグラウンドで待つ(結果はログのみ)。
func (m *Manager) pollDeletion(job storage.JobID) {
	ctx := context.Background()
	for i := 0; i < 240; i++ { // 最大 ~20 分
		st, err := m.st.Poll(ctx, job)
		if err != nil || st != storage.JobRunning {
			return
		}
		time.Sleep(5 * time.Second)
	}
}

// Delete はブランチを削除する。
func (m *Manager) Delete(ctx context.Context, name string) error {
	unlock := m.lock(name)
	defer unlock()

	b, err := m.db.GetBranch(name)
	if err != nil {
		return err
	}
	vol, err := m.resolveVolume(ctx, b)
	if err != nil {
		// 実体が見つからない(手動削除・不整合)場合は行だけ片付ける
		return m.db.DeleteBranch(name)
	}
	// promote 済みブランチの保護(#179): delete(ebs-zfs は zfs destroy -r)が
	// baseline snapshot を巻き込み、current baseline が宙吊りになる。
	if err := m.refuseIfBackingBaseline(name, vol, "削除"); err != nil {
		return err
	}
	if err := m.db.SetState(name, state.StateDeleting, ""); err != nil {
		return err
	}
	ins := m.instance(b, vol)
	_ = m.eng.Stop(ctx, ins) // 動いていなくてもよい
	// on-delete の失敗は記録して続行(仕様 14-2)。
	_ = m.runHook(ctx, hooks.OnDelete, b, vol)

	job, err := m.st.DeleteAsync(ctx, vol)
	if err != nil {
		_ = m.db.SetState(name, state.StateError, err.Error())
		return fmt.Errorf("destroy: %w", err)
	}
	status, err := m.st.Poll(ctx, job)
	if err != nil {
		return err
	}
	if status == storage.JobCompleted {
		return m.db.DeleteBranch(name)
	}
	// 非同期バックエンド: 行は消して(発行済みポートは解放)、実削除の完了は
	// バックグラウンドで見届ける。
	go m.pollDeletion(job)
	return m.db.DeleteBranch(name)
}

// SetActiveConns は接続数の参照先を設定する(プロキシは Manager に依存する
// ため、sashikid がプロキシ起動後に配線する)。
func (m *Manager) SetActiveConns(fn func(name string) int) {
	m.cfg.ActiveConns = fn
}

// activeConns は「使用中」判定に使う接続数。proxy(cfg.ActiveConns)と
// connpoll(pollConns, #41)の大きい方を返す。どちらか一方でも接続を検出
// していれば reaper の停止・削除対象から外す。
func (m *Manager) activeConns(name string) int {
	n := 0
	if m.cfg.ActiveConns != nil {
		n = m.cfg.ActiveConns(name)
	}
	m.pollMu.Lock()
	if p := m.pollConns[name]; p > n {
		n = p
	}
	m.pollMu.Unlock()
	return n
}

// resolveProfileName は create 時の profile 名を確定する。空なら DefaultProfile。
// profiles が定義されているのに未知の名前なら ErrUnknownProfile。
func (m *Manager) resolveProfileName(name string) (string, error) {
	if name == "" {
		name = m.cfg.DefaultProfile
	}
	if name == "" || len(m.cfg.Profiles) == 0 {
		return name, nil // profile 機能未使用(global のみ)
	}
	if _, ok := m.cfg.Profiles[name]; !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownProfile, name)
	}
	return name, nil
}

// resolveProfile は branch の profile 名から reaper 用の idle 閾値を返す。
// profile 未定義・未設定フィールドは global へフォールバックする。
func (m *Manager) resolveProfile(name string) ProfilePolicy {
	pol := ProfilePolicy{
		IdleStopAfter:   m.cfg.IdleStopAfter,
		DeleteAfterIdle: m.cfg.DeleteAfterIdle,
	}
	if p, ok := m.cfg.Profiles[name]; ok {
		if p.IdleStopAfter > 0 {
			pol.IdleStopAfter = p.IdleStopAfter
		}
		if p.DeleteAfterIdle > 0 {
			pol.DeleteAfterIdle = p.DeleteAfterIdle
		}
	}
	return pol
}

// Lease は branch の lease 期限(expires_at)を now+d に設定する(仕様 13-6)。
// create --ttl と lease renew の実体。d<=0 は無効。返り値は確定した期限。
func (m *Manager) Lease(ctx context.Context, name string, d time.Duration) (time.Time, error) {
	if d <= 0 {
		return time.Time{}, fmt.Errorf("lease duration must be > 0")
	}
	if _, err := m.db.GetBranch(name); err != nil {
		return time.Time{}, err
	}
	exp := time.Now().UTC().Add(d)
	if err := m.db.SetExpiresAt(name, exp); err != nil {
		return time.Time{}, err
	}
	return exp, nil
}

// RouteBranch はプロキシ用: ブランチのポートを返す。
// 未知の名前は lazy create(有効時)、sleeping は wake する。
// 接続を保持したまま待たせる前提なので、作成完了までブロックする。
func (m *Manager) RouteBranch(ctx context.Context, name string) (int, error) {
	b, err := m.db.GetBranch(name)
	if errors.Is(err, ErrNotFound) {
		if !m.lazyEnabled() {
			return 0, err
		}
		info, cerr := m.Create(ctx, name, 0)
		if errors.Is(cerr, ErrExists) {
			// 同時接続が先に作成した場合: 出来上がりを引く
			b, err = m.db.GetBranch(name)
			if err != nil {
				return 0, err
			}
			return m.routeExisting(ctx, b)
		}
		if cerr != nil {
			return 0, cerr
		}
		return info.Port, nil
	}
	if err != nil {
		return 0, err
	}
	return m.routeExisting(ctx, b)
}

func (m *Manager) routeExisting(ctx context.Context, b state.Branch) (int, error) {
	switch b.State {
	case state.StateRunning:
		return b.Port, nil
	case state.StateSleeping:
		if _, err := m.Wake(ctx, b.Name); err != nil {
			return 0, err
		}
		return b.Port, nil
	case state.StateCreating, state.StateResetting:
		// 別の接続が作成/リセット中: 完了を待って通す(CI の接続プールが
		// 同時に張ってくるケース)。TCP は保持されたままなので待てる。
		return m.waitRunning(ctx, b.Name)
	default:
		return 0, fmt.Errorf("branch %s is %s", b.Name, b.State)
	}
}

// waitRunning はブランチが running になるまで待ってポートを返す。
func (m *Manager) waitRunning(ctx context.Context, name string) (int, error) {
	maxWait := m.cfg.LazyMaxWait
	if maxWait == 0 {
		maxWait = 20 * time.Second
	}
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		b, err := m.db.GetBranch(name)
		if err != nil {
			return 0, err
		}
		switch b.State {
		case state.StateRunning:
			return b.Port, nil
		case state.StateCreating, state.StateResetting:
			time.Sleep(200 * time.Millisecond)
		default:
			return 0, fmt.Errorf("branch %s is %s", name, b.State)
		}
	}
	return 0, fmt.Errorf("timeout waiting for branch %s to become running", name)
}

// expectedRSS は mysqld 1 本の想定 RSS。
func (m *Manager) expectedRSS() int64 {
	if m.cfg.ExpectedRSSBytes > 0 {
		return m.cfg.ExpectedRSSBytes
	}
	if m.cfg.BufferPoolBytes > 0 {
		return m.cfg.BufferPoolBytes + 300*1024*1024
	}
	return 0
}

// admitMemory は mysqld を 1 本増やす操作(create/wake/recreate/lazy)の前に
// 空きメモリと max_running を確認する。不足なら理由付きで拒否する(仕様 14-1)。
func (m *Manager) admitMemory(op string) error {
	// max_running: 現在 running な mysqld 数
	if m.cfg.MaxRunning > 0 {
		branches, err := m.db.ListBranches()
		if err != nil {
			return err
		}
		running := 0
		for _, b := range branches {
			if b.State == state.StateRunning {
				running++
			}
		}
		if running >= m.cfg.MaxRunning {
			return fmt.Errorf("%w: max_running reached (%d running, op=%s)", ErrLimitReached, running, op)
		}
	}
	// メモリ: MemAvailable > expected_rss + headroom
	if m.cfg.AvailableMem == nil {
		return nil
	}
	rss := m.expectedRSS()
	if rss <= 0 {
		return nil
	}
	avail, err := m.cfg.AvailableMem()
	if err != nil {
		return nil // 判定不能なら通す(best-effort)
	}
	headroom := m.cfg.MemoryHeadroomBytes
	if headroom <= 0 {
		headroom = rss
	}
	need := rss + headroom
	if avail < need {
		return fmt.Errorf("%w: memory (available %dMB < required %dMB, op=%s)",
			ErrLimitReached, avail/1024/1024, need/1024/1024, op)
	}
	return nil
}

func (m *Manager) lazyEnabled() bool {
	if !m.cfg.LazyCreate {
		return false
	}
	maxWait := m.cfg.LazyMaxWait
	if maxWait == 0 {
		maxWait = 20 * time.Second
	}
	return m.st.Capabilities().TypicalCreate <= maxWait
}

// Wake は sleeping(または停止している)ブランチの mysqld を起動する。
// error / deleting / creating のブランチは対象外(状態を上書きしない)。
func (m *Manager) Wake(ctx context.Context, name string) (Info, error) {
	unlock := m.lock(name)
	defer unlock()

	b, err := m.db.GetBranch(name)
	if err != nil {
		return Info{}, err
	}
	if b.State != state.StateSleeping && b.State != state.StateRunning {
		return Info{}, fmt.Errorf("branch %s is %s (cannot wake)", name, b.State)
	}
	if b.State == state.StateSleeping {
		if err := m.admitMemory("wake"); err != nil {
			return Info{}, err
		}
		if err := m.admitStorage(ctx, "wake"); err != nil {
			return Info{}, err
		}
	}
	vol, err := m.resolveVolume(ctx, b)
	if err != nil {
		return Info{}, err
	}
	ins := m.instance(b, vol)
	if running, _ := m.eng.IsRunning(ctx, ins); !running {
		if err := m.eng.Start(ctx, ins); err != nil {
			return Info{}, err
		}
		if err := m.eng.WaitReady(ctx, ins); err != nil {
			return Info{}, err
		}
	}
	if err := m.db.SetState(name, state.StateRunning, ""); err != nil {
		return Info{}, err
	}
	return m.info(ctx, name)
}

// TouchConn は最終接続時刻を記録する。SQLite への書き込みを抑えるため
// ブランチごとに 1 分に 1 回まで。
func (m *Manager) TouchConn(name string) {
	m.touchMu.Lock()
	last, ok := m.lastTouch[name]
	now := time.Now()
	if ok && now.Sub(last) < time.Minute {
		m.touchMu.Unlock()
		return
	}
	m.lastTouch[name] = now
	m.touchMu.Unlock()
	_ = m.db.TouchLastConn(name)
}

// BaselineInfo は現在のベースラインと base の snapshot 一覧。
type BaselineInfo struct {
	Current   string
	Snapshots []string
}

// baselinesOnDataset は dataset 上に置かれた登録済み baseline snapshot を返す(#179)。
// ebs-zfs では promote が branch dataset に snapshot を作るため、その branch を
// zfs destroy -r すると baseline ごと消える。reflink/apfs は snapshot を base 配下の
// 独立パスに置くので prefix が一致せず、このガードは自然に発火しない。
func (m *Manager) baselinesOnDataset(dataset string) ([]string, error) {
	bls, err := m.db.ListBaselines()
	if err != nil {
		return nil, err
	}
	prefix := dataset + "@"
	var on []string
	for _, bl := range bls {
		if strings.HasPrefix(bl.Snapshot, prefix) {
			on = append(on, bl.Snapshot)
		}
	}
	return on, nil
}

// refuseIfBackingBaseline は branch の dataset 上に登録済み baseline があれば
// ErrPreconditionFailed を返す(#179 / #289)。delete だけでなく reset / recreate も
// 対象で、どちらも snapshot を壊す:
//   - reset は zfs rollback -r で @init より新しい @baseline-* を破棄する
//     (派生 clone があれば rollback 自体が失敗して error 状態になる)
//   - recreate は dataset を <name>-recreating に rename してから destroy -r する
//     ので、baselines 行が存在しない名前を指し、以後の create が全滅する
//
// verb はエラー文の動詞(削除 / reset / recreate)。
func (m *Manager) refuseIfBackingBaseline(name string, vol storage.Volume, verb string) error {
	backing, err := m.baselinesOnDataset(vol.Dataset)
	if err != nil {
		return err
	}
	if len(backing) == 0 {
		return nil
	}
	return fmt.Errorf("%w: branch %q は baseline %v の実体を保持しています。"+
		"%s すると登録済み baseline が壊れるため拒否しました。"+
		"先に別の baseline を promote/set し、この baseline を delete/GC してください",
		ErrPreconditionFailed, name, backing, verb)
}

// currentBaseline は DB の切り替え記録を優先し、無ければバックエンド既定を使う。
func (m *Manager) currentBaseline() storage.SnapshotRef {
	if snap, ok := m.db.CurrentBaselineOverride(); ok {
		return storage.SnapshotRef(snap)
	}
	return m.baseline.CurrentBaseline()
}

// Baseline はベースライン情報を返す(API GET /baseline 用)。
func (m *Manager) Baseline(ctx context.Context) (BaselineInfo, error) {
	snaps, err := m.st.ListSnapshots(ctx)
	if err != nil {
		return BaselineInfo{}, err
	}
	info := BaselineInfo{Current: string(m.currentBaseline())}
	for _, s := range snaps {
		info.Snapshots = append(info.Snapshots, string(s))
	}
	return info, nil
}

// Get は 1 件の詳細。
func (m *Manager) Get(ctx context.Context, name string) (Info, error) {
	return m.info(ctx, name)
}

// List は全ブランチの詳細。
func (m *Manager) List(ctx context.Context) ([]Info, error) {
	bs, err := m.db.ListBranches()
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(bs))
	for _, b := range bs {
		info, err := m.info(ctx, b.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

func (m *Manager) info(ctx context.Context, name string) (Info, error) {
	b, err := m.db.GetBranch(name)
	if err != nil {
		return Info{}, err
	}
	// 既定は -1(不明)。取得できたバックエンドだけ実値を入れる。CoW 差分を
	// 素直に取れない apfs / reflink 等は -1 のままにして表示側で「-」を出す(#128)。
	info := Info{Branch: b, UsedBytes: -1, LogicalBytes: -1}
	if vol, err := m.resolveVolume(ctx, b); err == nil {
		if backing, err := m.baselinesOnDataset(vol.Dataset); err == nil && len(backing) > 0 {
			info.BackingBaselines = backing
		}
		if used, err := m.st.UsedBytes(ctx, vol); err == nil && used >= 0 {
			info.UsedBytes = used
		}
		if ls, ok := m.st.(storage.LogicalSizer); ok {
			if logical, err := ls.LogicalBytes(ctx, vol); err == nil && logical >= 0 {
				info.LogicalBytes = logical
			}
		}
	}
	if hs, err := m.db.LastHookStatus(name); err == nil && len(hs) > 0 {
		info.HookStatus = hs
	}
	// origin が current baseline と違えば stale(reset は古い origin に戻る、#130)。
	if cur := string(m.currentBaseline()); cur != "" && b.OriginSnapshot != cur {
		info.Stale = true
	}
	return info, nil
}

// resolveVolume は既存ブランチの Volume を storage の規約から再構成する。
// v0.1 では Clone と同じ名前規約(zfs: branch_parent/<name>)を使う。
type volumeResolver interface {
	ResolveVolume(ctx context.Context, name string) (storage.Volume, error)
}

func (m *Manager) resolveVolume(ctx context.Context, b state.Branch) (storage.Volume, error) {
	if r, ok := m.st.(volumeResolver); ok {
		return r.ResolveVolume(ctx, b.Name)
	}
	return storage.Volume{Name: b.Name}, nil
}

func (m *Manager) hookExists(event hooks.Event) (string, bool) {
	if m.hooks == nil {
		return "", false
	}
	return m.hooks.Find(event)
}

func (m *Manager) runHook(ctx context.Context, event hooks.Event, b state.Branch, vol storage.Volume) error {
	if m.hooks == nil {
		return nil
	}
	if _, ok := m.hooks.Find(event); !ok {
		return nil
	}
	id, _ := m.db.RecordHookStart(b.Name, string(event))
	res, err := m.hooks.Run(ctx, event, m.hookEnv(b, vol))
	_ = m.db.RecordHookFinish(id, res.ExitCode)
	if err != nil {
		return fmt.Errorf("hook %s failed: %w", event, err)
	}
	return nil
}

// stageErr は失敗ステージを保持するエラー(#89)。errors.Is(err, ErrHookFailed)
// / errors.Is(err, ErrEngineFailed) で分類できる。メッセージは元エラーのまま。
type stageErr struct {
	stage string
	err   error
}

func (e *stageErr) Error() string { return e.err.Error() }
func (e *stageErr) Unwrap() error { return e.err }

// Code は失敗ステージを機械判定用のコードとして返す(operations.error_code、#151)。
// ops.Runner が interface{ Code() string } で拾う(パッケージ間の import 循環を避ける)。
func (e *stageErr) Code() string { return e.stage }
func (e *stageErr) Is(target error) bool {
	switch target {
	case ErrHookFailed:
		return e.stage == "hook"
	case ErrEngineFailed:
		return strings.HasPrefix(e.stage, "engine-")
	}
	return false
}

// StageError はテスト・他層からステージ付きエラーを作るための公開コンストラクタ。
func StageError(stage string, err error) error { return &stageErr{stage: stage, err: err} }
