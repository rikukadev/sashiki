// ベースライン更新(仕様 10-5)。refresh スクリプト(データ投入・マスク・
// マイグレーション適用・mysqld 正常終了までを担当)を実行し、完了後に
// 新しい baseline snapshot を取得して current を切り替える。
// 古い baseline から生えた既存ブランチには影響しない。
//
// 不変条件「スナップショットは必ず正常終了状態でのみ取得する」の担保は
// スクリプトの exit 0 だけに頼らず、snapshot 取得前に base の datadir を
// 掴んでいるプロセスが残っていないことを検証する(quiesce チェック)。
package workspace

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rikukadev/sashiki/internal/baseline"
	"github.com/rikukadev/sashiki/internal/hooks"
	"github.com/rikukadev/sashiki/internal/state"
	"github.com/rikukadev/sashiki/internal/storage"
)

// ErrRefreshRunning は refresh の多重実行。
var ErrRefreshRunning = fmt.Errorf("baseline refresh is already running")

// RefreshConfig は refresh の設定。
type RefreshConfig struct {
	Script  string        // /etc/sashiki/refresh.sh
	Timeout time.Duration // 既定 1h
	// SourceDir が設定され、かつ Script のファイルが存在しない場合は
	// 組み込みローダー(internal/baseline.ApplyDir)で SourceDir/*.sql を適用する
	// (#101。「SQL を置いて refresh」だけで更新が完結する)。
	SourceDir string
	SourceDB  string // ローダーが各ファイル実行時に選択する DB(空 = 未選択)
	// RunSource はテスト注入用(非 nil ならローダーの代わりに呼ばれる)。
	RunSource func(ctx context.Context) error

	useLoader bool // 内部: SourceDir モードで実行するか
	// CheckQuiesced は snapshot 取得前の検証(テストで注入)。nil なら
	// storage の BasePath から既定実装を組み立てる。
	CheckQuiesced func(ctx context.Context) error

	// publish ポリシー(仕様 12-4)。
	RequireMasked    bool
	RequireValidated bool
	ValidatePort     int
	MaskedSentinel   string
	// SkipValidate はテストで validate(clone+engine)を飛ばす。
	SkipValidate bool
}

// basePathProvider は base の実パスを返せるバックエンド(ebszfs)。
type basePathProvider interface {
	BasePath(ctx context.Context) (string, error)
}

var (
	refreshRunning atomic.Bool
	refreshLastErr atomic.Value // string
)

