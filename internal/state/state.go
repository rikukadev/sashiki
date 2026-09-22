// Package state は sashikid の状態を SQLite に保持する(仕様 14-3)。
// used_bytes は保存しない(zfs get を都度引く)。
package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	_ "modernc.org/sqlite" // pure Go SQLite driver
)

// Branch の state 値。
const (
	StateCreating  = "creating"
	StateRunning   = "running"
	StateSleeping  = "sleeping"
	StateResetting = "resetting"
	StateDeleting  = "deleting"
	StateError     = "error"
)

// ErrNotFound はブランチが存在しないとき。
var ErrNotFound = errors.New("branch not found")

// Branch は state.db の branches 1 行。
type Branch struct {
	Name             string
	State            string
	Port             int
	OriginSnapshot   string
	CreatedAt        time.Time
	LastConnAt       *time.Time
	ErrorMessage     string
	FailedOp         string
	ErrorCode        string
	Recoverable      bool
	SuggestedActions []string
	// provenance(仕様 11-2): core は source の中身を解釈しない。
	Profile   string
	Owner     string
	Purpose   string
	Source    string // opaque JSON
	ExpiresAt *time.Time
	// 実体参照(仕様 19章 / #90): fsx の世代管理と reset 戻り先の明示に使う。
	EngineState  string // mysqld の期待状態(running|stopped。空 = 不明/遷移中)
	InitSnapshot string // reset の戻り先 @init(空なら <dataset>@init を組み立てる)
	VolumeRef    string // backend 固有の volume 識別子(dataset 名など)
	// ErrorAt は error 状態になった時刻(#298)。reaper の error_retention の起点。
	// 旧行や error 以外では nil。
	ErrorAt *time.Time
}

// HookRun は hook 実行記録。
type HookRun struct {
	ID         int64
	Branch     string
	Event      string
	StartedAt  time.Time
	FinishedAt *time.Time
	ExitCode   *int
	LogPath    string
}

// DB は state.db へのハンドル。
type DB struct {
	sql *sql.DB
}

// Open は SQLite を開き、スキーマを作成する。
func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	// sashikid はシングルプロセスで、SQLite への同時書き込みを避ける。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("init schema: %w", err)
	}
	// 既存 DB へのカラム追加(存在すればエラーになるが無視する)。
	for _, col := range []string{
		"ALTER TABLE branches ADD COLUMN failed_operation TEXT",
		"ALTER TABLE branches ADD COLUMN error_code TEXT",
		"ALTER TABLE branches ADD COLUMN recoverable INTEGER",
		"ALTER TABLE branches ADD COLUMN suggested_actions TEXT",
		"ALTER TABLE branches ADD COLUMN profile TEXT",
		"ALTER TABLE branches ADD COLUMN owner TEXT",
		"ALTER TABLE branches ADD COLUMN purpose TEXT",
		"ALTER TABLE branches ADD COLUMN source TEXT",
		"ALTER TABLE branches ADD COLUMN expires_at TEXT",
		"ALTER TABLE branches ADD COLUMN engine_state TEXT",
		"ALTER TABLE branches ADD COLUMN init_snapshot TEXT",
		"ALTER TABLE branches ADD COLUMN volume_ref TEXT",
		"ALTER TABLE branches ADD COLUMN error_at TEXT", // #298
		"ALTER TABLE tokens ADD COLUMN scope TEXT",      // #294: NULL は admin(旧トークン互換)
		"ALTER TABLE baselines ADD COLUMN source_revision TEXT",
		"ALTER TABLE baselines ADD COLUMN schema_revision TEXT",
		"ALTER TABLE baselines ADD COLUMN data_as_of TEXT",
		"ALTER TABLE baselines ADD COLUMN masked INTEGER",
		"ALTER TABLE baselines ADD COLUMN mask_pipeline_version TEXT",
		"ALTER TABLE baselines ADD COLUMN validated INTEGER",
	} {
		_, _ = db.Exec(col)
	}
	alignOwner(path)
	return &DB{sql: db}, nil
}