// RefreshLastError は直近の refresh 失敗理由(成功時は空)。
func RefreshLastError() string {
	if v := refreshLastErr.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// resolveRefreshConfig は RefreshConfig の未設定フィールドを Manager 既定で埋める
// (refresh 一括 / build / validate 共通)。
func (m *Manager) resolveRefreshConfig(rc RefreshConfig) RefreshConfig {
	if rc.Script == "" {
		rc.Script = m.baselinePolicy.Script
	}
	if rc.Script == "" {
		rc.Script = "/etc/sashiki/refresh.sh"
	}
	if rc.Timeout == 0 {
		rc.Timeout = m.baselinePolicy.Timeout
	}
	if rc.Timeout == 0 {
		rc.Timeout = time.Hour
	}
	if rc.SourceDir == "" {
		rc.SourceDir = m.baselinePolicy.SourceDir
	}
	if rc.SourceDB == "" {
		rc.SourceDB = m.baselinePolicy.SourceDB
	}
	if !rc.RequireMasked && m.baselinePolicy.RequireMasked {
		rc.RequireMasked = true
	}
	if !rc.RequireValidated && m.baselinePolicy.RequireValidated {
		rc.RequireValidated = true
	}
	if rc.ValidatePort == 0 {
		rc.ValidatePort = m.baselinePolicy.ValidatePort
	}
	if rc.MaskedSentinel == "" {
		rc.MaskedSentinel = m.baselinePolicy.MaskedSentinel
	}
	// SourceDir モード判定: script が無く source_dir があれば組み込みローダー(#101)。
	// 実在チェック(script も source_dir も無い)は呼び出し側で行う。
	rc.useLoader = rc.RunSource != nil
	if !rc.useLoader && rc.SourceDir != "" {
		if _, err := os.Stat(rc.Script); err != nil {
			rc.useLoader = true
		}
	}
	if rc.CheckQuiesced == nil {
		rc.CheckQuiesced = m.defaultQuiesceCheck
	}
	return rc
}

// newBaselineTag は baseline-YYYYMMDDTHHMMSSZ 形式のタグを返す。
func newBaselineTag() string {
	return "baseline-" + time.Now().UTC().Format("20060102T150405Z")
}

// baseSnapshotRef は tag に対応する base の snapshot 完全修飾名を返す
// (current の "@" 手前を base dataset とみなす)。build の応答に使う。
func (m *Manager) baseSnapshotRef(tag string) string {
	cur := string(m.currentBaseline())
	if i := strings.LastIndex(cur, "@"); i >= 0 {
		return cur[:i+1] + tag
	}
	return tag
}

// RefreshBaseline は refresh(build→validate→publish 一括)を非同期で開始する。
func (m *Manager) RefreshBaseline(ctx context.Context, rc RefreshConfig) (tag string, err error) {
	rc = m.resolveRefreshConfig(rc)
	if !rc.useLoader { // source loader モードでは script は不要(#101)
		if _, err := os.Stat(rc.Script); err != nil {
			return "", fmt.Errorf("refresh script %s: %w", rc.Script, err)
		}
	}
	if !refreshRunning.CompareAndSwap(false, true) {
		return "", ErrRefreshRunning
	}
	tag = newBaselineTag()

	go func() {
		defer refreshRunning.Store(false)
		cctx, cancel := context.WithTimeout(context.Background(), rc.Timeout)
		defer cancel()
		if err := m.runRefresh(cctx, rc, tag); err != nil {
			refreshLastErr.Store(err.Error())
			log.Printf("baseline refresh: %v", err)
			return
		}
		refreshLastErr.Store("")
	}()
	return tag, nil
}

// buildCandidate は source loader(#101)または refresh script を実行し、正常終了・
// quiesce・auto.cnf 削除を確認してから base の snapshot を取得し candidate として
// 登録する(#84 build 段階)。返り値は candidate の snapshot と masked フラグ。
func (m *Manager) buildCandidate(ctx context.Context, rc RefreshConfig, tag string) (storage.SnapshotRef, bool, error) {
	// masked 判定は sentinel ファイルの「今回の build で作られたか」で行う。
	// 前回の残留を今回のマスク済み扱いにしないよう、build 実行の前に必ず消す
	// (これを怠ると require_masked が 2 回目以降で実質無効になる。security invariant)。
	if rc.MaskedSentinel != "" {
		if err := os.Remove(rc.MaskedSentinel); err != nil && !os.IsNotExist(err) {
			return "", false, fmt.Errorf("masked sentinel の事前削除に失敗: %w", err)
		}
	}
	if rc.useLoader {
		if err := m.runSourceLoader(ctx, rc); err != nil {
			// ローダー失敗時も mysqld が残っていれば回収を試みる(自己修復)
			if qerr := rc.CheckQuiesced(ctx); qerr != nil {
				m.reclaimBase(ctx)
			}
			return "", false, fmt.Errorf("source loader: %w", err)
		}
	} else {
		cmd := exec.CommandContext(ctx, rc.Script)
		cmd.Env = append(os.Environ(),
			"SASHIKI_EVENT=baseline-build",
			"SASHIKI_BASELINE_TAG="+tag,
		)
		out, scriptErr := cmd.CombinedOutput()
		if scriptErr != nil {
			// スクリプト失敗時も mysqld が残っていれば回収を試みる(自己修復)
			if qerr := rc.CheckQuiesced(ctx); qerr != nil {
				m.reclaimBase(ctx)
			}
			return "", false, fmt.Errorf("script failed: %w: %s", scriptErr, tail(out, 500))
		}
	}
	// exit 0 でも信用せず、snapshot 取得前に quiesce を検証する
	if err := rc.CheckQuiesced(ctx); err != nil {
		m.reclaimBase(ctx)
		if err2 := rc.CheckQuiesced(ctx); err2 != nil {
			return "", false, fmt.Errorf("base is not quiesced after script (snapshot aborted): %w", err2)
		}
		log.Printf("baseline build: leftover mysqld was terminated before snapshot")
	}
	// server_uuid の重複対策(#80 / 仕様 12-3): 正常終了確認後・snapshot 前に auto.cnf 削除。
	if err := m.removeBaseAutoCnf(ctx); err != nil {
		return "", false, fmt.Errorf("auto.cnf removal failed (snapshot aborted): %w", err)
	}
	snap, err := m.st.SnapshotBase(ctx, tag)
	if err != nil {
		return "", false, fmt.Errorf("snapshot: %w", err)
	}
	masked := rc.MaskedSentinel != "" && fileExists(rc.MaskedSentinel)
	if err := m.db.RegisterBaseline(string(snap), state.BaselineProvenance{DataAsOf: tag, Masked: masked}); err != nil {
		return "", false, fmt.Errorf("register candidate: %w", err)
	}
	return snap, masked, nil
}

func (m *Manager) runRefresh(ctx context.Context, rc RefreshConfig, tag string) error {
	snap, masked, err := m.buildCandidate(ctx, rc, tag)
	if err != nil {
		return err
	}
	validated := false
	if !rc.SkipValidate {
		if err := m.validateCandidate(ctx, snap, rc); err != nil {
			return fmt.Errorf("validate: %w (candidate %s は publish しない)", err, snap)
		}
		validated = true
		_ = m.db.RegisterBaseline(string(snap), state.BaselineProvenance{DataAsOf: tag, Masked: masked, Validated: true})
	}
	if rc.RequireMasked && !masked {
		return fmt.Errorf("publish rejected: baseline is not masked (require_masked)")
	}
	if rc.RequireValidated && !validated {
		return fmt.Errorf("publish rejected: baseline is not validated (require_validated)")
	}
	m.baselineMu.Lock()
	perr := m.db.SetCurrentBaseline(string(snap))
	m.baselineMu.Unlock()
	if perr != nil {
		return fmt.Errorf("publish (set current): %w", perr)
	}
	log.Printf("baseline refresh: published %s (masked=%v validated=%v)", snap, masked, validated)
	return nil
}

// PrepareBuild は build 用の tag と、その candidate が取得される snapshot 完全修飾名を
// 事前に返す(#84)。API は build を非同期実行しつつ応答でこの値を返す。
func (m *Manager) PrepareBuild() (tag, snapshot string) {
	tag = newBaselineTag()
	return tag, m.baseSnapshotRef(tag)
}

// BuildBaseline は build 段階だけを実行し candidate を作る(#84)。tag が空なら自動生成。
// refresh と排他(refreshRunning)。API はこれを ops で非同期実行し、応答に snapshot を返す。
func (m *Manager) BuildBaseline(ctx context.Context, tag string) (storage.SnapshotRef, error) {
	rc := m.resolveRefreshConfig(RefreshConfig{})
	if !rc.useLoader { // source loader モードでは script は不要(#101)
		if _, err := os.Stat(rc.Script); err != nil {
			return "", fmt.Errorf("refresh script %s: %w", rc.Script, err)
		}
	}
	if tag == "" {
		tag = newBaselineTag()
	}
	if !refreshRunning.CompareAndSwap(false, true) {
		return "", ErrRefreshRunning
	}
	defer refreshRunning.Store(false)
	snap, _, err := m.buildCandidate(ctx, rc, tag)
	return snap, err
}

// ValidateBaseline は candidate を一時 branch で起動して検証し、validated=true にする(#84)。
func (m *Manager) ValidateBaseline(ctx context.Context, snapshot string) error {
	b, err := m.db.GetBaseline(snapshot)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrBaselineNotFound, snapshot)
	}
	rc := m.resolveRefreshConfig(RefreshConfig{})
	if err := m.validateCandidate(ctx, storage.SnapshotRef(snapshot), rc); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	prov := b.Prov
	prov.Validated = true
	return m.db.RegisterBaseline(snapshot, prov)
}

// validateCandidate は candidate snapshot を一時 branch で起動し、
// on-baseline-validate hook で検証してから破棄する(仕様 12-4)。
// crash recovery が走らずに起動できること自体が「正常終了状態で撮られた」検証を兼ねる。
func (m *Manager) validateCandidate(ctx context.Context, snap storage.SnapshotRef, rc RefreshConfig) error {
	// 共有名 _validate を使うので同時実行を許さない(#310 review)。
	m.validateMu.Lock()
	defer m.validateMu.Unlock()
	name := "_validate"
	// 既存の検証 volume が残っていれば掃除
	if vol, err := m.resolveVolume(ctx, state.Branch{Name: name}); err == nil {
		_ = m.eng.Kill(ctx, m.instance(state.Branch{Name: name, Port: rc.ValidatePort}, vol))
		if job, derr := m.st.DeleteAsync(ctx, vol); derr == nil {
			_, _ = m.st.Poll(ctx, job)
		}
	}
	if err := m.admitMemory("baseline-validate"); err != nil {
		return fmt.Errorf("cannot validate (admission): %w", err)
	}
	vol, err := m.st.Clone(ctx, snap, name)
	if err != nil {
		return fmt.Errorf("clone candidate: %w", err)
	}
	port := rc.ValidatePort
	if port == 0 {
		port = 3999
	}
	b := state.Branch{Name: name, Port: port}
	ins := m.instance(b, vol)
	cleanup := func() {
		// 親 ctx が timeout/cancel されていても掃除は必ず完了させる。
		// 親 ctx を使うと、validate タイムアウト時に canceled ctx で Kill/Delete が
		// 即失敗し、未マスクかもしれない _validate の mysqld が :3999 に残ってしまう。
		cctx := context.WithoutCancel(ctx)
		_ = m.eng.Kill(cctx, ins)
		if job, derr := m.st.DeleteAsync(cctx, vol); derr == nil {
			_, _ = m.st.Poll(cctx, job)
		}
	}
	defer cleanup()

	// crash recovery なしで ready になること = 正常終了状態で撮られた証拠。
	if err := m.eng.Start(ctx, ins); err != nil {
		return fmt.Errorf("candidate engine start: %w", err)
	}
	if err := m.eng.WaitReady(ctx, ins); err != nil {
		return fmt.Errorf("candidate not ready (crash recovery?): %w", err)
	}
	// operator の検証(mask validation / migration version / sanity)。無ければスキップ。
	if m.hooks != nil {
		if _, ok := m.hooks.Find(hooks.OnBaselineValidate); ok {
			if err := m.runHook(ctx, hooks.OnBaselineValidate, b, vol); err != nil {
				return fmt.Errorf("validation hook failed: %w", err)
			}
		}
	}
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// removeBaseAutoCnf は base datadir の auto.cnf を snapshot 取得前に削除する。
// datadir をクローンすると auto.cnf の server_uuid まで複製され、全ブランチが
// 同一 UUID になる。削除しておけば各ブランチの初回起動時に mysqld が固有の
// UUID を再生成する(#80)。mysqld の正常終了(quiesce 検証)後にのみ呼ぶこと。
// 削除には base datadir への書き込み権限(root 相当)が必要。
// BasePath を提供しないバックエンド(fsx)ではスキップ。
func (m *Manager) removeBaseAutoCnf(ctx context.Context) error {
	bp, ok := m.st.(basePathProvider)
	if !ok {
		return nil
	}
	path, err := bp.BasePath(ctx)
	if err != nil || path == "" {
		return nil
	}
	autoCnf := filepath.Join(path, "data", "auto.cnf")
	if err := os.Remove(autoCnf); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", autoCnf, err)
	}
	return nil
}