// alignOwner は作った DB ファイルを、置き場のディレクトリと同じ所有者に揃える。
//
// root で叩いた CLI(`sudo sashiki baseline import` 等)が state.db を先に作ると
// root 所有になり、`User=sashiki` で動く sashikid が書けなくなる
// (`attempt to write a readonly database`)。README どおりの順で入れると必ず踏む。
//
// ディレクトリの所有者(deb の postinstall が sashiki にしている)に合わせれば、
// どちらが先に作っても daemon が書ける。**特定のユーザー名を焼き込まない**のは、
// ソースから入れた場合や macOS の process モードでは sashiki ユーザーが
// 存在しないため。その場合はディレクトリも自分の所有なので何もしない。
//
// ここ(作る場所)に置くのは、呼び出し側で忘れられるとこのバグが再発するから。
func alignOwner(path string) {
	if os.Geteuid() != 0 {
		return // root でなければ chown できず、必要も無い
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return
	}
	dst, ok := di.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	// WAL モードなので -wal / -shm も同じ所有者でないと書けない。
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(f)
		if err != nil {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid == dst.Uid && st.Gid == dst.Gid {
			continue
		}
		_ = os.Chown(f, int(dst.Uid), int(dst.Gid)) // 失敗しても致命ではない
	}
}

const schema = `
CREATE TABLE IF NOT EXISTS branches (
  name             TEXT PRIMARY KEY,
  state            TEXT NOT NULL,
  port             INTEGER NOT NULL UNIQUE,
  origin_snapshot  TEXT NOT NULL,
  created_at       TEXT NOT NULL,
  last_conn_at     TEXT,
  error_message    TEXT,
  failed_operation TEXT,
  error_code       TEXT,
  recoverable      INTEGER,
  suggested_actions TEXT,
  profile          TEXT,
  owner            TEXT,
  purpose          TEXT,
  source           TEXT,
  expires_at       TEXT,
  engine_state     TEXT,
  init_snapshot    TEXT,
  volume_ref       TEXT,
  error_at         TEXT
);
CREATE TABLE IF NOT EXISTS hook_runs (
  id          INTEGER PRIMARY KEY,
  branch      TEXT NOT NULL,
  event       TEXT NOT NULL,
  started_at  TEXT NOT NULL,
  finished_at TEXT,
  exit_code   INTEGER
);
CREATE TABLE IF NOT EXISTS tokens (
  name         TEXT PRIMARY KEY,
  hash         TEXT NOT NULL,
  created_at   TEXT NOT NULL,
  last_used_at TEXT
);
CREATE TABLE IF NOT EXISTS baselines (
  snapshot    TEXT PRIMARY KEY,
  created_at  TEXT NOT NULL,
  is_current  INTEGER NOT NULL DEFAULT 0,
  source_revision     TEXT,
  schema_revision     TEXT,
  data_as_of          TEXT,
  masked              INTEGER,
  mask_pipeline_version TEXT,
  validated           INTEGER
);
CREATE TABLE IF NOT EXISTS operations (
  id          TEXT PRIMARY KEY,
  type        TEXT NOT NULL,          -- create|reset|recreate|delete|wake|baseline-refresh
  target      TEXT NOT NULL,          -- branch 名 or baseline tag
  state       TEXT NOT NULL,          -- running|completed|failed
  started_at  TEXT NOT NULL,
  finished_at TEXT,
  error_json  TEXT
);
CREATE INDEX IF NOT EXISTS idx_operations_target ON operations(target);
`

// Close は DB を閉じる。
func (d *DB) Close() error { return d.sql.Close() }

// Writable は state.db が書き込み可能か確認する(doctor 用 #88)。
// スキーマ変更を伴う一時テーブルの作成/削除で、read-only ファイルや
// ディスク満杯を検出する。副作用は残さない。
func (d *DB) Writable() error {
	if _, err := d.sql.Exec(`CREATE TABLE IF NOT EXISTS _doctor_probe (id INTEGER)`); err != nil {
		return err
	}
	_, err := d.sql.Exec(`DROP TABLE IF EXISTS _doctor_probe`)
	return err
}