// defaultQuiesceCheck は base の datadir を引数に持つプロセスが残っていないか
// を確認する。BasePath を提供しないバックエンド(fsx)ではスキップ。
func (m *Manager) defaultQuiesceCheck(ctx context.Context) error {
	bp, ok := m.st.(basePathProvider)
	if !ok {
		return nil
	}
	path, err := bp.BasePath(ctx)
	if err != nil || path == "" {
		return nil
	}
	pids := findProcsUsing(path)
	if len(pids) > 0 {
		return fmt.Errorf("processes still using %s: pids %v", path, pids)
	}
	return nil
}

// reclaimBase は base の datadir を掴んでいるプロセスへ SIGTERM を送り、
// 停止を待つ(mysqld は TERM で正常シャットダウンする)。
func (m *Manager) reclaimBase(ctx context.Context) {
	bp, ok := m.st.(basePathProvider)
	if !ok {
		return
	}
	path, err := bp.BasePath(ctx)
	if err != nil || path == "" {
		return
	}
	pids := findProcsUsing(path)
	for _, pid := range pids {
		log.Printf("baseline refresh: sending SIGTERM to leftover pid %d", pid)
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if len(findProcsUsing(path)) == 0 {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// findProcsUsing は path をコマンドラインに含むプロセスの PID 一覧(pgrep -f)。
func findProcsUsing(path string) []int {
	out, err := exec.Command("pgrep", "-f", "--", "datadir="+path).Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Fields(string(out)) {
		var pid int
		if _, err := fmt.Sscanf(line, "%d", &pid); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// RefreshInProgress は実行中かどうか。
func RefreshInProgress() bool { return refreshRunning.Load() }

func tail(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return "..." + string(b[len(b)-n:])
}

// runSourceLoader は組み込みローダー(#101)で SourceDir/*.sql を base に適用する。
// refresh.sh を書かなくても「SQL を置いて refresh」だけで更新が完結する。
func (m *Manager) runSourceLoader(ctx context.Context, rc RefreshConfig) error {
	if rc.RunSource != nil { // テスト注入
		return rc.RunSource(ctx)
	}
	bp, ok := m.st.(basePathProvider)
	if !ok {
		return fmt.Errorf("このバックエンドは base の実パスを解決できないため source_dir を使えません(refresh.sh を使ってください)")
	}
	base, err := bp.BasePath(ctx)
	if err != nil || base == "" {
		return fmt.Errorf("base path: %v", err)
	}
	srv := baseline.Server{
		DataDir:   base + "/data",
		Socket:    "/tmp/sashiki-refresh.sock",
		PidFile:   "/tmp/sashiki-refresh.pid",
		LogError:  "/var/log/sashiki/refresh.err",
		MysqldBin: m.cfg.MysqldBin,
		ExtraCnf:  m.cfg.MysqlExtraCnf,
	}
	// エンジンごとに実行系(と適用記録の SQL 方言)を差し替える(#226)。
	ops := baseline.RealOps()
	runUser := "mysql"
	if m.cfg.EngineType == "postgres" {
		ops = baseline.PostgresOps()
		srv.Socket = "/tmp" // postgres では socket ディレクトリ
		srv.LogError = ""   // 既定(os.TempDir())に任せる
		srv.PgBinDir = m.cfg.PgBinDir
		srv.PgDB = rc.SourceDB
		runUser = m.cfg.PgRunUser
		if runUser == "" {
			runUser = "postgres"
		}
	}
	if os.Geteuid() == 0 {
		if uid, gid, err := baseline.LookupOSUser(runUser); err == nil {
			srv.UID, srv.GID = uid, gid
		}
	}
	applied, err := baseline.ApplyDir(ctx, srv, ops, rc.SourceDir, rc.SourceDB)
	if err != nil {
		return err
	}
	log.Printf("baseline refresh: source loader applied %d file(s): %v", len(applied), applied)
	return nil
}