const timeFmt = time.RFC3339

// CreateBranch は新しいブランチ行を creating 状態で挿入する。
func (d *DB) CreateBranch(name string, port int, origin string) error {
	_, err := d.sql.Exec(
		`INSERT INTO branches (name, state, port, origin_snapshot, created_at) VALUES (?, ?, ?, ?, ?)`,
		name, StateCreating, port, origin, time.Now().UTC().Format(timeFmt))
	return err
}

// SetState は状態遷移を記録する。error 以外に遷移するとき詳細をクリアする。
// SetProvision は volume の実体参照と @init snapshot を記録する(仕様 19章 / #90)。
// create / recreate の切替後に呼ぶ。fsx の世代管理と reset 戻り先の明示に使う。
func (d *DB) SetProvision(name, volumeRef, initSnapshot string) error {
	res, err := d.sql.Exec(
		`UPDATE branches SET volume_ref = ?, init_snapshot = ? WHERE name = ?`,
		volumeRef, initSnapshot, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (d *DB) SetState(name, st, errMsg string) error {
	// engine_state(仕様 11-1)は lifecycle state から導出して同時更新する:
	// running → mysqld 稼働、sleeping → 停止。遷移中(creating/resetting/deleting)
	// と error は前回値を保持する(不明のため)。
	res, err := d.sql.Exec(
		`UPDATE branches SET state = ?, error_message = ?,
		   engine_state = CASE ?
		     WHEN 'running' THEN 'running'
		     WHEN 'sleeping' THEN 'stopped'
		     ELSE engine_state END,
		   failed_operation = NULL, error_code = NULL, recoverable = NULL, suggested_actions = NULL,
		   error_at = CASE WHEN ? = 'error' THEN COALESCE(error_at, ?) ELSE NULL END
		 WHERE name = ?`, st, errMsg, st, st, time.Now().UTC().Format(timeFmt), name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetError は error 状態と診断情報を記録する(仕様 11-1)。
func (d *DB) SetError(name, failedOp, code string, recoverable bool, msg string, suggestions []string) error {
	rec := 0
	if recoverable {
		rec = 1
	}
	sug := ""
	if len(suggestions) > 0 {
		b, _ := json.Marshal(suggestions)
		sug = string(b)
	}
	res, err := d.sql.Exec(
		`UPDATE branches SET state = ?, error_message = ?, failed_operation = ?,
		   error_code = ?, recoverable = ?, suggested_actions = ?,
		   error_at = COALESCE(error_at, ?) WHERE name = ?`,
		StateError, msg, failedOp, code, rec, sug, time.Now().UTC().Format(timeFmt), name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkErrorAtIfMissing は error 状態で error_at が無い行(#298 以前に error に
// なったもの)に現在時刻を入れる。retention をアップグレード時点から数えるため。
func (d *DB) MarkErrorAtIfMissing(name string) error {
	_, err := d.sql.Exec(`UPDATE branches SET error_at = ? WHERE name = ? AND state = ? AND error_at IS NULL`,
		time.Now().UTC().Format(timeFmt), name, StateError)
	return err
}

// SetErrorAtForTest はテスト用に error_at を書き換える。
func (d *DB) SetErrorAtForTest(name string, t time.Time) error {
	_, err := d.sql.Exec(`UPDATE branches SET error_at = ? WHERE name = ?`, t.UTC().Format(timeFmt), name)
	return err
}

// TouchLastConn は最終接続時刻を更新する。
func (d *DB) TouchLastConn(name string) error {
	_, err := d.sql.Exec(`UPDATE branches SET last_conn_at = ? WHERE name = ?`,
		time.Now().UTC().Format(timeFmt), name)
	return err
}

// Meta は create 時に付ける provenance(仕様 11-2)。
type Meta struct {
	Profile string
	Owner   string
	Purpose string
	Source  string // opaque JSON。core は解釈しない
}

// SetMeta は provenance を記録する(create 直後)。
func (d *DB) SetMeta(name string, m Meta) error {
	_, err := d.sql.Exec(
		`UPDATE branches SET profile = ?, owner = ?, purpose = ?, source = ? WHERE name = ?`,
		nullIfEmpty(m.Profile), nullIfEmpty(m.Owner), nullIfEmpty(m.Purpose), nullIfEmpty(m.Source), name)
	return err
}

// SetExpiresAt は lease 期限を設定する(#34)。
func (d *DB) SetExpiresAt(name string, t time.Time) error {
	_, err := d.sql.Exec(`UPDATE branches SET expires_at = ? WHERE name = ?`,
		t.UTC().Format(timeFmt), name)
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// UpdateOrigin は branch の origin_snapshot を更新する(recreate 時)。
func (d *DB) UpdateOrigin(name, origin string) error {
	_, err := d.sql.Exec(`UPDATE branches SET origin_snapshot = ? WHERE name = ?`, origin, name)
	return err
}

// DeleteBranch は行を削除する。
func (d *DB) DeleteBranch(name string) error {
	_, err := d.sql.Exec(`DELETE FROM branches WHERE name = ?`, name)
	return err
}

// GetBranch は 1 件取得。無ければ ErrNotFound。
func (d *DB) GetBranch(name string) (Branch, error) {
	row := d.sql.QueryRow(
		`SELECT name, state, port, origin_snapshot, created_at, last_conn_at, COALESCE(error_message,''),
		        failed_operation, error_code, recoverable, suggested_actions,
		        profile, owner, purpose, source, expires_at,
		        engine_state, init_snapshot, volume_ref, error_at
		 FROM branches WHERE name = ?`, name)
	return scanBranch(row)
}

// ListBranches は作成順で全件返す。
func (d *DB) ListBranches() ([]Branch, error) {
	rows, err := d.sql.Query(
		`SELECT name, state, port, origin_snapshot, created_at, last_conn_at, COALESCE(error_message,''),
		        failed_operation, error_code, recoverable, suggested_actions,
		        profile, owner, purpose, source, expires_at,
		        engine_state, init_snapshot, volume_ref, error_at
		 FROM branches ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Branch
	for rows.Next() {
		b, err := scanBranch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// UsedPorts は使用中ポートの集合。
func (d *DB) UsedPorts() (map[int]bool, error) {
	rows, err := d.sql.Query(`SELECT port FROM branches`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	used := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		used[p] = true
	}
	return used, rows.Err()
}

type scannable interface{ Scan(dest ...any) error }

func scanBranch(row scannable) (Branch, error) {
	var b Branch
	var created string
	var lastConn sql.NullString
	var failedOp, errCode, sug sql.NullString
	var rec sql.NullInt64
	var profile, owner, purpose, source, expires sql.NullString
	var engineState, initSnap, volRef, errorAt sql.NullString
	err := row.Scan(&b.Name, &b.State, &b.Port, &b.OriginSnapshot, &created, &lastConn, &b.ErrorMessage,
		&failedOp, &errCode, &rec, &sug,
		&profile, &owner, &purpose, &source, &expires,
		&engineState, &initSnap, &volRef, &errorAt)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	if err != nil {
		return b, err
	}
	b.FailedOp = failedOp.String
	b.ErrorCode = errCode.String
	b.Recoverable = rec.Valid && rec.Int64 != 0
	if sug.Valid && sug.String != "" {
		_ = json.Unmarshal([]byte(sug.String), &b.SuggestedActions)
	}
	b.Profile = profile.String
	b.Owner = owner.String
	b.Purpose = purpose.String
	b.Source = source.String
	b.EngineState = engineState.String
	b.InitSnapshot = initSnap.String
	b.VolumeRef = volRef.String
	if expires.Valid {
		if t, err := time.Parse(timeFmt, expires.String); err == nil {
			b.ExpiresAt = &t
		}
	}
	if t, err := time.Parse(timeFmt, created); err == nil {
		b.CreatedAt = t
	}
	if lastConn.Valid {
		if t, err := time.Parse(timeFmt, lastConn.String); err == nil {
			b.LastConnAt = &t
		}
	}
	if errorAt.Valid {
		if t, err := time.Parse(timeFmt, errorAt.String); err == nil {
			b.ErrorAt = &t
		}
	}
	return b, nil
}

// RecordHookStart は hook 実行開始を記録し、行 ID を返す。
func (d *DB) RecordHookStart(branch, event string) (int64, error) {
	res, err := d.sql.Exec(
		`INSERT INTO hook_runs (branch, event, started_at) VALUES (?, ?, ?)`,
		branch, event, time.Now().UTC().Format(timeFmt))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RecordHookFinish は hook 実行終了を記録する。
func (d *DB) RecordHookFinish(id int64, exitCode int) error {
	_, err := d.sql.Exec(
		`UPDATE hook_runs SET finished_at = ?, exit_code = ? WHERE id = ?`,
		time.Now().UTC().Format(timeFmt), exitCode, id)
	return err
}

// LastHookStatus はブランチの各 event の最新 exit code を返す(API の hook_status 用)。
func (d *DB) LastHookStatus(branch string) (map[string]string, error) {
	rows, err := d.sql.Query(
		`SELECT event, exit_code FROM hook_runs
		 WHERE branch = ? AND id IN (SELECT MAX(id) FROM hook_runs WHERE branch = ? GROUP BY event)`,
		branch, branch)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var event string
		var code sql.NullInt64
		if err := rows.Scan(&event, &code); err != nil {
			return nil, err
		}
		switch {
		case !code.Valid:
			out[event] = "running"
		case code.Int64 == 0:
			out[event] = "ok"
		default:
			out[event] = fmt.Sprintf("failed(%d)", code.Int64)
		}
	}
	return out, rows.Err()
}

// Token は tokens テーブルの 1 行。
type Token struct {
	Name       string
	Scope      string // "admin" | "branches"(#294)。旧行(NULL)は admin
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

// トークンのスコープ(#294)。
const (
	// ScopeAdmin は全操作。baseline の publish / promote、drain、gc、データブラウザ、
	// hook 手動実行を含む。loopback と env トークンはこれ。
	ScopeAdmin = "admin"
	// ScopeBranches はブランチのライフサイクルと読み取りだけ。CI / Action に配る想定。
	ScopeBranches = "branches"
)

// CreateToken はトークンのハッシュを保存する。同名は上書きしない。
// scope が空なら admin(旧 CLI 互換)。
func (d *DB) CreateToken(name, hash, scope string) error {
	if scope == "" {
		scope = ScopeAdmin
	}
	_, err := d.sql.Exec(
		`INSERT INTO tokens (name, hash, created_at, scope) VALUES (?, ?, ?, ?)`,
		name, hash, time.Now().UTC().Format(timeFmt), scope)
	return err
}

// RevokeToken は削除する。
func (d *DB) RevokeToken(name string) error {
	res, err := d.sql.Exec(`DELETE FROM tokens WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListTokens は一覧(ハッシュは返さない)。
func (d *DB) ListTokens() ([]Token, error) {
	rows, err := d.sql.Query(`SELECT name, created_at, last_used_at, scope FROM tokens ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Token
	for rows.Next() {
		var t Token
		var created string
		var lastUsed, scope sql.NullString
		if err := rows.Scan(&t.Name, &created, &lastUsed, &scope); err != nil {
			return nil, err
		}
		t.Scope = ScopeAdmin
		if scope.Valid && scope.String != "" {
			t.Scope = scope.String
		}
		if ts, err := time.Parse(timeFmt, created); err == nil {
			t.CreatedAt = ts
		}
		if lastUsed.Valid {
			if ts, err := time.Parse(timeFmt, lastUsed.String); err == nil {
				t.LastUsedAt = &ts
			}
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// LookupTokenHash はハッシュが登録済みなら name / scope を返し、last_used_at を
// 更新する。未登録なら ok=false。旧行(scope NULL)は admin として返す(#294)。
func (d *DB) LookupTokenHash(hash string) (name, scope string, ok bool, err error) {
	var sc sql.NullString
	err = d.sql.QueryRow(`SELECT name, scope FROM tokens WHERE hash = ?`, hash).Scan(&name, &sc)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	scope = ScopeAdmin
	if sc.Valid && sc.String != "" {
		scope = sc.String
	}
	_, _ = d.sql.Exec(`UPDATE tokens SET last_used_at = ? WHERE hash = ?`,
		time.Now().UTC().Format(timeFmt), hash)
	return name, scope, true, nil
}

// CheckTokenHash はハッシュが登録済みなら true を返す(scope を見ない旧 API)。
func (d *DB) CheckTokenHash(hash string) (bool, error) {
	_, _, ok, err := d.LookupTokenHash(hash)
	return ok, err
}

// BaselineProvenance は baseline の来歴(仕様 12-1)。schema の鮮度と data の
// 鮮度は一致しないため分けて持つ。
type BaselineProvenance struct {
	SourceRevision      string
	SchemaRevision      string
	DataAsOf            string
	Masked              bool
	MaskPipelineVersion string
	Validated           bool
}

// BaselineRow は baselines テーブルの 1 行。
type BaselineRow struct {
	Snapshot  string
	CreatedAt time.Time
	IsCurrent bool
	Prov      BaselineProvenance
}

// RegisterBaseline は snapshot を provenance 付きで登録する(current は変えない)。
func (d *DB) RegisterBaseline(snapshot string, p BaselineProvenance) error {
	_, err := d.sql.Exec(
		`INSERT INTO baselines (snapshot, created_at, is_current, source_revision, schema_revision,
		   data_as_of, masked, mask_pipeline_version, validated)
		 VALUES (?, ?, 0, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(snapshot) DO UPDATE SET source_revision=excluded.source_revision,
		   schema_revision=excluded.schema_revision, data_as_of=excluded.data_as_of,
		   masked=excluded.masked, mask_pipeline_version=excluded.mask_pipeline_version,
		   validated=excluded.validated`,
		snapshot, time.Now().UTC().Format(timeFmt),
		nullIfEmpty(p.SourceRevision), nullIfEmpty(p.SchemaRevision), nullIfEmpty(p.DataAsOf),
		boolToInt(p.Masked), nullIfEmpty(p.MaskPipelineVersion), boolToInt(p.Validated))
	return err
}

// SetCurrentBaseline は snapshot を登録して current に切り替える(pointer 更新)。
func (d *DB) SetCurrentBaseline(snapshot string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE baselines SET is_current = 0`); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO baselines (snapshot, created_at, is_current) VALUES (?, ?, 1)
		 ON CONFLICT(snapshot) DO UPDATE SET is_current = 1`,
		snapshot, time.Now().UTC().Format(timeFmt)); err != nil {
		return err
	}
	return tx.Commit()
}

// ListBaselines は登録済み baseline を新しい順に返す。
func (d *DB) ListBaselines() ([]BaselineRow, error) {
	rows, err := d.sql.Query(
		`SELECT snapshot, created_at, is_current, COALESCE(source_revision,''),
		        COALESCE(schema_revision,''), COALESCE(data_as_of,''), COALESCE(masked,0),
		        COALESCE(mask_pipeline_version,''), COALESCE(validated,0)
		 FROM baselines ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []BaselineRow
	for rows.Next() {
		var b BaselineRow
		var created string
		var cur, masked, validated int
		if err := rows.Scan(&b.Snapshot, &created, &cur, &b.Prov.SourceRevision,
			&b.Prov.SchemaRevision, &b.Prov.DataAsOf, &masked, &b.Prov.MaskPipelineVersion, &validated); err != nil {
			return nil, err
		}
		if t, err := time.Parse(timeFmt, created); err == nil {
			b.CreatedAt = t
		}
		b.IsCurrent = cur != 0
		b.Prov.Masked = masked != 0
		b.Prov.Validated = validated != 0
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetBaseline は 1 件取得。
func (d *DB) GetBaseline(snapshot string) (BaselineRow, error) {
	list, err := d.ListBaselines()
	if err != nil {
		return BaselineRow{}, err
	}
	for _, b := range list {
		if b.Snapshot == snapshot {
			return b, nil
		}
	}
	return BaselineRow{}, ErrNotFound
}

// BaselineRefCounts は各 baseline を origin にしている branch 数を返す。
func (d *DB) BaselineRefCounts() (map[string]int, error) {
	rows, err := d.sql.Query(`SELECT origin_snapshot, COUNT(*) FROM branches GROUP BY origin_snapshot`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var snap string
		var n int
		if err := rows.Scan(&snap, &n); err != nil {
			return nil, err
		}
		out[snap] = n
	}
	return out, rows.Err()
}

// DeleteBaseline は baselines 行を削除する(GC 用。current/参照中は呼び出し側で除外)。
func (d *DB) DeleteBaseline(snapshot string) error {
	_, err := d.sql.Exec(`DELETE FROM baselines WHERE snapshot = ?`, snapshot)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// CurrentBaselineOverride は DB に記録された current baseline を返す(無ければ false)。
func (d *DB) CurrentBaselineOverride() (string, bool) {
	var snap string
	err := d.sql.QueryRow(`SELECT snapshot FROM baselines WHERE is_current = 1`).Scan(&snap)
	if err != nil {
		return "", false
	}
	return snap, true
}

// Operation は operations テーブルの 1 行。
type Operation struct {
	ID         string
	Type       string
	Target     string
	State      string // running|completed|failed
	StartedAt  time.Time
	FinishedAt *time.Time
	Error      string // 人間可読メッセージ(error_json の message)
	ErrorCode  string // error_json の code(将来の機械判定用。現状は空)
}

// Operation の state 値。
const (
	OpRunning   = "running"
	OpCompleted = "completed"
	OpFailed    = "failed"
)

// CreateOperation は running 状態の operation を挿入する。
func (d *DB) CreateOperation(id, typ, target string) error {
	_, err := d.sql.Exec(
		`INSERT INTO operations (id, type, target, state, started_at) VALUES (?, ?, ?, ?, ?)`,
		id, typ, target, OpRunning, time.Now().UTC().Format(timeFmt))
	return err
}

// FinishOperation は operation を completed/failed にする。errMsg が空なら completed。
// error_json 列には列名どおり JSON({"code","message"})を入れる(#83)。errCode は
// 機械判定用の分類(失敗ステージ等、無ければ空)。
func (d *DB) FinishOperation(id, errCode, errMsg string) error {
	st := OpCompleted
	var e any
	if errMsg != "" {
		st = OpFailed
		b, _ := json.Marshal(map[string]string{"code": errCode, "message": errMsg})
		e = string(b)
	}
	_, err := d.sql.Exec(
		`UPDATE operations SET state = ?, finished_at = ?, error_json = ? WHERE id = ?`,
		st, time.Now().UTC().Format(timeFmt), e, id)
	return err
}

// RecoverInterruptedOperations は running のまま残った operation を failed にする。
// sashikid はシングルプロセスなので、起動時点で running の operation は前回の
// クラッシュ/再起動で中断されたもの。これを回収しないと `op wait` が
// タイムアウトまで永久に待つ(#53)。回収した件数を返す。
func (d *DB) RecoverInterruptedOperations() (int64, error) {
	b, _ := json.Marshal(map[string]string{"code": "interrupted", "message": "daemon restarted while operation was running"})
	res, err := d.sql.Exec(
		`UPDATE operations SET state = ?, finished_at = ?, error_json = ? WHERE state = ?`,
		OpFailed, time.Now().UTC().Format(timeFmt), string(b), OpRunning)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PruneHookRuns は finished_at が before より古い hook_runs を削除する(#295)。
// 各 (branch, event) の最新 1 件は LastHookStatus が参照するので残す。
func (d *DB) PruneHookRuns(before time.Time) (int64, error) {
	res, err := d.sql.Exec(
		`DELETE FROM hook_runs
		  WHERE finished_at IS NOT NULL AND finished_at < ?
		    AND id NOT IN (SELECT MAX(id) FROM hook_runs GROUP BY branch, event)`,
		before.UTC().Format(timeFmt))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PruneOperations は finished_at が before より古い完了/失敗 operation を削除し、
// 削除件数を返す(operations テーブルの無限成長を防ぐ。reaper から呼ぶ #83)。
func (d *DB) PruneOperations(before time.Time) (int64, error) {
	res, err := d.sql.Exec(
		`DELETE FROM operations WHERE state != ? AND finished_at IS NOT NULL AND finished_at < ?`,
		OpRunning, before.UTC().Format(timeFmt))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// GetOperation は 1 件取得。
func (d *DB) GetOperation(id string) (Operation, error) {
	row := d.sql.QueryRow(
		`SELECT id, type, target, state, started_at, finished_at, COALESCE(error_json,'')
		 FROM operations WHERE id = ?`, id)
	return scanOperation(row)
}

// ListOperations は新しい順に最大 limit 件返す。
func (d *DB) ListOperations(limit int) ([]Operation, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.sql.Query(
		`SELECT id, type, target, state, started_at, finished_at, COALESCE(error_json,'')
		 FROM operations ORDER BY started_at DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Operation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

func scanOperation(row scannable) (Operation, error) {
	var o Operation
	var started string
	var finished sql.NullString
	var errJSON string
	err := row.Scan(&o.ID, &o.Type, &o.Target, &o.State, &started, &finished, &errJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	if err != nil {
		return o, err
	}
	// error_json は {"code","message"} JSON。旧データ(生文字列)は message として扱う。
	if errJSON != "" {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(errJSON), &e) == nil && (e.Message != "" || e.Code != "") {
			o.Error, o.ErrorCode = e.Message, e.Code
		} else {
			o.Error = errJSON
		}
	}
	if t, err := time.Parse(timeFmt, started); err == nil {
		o.StartedAt = t
	}
	if finished.Valid {
		if t, err := time.Parse(timeFmt, finished.String); err == nil {
			o.FinishedAt = &t
		}
	}
	return o, nil
}

// OperationStat は operations の type×state ごとの集計(metrics 用)。
type OperationStat struct {
	Type        string
	State       string
	Count       int
	DurationSum float64 // 完了済み operation の合計所要秒
}

// OperationStats は operations を type×state で集計して返す。
func (d *DB) OperationStats() ([]OperationStat, error) {
	rows, err := d.sql.Query(
		`SELECT type, state, COUNT(*),
		        COALESCE(SUM(CASE WHEN finished_at IS NOT NULL
		          THEN (julianday(finished_at) - julianday(started_at)) * 86400.0
		          ELSE 0 END), 0)
		 FROM operations GROUP BY type, state ORDER BY type, state`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []OperationStat
	for rows.Next() {
		var s OperationStat
		if err := rows.Scan(&s.Type, &s.State, &s.Count, &s.DurationSum); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// HookFailureCount は失敗した hook 実行(exit_code 非 0)の累計を返す。
func (d *DB) HookFailureCount() (int, error) {
	var n int
	err := d.sql.QueryRow(
		`SELECT COUNT(*) FROM hook_runs WHERE exit_code IS NOT NULL AND exit_code != 0`).Scan(&n)
	return n, err
}
