// Package api は REST API(仕様 14-1)。プロキシ(v0.2)は含まない。
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rikukadev/sashiki/internal/hooks"
	"github.com/rikukadev/sashiki/internal/ops"
	"github.com/rikukadev/sashiki/internal/state"
	"github.com/rikukadev/sashiki/internal/workspace"
)

// TokenChecker は Bearer トークンの検証(state.db の tokens テーブル)。
type TokenChecker interface {
	LookupTokenHash(hash string) (name, scope string, ok bool, err error)
}

// principal は認証を通った呼び出し元(#294)。scope で admin 専用の経路を絞る。
type principal struct {
	Name  string // "loopback" / "env" / token 名
	Scope string // state.ScopeAdmin | state.ScopeBranches
}

type principalKey struct{}

// adminOnlyBranchPath は branches スコープに許さない branch 配下の経路。
// query / schema は app 資格情報で任意 SQL(Postgres は SUPERUSER、MySQL は
// GRANT ALL)を流せるので OS コマンド実行に等しい。hooks は hooks dir の
// 実行ファイルを起動する。
var adminOnlyBranchPath = regexp.MustCompile(`^/v1/branches/[^/]+/(query|schema|hooks/)`)

// adminOnly は admin スコープが要る要求か(#294)。branches スコープ(CI に配る
// トークン)にはブランチのライフサイクルと読み取りだけを許す。
func adminOnly(r *http.Request) bool {
	p := r.URL.Path
	if adminOnlyBranchPath.MatchString(p) {
		return true
	}
	if r.Method != http.MethodPost {
		return false
	}
	return strings.HasPrefix(p, "/v1/baseline/") || strings.HasPrefix(p, "/v1/gc/") || p == "/v1/drain"
}

// Server は REST API サーバー。
type Server struct {
	mgr    *workspace.Manager
	domain string
	engine string // mysql | postgres(データブラウザは両方対応)
	user   string
	pass   string
	token  string // 環境変数トークン(後方互換)。空なら無効
	tokens TokenChecker
	openDB func(ctx context.Context, name string) (*sql.DB, error) // テストで差し替え可
	ops    *ops.Runner                                             // nil 可(operation 記録なし)
	mux    *http.ServeMux
	// proxyPort は固定エンドポイント(listen.proxy)のポート。0 は proxy 無効。
	// 接続情報(host/port/user)はこれを見て proxy 宛か直結かを切り替える(#260)。
	proxyPort int
	// trustLoopback が true(既定)なら loopback を無認証で通す。false なら
	// loopback でも Bearer トークン必須(リバプロ公開時の素通し防止、#198)。
	trustLoopback bool
}

// SetTrustLoopback は loopback 無認証の可否を設定する(sashikid 起動時)。
func (s *Server) SetTrustLoopback(v bool) { s.trustLoopback = v }

// SetProxyListen は固定エンドポイントの listen アドレス("0.0.0.0:3306" 等)を
// 渡して、接続情報に載せるポートを決める(sashikid 起動時)。空文字なら proxy
// 無効として扱い、接続情報はブランチへ直結する形になる。
//
// 解釈できない値でも起動は止めない。API が上がらないより「直結の接続情報が
// 返る」方が気づいて直せる。
func (s *Server) SetProxyListen(addr string) {
	if addr == "" {
		s.proxyPort = 0
		return
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		log.Printf("api: listen.proxy %q を解釈できません。接続情報はブランチ直結として返します", addr)
		s.proxyPort = 0
		return
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		log.Printf("api: listen.proxy %q のポートが数値ではありません。接続情報はブランチ直結として返します", addr)
		s.proxyPort = 0
		return
	}
	s.proxyPort = n
}

// Ops は配線済みの operation Runner を返す(停止時の Drain 用。未配線なら nil)。
func (s *Server) Ops() *ops.Runner { return s.ops }

// SetOps は operation Runner を配線する(sashikid 起動時)。
func (s *Server) SetOps(r *ops.Runner) {
	s.ops = r
	s.mux.HandleFunc("GET /v1/operations", s.handleListOps)
	s.mux.HandleFunc("GET /v1/operations/{id}", s.handleGetOp)
}

// track は typ/target の operation を記録しつつ fn を同期実行する。
// operation_id を返す(ops 未配線なら空)。同期のまま残す操作(wake 等)で使う。
func (s *Server) track(typ, target string, fn func() error) (string, error) {
	if s.ops == nil {
		return "", fn()
	}
	return s.ops.RunSync(typ, target, func(context.Context) error { return fn() })
}

// accepted は変更操作を非同期 operation として開始し、202 + operation_id を返す
// (仕様 17章 / #82)。fsx-zfs で数分かかる操作でも HTTP を待たせない。CLI は
// 既定で --wait し、operation の完了をポーリングして結果を取得する。
// ops 未配線(テスト等)のときは同期実行して 202 を返す。
func (s *Server) accepted(w http.ResponseWriter, typ, target string, fn func(context.Context) error) {
	if s.ops == nil {
		if err := fn(context.Background()); err != nil {
			s.writeError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"operation_id": "", "status": "accepted"})
		return
	}
	// ブランチ操作は同名に実行中の operation があれば 409(#303)。baseline 検証は
	// 対象が snapshot なので従来どおり。
	start := s.ops.Start
	if typ != "baseline-validate" {
		start = s.ops.StartExclusive
	}
	opID, err := start(typ, target, fn)
	if errors.Is(err, ops.ErrInProgress) {
		setOpID(w, opID)
		writeErr(w, http.StatusConflict, "operation_in_progress", err.Error())
		return
	}
	if err != nil {
		s.writeError(w, err)
		return
	}
	setOpID(w, opID)
	writeJSON(w, http.StatusAccepted, map[string]any{"operation_id": opID, "status": "accepted"})
}

// New は Server を作る。tokens は nil 可(env トークンのみ)。
func New(mgr *workspace.Manager, domain, engineType, proxyUser, proxyPass, token string, tokens TokenChecker) *Server {
	s := &Server{mgr: mgr, domain: domain, engine: engineType, user: proxyUser, pass: proxyPass, token: token, tokens: tokens, mux: http.NewServeMux(), trustLoopback: true}
	s.openDB = s.branchDB
	s.mux.HandleFunc("GET /v1/branches", s.handleList)
	s.mux.HandleFunc("POST /v1/branches", s.handleCreate)
	s.mux.HandleFunc("GET /v1/branches/{name}", s.handleGet)
	s.mux.HandleFunc("POST /v1/branches/{name}/reset", s.handleReset)
	s.mux.HandleFunc("POST /v1/branches/{name}/recreate", s.handleRecreate)
	s.mux.HandleFunc("POST /v1/branches/{name}/wake", s.handleWake)
	s.mux.HandleFunc("POST /v1/branches/{name}/sleep", s.handleSleep)
	s.mux.HandleFunc("POST /v1/branches/{name}/retry", s.handleRetry)
	s.mux.HandleFunc("POST /v1/branches/{name}/lease", s.handleLease)
	s.mux.HandleFunc("POST /v1/branches/{name}/hooks/{event}", s.handleRunHook)
	s.mux.HandleFunc("GET /v1/branches/{name}/schema", s.handleSchema)
	s.mux.HandleFunc("POST /v1/branches/{name}/query", s.handleQuery)
	s.mux.HandleFunc("DELETE /v1/branches/{name}", s.handleDelete)
	s.mux.HandleFunc("GET /v1/baseline", s.handleBaseline)
	s.mux.HandleFunc("POST /v1/baseline/refresh", s.handleBaselineRefresh)
	s.mux.HandleFunc("GET /v1/baselines", s.handleListBaselines)
	s.mux.HandleFunc("POST /v1/baseline/set", s.handleSetBaseline)
	s.mux.HandleFunc("POST /v1/baseline/gc", s.handleGCBaselines)
	// 段階 API(#84): build → validate → publish → delete。refresh(一括)は互換維持。
	s.mux.HandleFunc("POST /v1/baseline/build", s.handleBaselineBuild)
	s.mux.HandleFunc("POST /v1/baseline/validate", s.handleBaselineValidate)
	s.mux.HandleFunc("POST /v1/baseline/publish", s.handleSetBaseline) // publish == set(policy 検査つき)
	s.mux.HandleFunc("POST /v1/baseline/delete", s.handleBaselineDelete)
	s.mux.HandleFunc("POST /v1/baseline/promote", s.handleBaselinePromote)
	s.mux.HandleFunc("GET /v1/capacity", s.handleCapacity)
	s.mux.HandleFunc("GET /v1/doctor", s.handleDoctor)
	s.mux.HandleFunc("POST /v1/gc/orphans", s.handleGCOrphans)
	s.mux.HandleFunc("POST /v1/drain", s.handleDrain)
	s.mux.HandleFunc("GET /v1/healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /", s.handleWebUI)
	return s
}

// ServeHTTP は認証を通してからルーティングする。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 認証の外に置くもの(#301):
	//   - GET /v1/healthz: LB / ALB のヘルスチェックはトークンを持たない。中身は
	//     {"status":"ok"} だけで情報を出さない
	//   - GET /: Web UI の HTML そのもの。データは含まず、UI はトークンを
	//     入力させてから API を叩く(trust_loopback: false でも使えるように)
	if r.Method == http.MethodGet && (r.URL.Path == "/v1/healthz" || r.URL.Path == "/") {
		s.mux.ServeHTTP(w, r)
		return
	}
	p, ok := s.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid token")
		return
	}
	if p.Scope != state.ScopeAdmin && adminOnly(r) {
		writeErr(w, http.StatusForbidden, "insufficient_scope",
			fmt.Sprintf("token %q (scope %s) cannot %s %s: admin scope required", p.Name, p.Scope, r.Method, r.URL.Path))
		return
	}
	s.mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
}

// callerOf は要求の principal(無ければ空)を返す。監査ログ用。
func callerOf(r *http.Request) principal {
	if p, ok := r.Context().Value(principalKey{}).(principal); ok {
		return p
	}
	return principal{}
}

// authorized: 認証を通るか(scope は見ない)。
func (s *Server) authorized(r *http.Request) bool {
	_, ok := s.authenticate(r)
	return ok
}

// authenticate: localhost からは無認証(admin)、それ以外は Bearer トークン(仕様 13-3)。
// env トークン(SASHIKI_API_TOKEN / Terraform 生成)は後方互換で admin。
// state.db のトークンは発行時の scope を持つ(#294)。
func (s *Server) authenticate(r *http.Request) (principal, bool) {
	if s.trustLoopback {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
				return principal{Name: "loopback", Scope: state.ScopeAdmin}, true
			}
		}
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" {
		return principal{}, false
	}
	gh := sha256.Sum256([]byte(got))
	if s.token != "" {
		th := sha256.Sum256([]byte(s.token))
		if subtle.ConstantTimeCompare(gh[:], th[:]) == 1 {
			return principal{Name: "env", Scope: state.ScopeAdmin}, true
		}
	}
	if s.tokens != nil {
		name, scope, ok, err := s.tokens.LookupTokenHash(hex.EncodeToString(gh[:]))
		if err != nil {
			// DB 障害を無言の 401 にしない(認証失敗とは区別してログに残す)
			log.Printf("api: token check failed: %v", err)
			return principal{}, false
		}
		if ok {
			return principal{Name: name, Scope: scope}, true
		}
	}
	return principal{}, false
}

// --- handlers ---

func (s *Server) handleBaseline(w http.ResponseWriter, r *http.Request) {
	info, err := s.mgr.Baseline(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	if info.Snapshots == nil {
		info.Snapshots = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current":            info.Current,
		"snapshots":          info.Snapshots,
		"refreshing":         workspace.RefreshInProgress(),
		"last_refresh_error": workspace.RefreshLastError(),
	})
}

func (s *Server) handleBaselineRefresh(w http.ResponseWriter, r *http.Request) {
	tag, err := s.mgr.RefreshBaseline(r.Context(), workspace.RefreshConfig{})
	if errors.Is(err, workspace.ErrRefreshRunning) {
		// 仕様 17章のコード名に統一(旧 refresh_running)
		writeErr(w, http.StatusConflict, "operation_in_progress", err.Error())
		return
	}
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started", "tag": tag})
}

func (s *Server) handleListBaselines(w http.ResponseWriter, r *http.Request) {
	rows, err := s.mgr.ListBaselineRows()
	if err != nil {
		s.writeError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, b := range rows {
		out = append(out, map[string]any{
			"snapshot":        b.Snapshot,
			"created_at":      b.CreatedAt.UTC().Format(time.RFC3339),
			"is_current":      b.IsCurrent,
			"schema_revision": b.Prov.SchemaRevision,
			"data_as_of":      b.Prov.DataAsOf,
			"masked":          b.Prov.Masked,
			"validated":       b.Prov.Validated,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"baselines": out})
}

// handleBaselineBuild は build 段階を非同期実行し、202 + {operation_id, tag, snapshot} を返す(#84)。
func (s *Server) handleBaselineBuild(w http.ResponseWriter, r *http.Request) {
	tag, snap := s.mgr.PrepareBuild()
	body := map[string]any{"tag": tag, "snapshot": snap}
	if s.ops == nil {
		if _, err := s.mgr.BuildBaseline(r.Context(), tag); err != nil {
			s.writeError(w, err)
			return
		}
		body["operation_id"] = ""
		writeJSON(w, http.StatusAccepted, body)
		return
	}
	opID, err := s.ops.Start("baseline-build", tag, func(ctx context.Context) error {
		_, e := s.mgr.BuildBaseline(ctx, tag)
		return e
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	setOpID(w, opID)
	body["operation_id"] = opID
	writeJSON(w, http.StatusAccepted, body)
}

// handleBaselineValidate は candidate を検証する(非同期、#84)。body {snapshot}。
func (s *Server) handleBaselineValidate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Snapshot string `json:"snapshot"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Snapshot == "" {
		writeErr(w, http.StatusBadRequest, "invalid_name", "body must be {\"snapshot\": \"...\"}")
		return
	}
	s.accepted(w, "baseline-validate", req.Snapshot, func(ctx context.Context) error {
		return s.mgr.ValidateBaseline(ctx, req.Snapshot)
	})
}

// handleBaselineDelete は candidate を削除する(#84)。current・参照中は 412。body {snapshot}。
func (s *Server) handleBaselineDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Snapshot string `json:"snapshot"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Snapshot == "" {
		writeErr(w, http.StatusBadRequest, "invalid_name", "body must be {\"snapshot\": \"...\"}")
		return
	}
	if err := s.mgr.DeleteBaseline(r.Context(), req.Snapshot); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": req.Snapshot})
}

// handleBaselinePromote は既存ブランチを新 baseline に昇格する(#129)。
// body {branch, masked?, skip_validate?}。branch でマイグレーション済みの状態を
// そのまま current baseline にする。masked は require_masked 用の宣言(#296)。
func (s *Server) handleBaselinePromote(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Branch       string `json:"branch"`
		Masked       bool   `json:"masked"`
		SkipValidate bool   `json:"skip_validate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Branch == "" {
		writeErr(w, http.StatusBadRequest, "invalid_name", "body must be {\"branch\": \"...\"}")
		return
	}
	snap, err := s.mgr.PromoteBranch(r.Context(), req.Branch, workspace.PromoteOptions{
		Masked: req.Masked, SkipValidate: req.SkipValidate,
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"from": req.Branch, "current": snap})
}

func (s *Server) handleSetBaseline(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Snapshot string `json:"snapshot"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Snapshot == "" {
		writeErr(w, http.StatusBadRequest, "invalid_name", "body must be {\"snapshot\": \"...\"}")
		return
	}
	if err := s.mgr.SetBaseline(r.Context(), req.Snapshot); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"current": req.Snapshot})
}

func (s *Server) handleGCBaselines(w http.ResponseWriter, r *http.Request) {
	gc := s.mgr.BaselineGCConfig() // config 由来の keep_last / retention 既定
	if v := r.URL.Query().Get("keep_last"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "invalid_name", "keep_last must be a non-negative integer")
			return
		}
		gc.KeepLast = n
	}
	if r.URL.Query().Get("dry_run") == "true" {
		gc.DryRun = true
	}
	res, err := s.mgr.GCBaselines(r.Context(), gc)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": res.Deleted, "kept": res.Kept, "dry_run": gc.DryRun})
}

func (s *Server) handleCapacity(w http.ResponseWriter, r *http.Request) {
	c, err := s.mgr.Capacity(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	// pool 容量を取得できないバックエンド(apfs / reflink)は 0 でなく -1(不明)を
	// 返し、表示側で「-」を出す(#128、誤った 0 を見せない)。
	poolUsed, poolTotal, poolRatio := c.PoolUsedBytes, c.PoolTotalBytes, c.PoolUsedRatio
	if !c.StorageIntrospectable {
		poolUsed, poolTotal, poolRatio = -1, -1, -1
	}
	memAvail := c.MemAvailableBytes
	if memAvail <= 0 {
		memAvail = -1
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"storage": map[string]any{
			"introspectable":     c.StorageIntrospectable,
			"pool_used_bytes":    poolUsed,
			"pool_total_bytes":   poolTotal,
			"pool_used_ratio":    poolRatio,
			"high_watermark":     c.HighWatermark,
			"critical_watermark": c.CritWatermark,
		},
		"ports":    map[string]any{"used": c.PortsUsed, "total": c.PortsTotal},
		"memory":   map[string]any{"available_bytes": memAvail, "expected_rss_bytes": c.ExpectedRSSBytes},
		"branches": map[string]any{"running": c.Running, "max_running": c.MaxRunning, "max_branches": c.MaxBranches},
	})
}

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	d, err := s.mgr.Doctor(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pool_healthy":        d.PoolHealthy,
		"pool_status_healthy": d.PoolStatusHealthy,
		"pool_status_detail":  d.PoolStatusDetail,
		"pool_used_ratio":     d.PoolUsedRatio,
		"current_baseline":    d.CurrentBaseline,
		"baseline_masked":     d.BaselineMasked,
		"baseline_validated":  d.BaselineValidated,
		"state_db_writable":   d.StateDBWritable,
		"branch_count":        d.BranchCount,
		"port_conflicts":      d.PortConflicts,
		"exposed_listeners":   d.ExposedListeners,
		"orphans":             d.Orphans,
		"memory_headroom_ok":  d.MemHeadroomOK,
		"issues":              d.Issues,
		"checks":              d.Checks,
	})
}

// checkBranchMutation は非同期 operation を作る前に、同期で判定できる拒否条件を
// 返す。operation 内だけで ErrPreconditionFailed にすると HTTP は 202 になり、
// README/API の 412 契約を満たせない(#289)。Manager 側の検査も競合対策として残す。
func (s *Server) checkBranchMutation(w http.ResponseWriter, r *http.Request, name, verb string) bool {
	if err := s.mgr.CheckBranchMutation(r.Context(), name, verb); err != nil {
		s.writeError(w, err)
		return false
	}
	return true
}

func (s *Server) handleGCOrphans(w http.ResponseWriter, r *http.Request) {
	deleted, err := s.mgr.GCOrphans(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

// handleDrain は全 running branch を sleeping にする(POST /v1/drain, 仕様17章)。
// instance_class 変更前などに mysqld を安全に落とすのに使う。
func (s *Server) handleDrain(w http.ResponseWriter, r *http.Request) {
	res, err := s.mgr.Drain(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"slept": res.Slept, "skipped": res.Skipped, "failed": res.Failed,
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	infos, err := s.mgr.List(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	out := make([]branchJSON, 0, len(infos))
	for _, i := range infos {
		out = append(out, s.toJSON(i))
	}
	writeJSON(w, http.StatusOK, map[string]any{"branches": out})
}

type createReq struct {
	Name     string          `json:"name"`
	Port     int             `json:"port,omitempty"`
	Profile  string          `json:"profile,omitempty"`
	Owner    string          `json:"owner,omitempty"`
	Purpose  string          `json:"purpose,omitempty"`
	Source   json.RawMessage `json:"source,omitempty"`
	TTL      string          `json:"ttl,omitempty"`      // 初期 lease 期限(例 "7d","1h"）。空なら無期限
	Baseline string          `json:"baseline,omitempty"` // 作成元 baseline snapshot。空なら current(#82)
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_name", "invalid request body")
		return
	}
	// 名前は同期で検証して即 400(非同期 op に落とさない)。
	if !s.mgr.ValidName(req.Name) {
		s.writeError(w, workspace.ErrInvalidName)
		return
	}
	existOK := r.URL.Query().Get("exist_ok") == "true"
	var ttl time.Duration
	if req.TTL != "" {
		var perr error
		if ttl, perr = parseDur(req.TTL); perr != nil {
			writeErr(w, http.StatusBadRequest, "invalid_name", "invalid ttl: "+perr.Error())
			return
		}
	}
	// 既存チェック(#82: 非同期化の前段)。既にあれば同期で返す:
	// exist_ok なら 200+branch(冪等)、そうでなければ 409。
	if info, gerr := s.mgr.Get(r.Context(), req.Name); gerr == nil {
		if existOK {
			writeJSON(w, http.StatusOK, s.toJSON(info))
			return
		}
		s.writeError(w, workspace.ErrExists)
		return
	}
	// 新規作成は非同期(fsx で数分)。202 + operation_id を返し、CLI が --wait で追う。
	meta := state.Meta{Profile: req.Profile, Owner: req.Owner, Purpose: req.Purpose, Source: string(req.Source)}
	s.accepted(w, "create", req.Name, func(ctx context.Context) error {
		if _, e := s.mgr.CreateWithMetaFrom(ctx, req.Name, req.Port, meta, req.Baseline); e != nil {
			return e
		}
		if ttl > 0 {
			if _, e := s.mgr.Lease(ctx, req.Name, ttl); e != nil {
				return e
			}
		}
		return nil
	})
}

type leaseReq struct {
	For string `json:"for"` // 追加する期間(例 "7d")。now からの新しい expires_at を設定
}

// handleLease は lease を renew する(POST /v1/branches/{name}/lease)。
// expires_at を now+for に(再)設定する。冪等ではなく「今から for 後まで延長」。
func (s *Server) handleLease(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req leaseReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.For == "" {
		writeErr(w, http.StatusBadRequest, "invalid_name", "invalid request body (need {\"for\":\"7d\"})")
		return
	}
	d, perr := parseDur(req.For)
	if perr != nil {
		writeErr(w, http.StatusBadRequest, "invalid_name", "invalid for: "+perr.Error())
		return
	}
	var info workspace.Info
	opID, err := s.track("lease", name, func() error {
		if _, e := s.mgr.Lease(r.Context(), name, d); e != nil {
			return e
		}
		var e error
		info, e = s.mgr.Get(r.Context(), name)
		return e
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	setOpID(w, opID)
	writeJSON(w, http.StatusOK, s.toJSON(info))
}

// parseDur は time.ParseDuration に加えて末尾 d(日)/w(週)を許す。
func parseDur(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if n := len(s); n >= 2 {
		switch s[n-1] {
		case 'd', 'w':
			num, err := strconv.ParseFloat(s[:n-1], 64)
			if err != nil {
				return 0, fmt.Errorf("invalid duration %q", s)
			}
			base := 24 * time.Hour
			if s[n-1] == 'w' {
				base = 7 * 24 * time.Hour
			}
			return time.Duration(num * float64(base)), nil
		}
	}
	return time.ParseDuration(s)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	info, err := s.mgr.Get(r.Context(), r.PathValue("name"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toJSON(info))
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.checkBranchMutation(w, r, name, "reset") {
		return
	}
	s.accepted(w, "reset", name, func(ctx context.Context) error {
		_, e := s.mgr.Reset(ctx, name)
		return e
	})
}

func (s *Server) handleRecreate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.checkBranchMutation(w, r, name, "recreate") {
		return
	}
	s.accepted(w, "recreate", name, func(ctx context.Context) error {
		_, e := s.mgr.Recreate(ctx, name)
		return e
	})
}

func (s *Server) handleWake(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var info workspace.Info
	opID, err := s.track("wake", name, func() error {
		var e error
		info, e = s.mgr.Wake(r.Context(), name)
		return e
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	setOpID(w, opID)
	writeJSON(w, http.StatusOK, s.toJSON(info))
}

// handleSleep は running → sleeping(mysqld を正常終了)。高速なので同期(#87)。
func (s *Server) handleSleep(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var info workspace.Info
	opID, err := s.track("sleep", name, func() error {
		if e := s.mgr.Sleep(r.Context(), name); e != nil {
			return e
		}
		var e error
		info, e = s.mgr.Get(r.Context(), name)
		return e
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	setOpID(w, opID)
	writeJSON(w, http.StatusOK, s.toJSON(info))
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// reset / recreate / delete と同じく、存在確認と promote 元の保護は operation を
	// 作る前に同期で返す(#323: retry だけ 202 → op failed で、404 が CLI の 1 になっていた)。
	if !s.checkBranchMutation(w, r, name, "retry") {
		return
	}
	s.accepted(w, "retry", name, func(ctx context.Context) error {
		_, e := s.mgr.Retry(ctx, name)
		return e
	})
}

func (s *Server) handleRunHook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	event := hooks.Event(r.PathValue("event"))
	// 誰が何を走らせたか残す(#294)。
	log.Printf("api: hook run branch=%s event=%s by=%s", name, event, callerOf(r).Name)
	if err := s.mgr.RunHookManually(r.Context(), name, event); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ran", "event": string(event)})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// 存在確認と promote 元保護を operation 作成前に行い、404 / 412 を同期で返す。
	if !s.checkBranchMutation(w, r, name, "削除") {
		return
	}
	s.accepted(w, "delete", name, func(ctx context.Context) error {
		return s.mgr.Delete(ctx, name)
	})
}

func setOpID(w http.ResponseWriter, id string) {
	if id != "" {
		w.Header().Set("Sashiki-Operation-Id", id)
	}
}

func (s *Server) handleListOps(w http.ResponseWriter, r *http.Request) {
	list, err := s.ops.List(50)
	if err != nil {
		s.writeError(w, err)
		return
	}
	out := make([]opJSON, 0, len(list))
	for _, o := range list {
		out = append(out, toOpJSON(o))
	}
	writeJSON(w, http.StatusOK, map[string]any{"operations": out})
}

func (s *Server) handleGetOp(w http.ResponseWriter, r *http.Request) {
	op, err := s.ops.Get(r.PathValue("id"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toOpJSON(op))
}

type opJSON struct {
	ID         string  `json:"operation_id"`
	Type       string  `json:"type"`
	Target     string  `json:"target"`
	State      string  `json:"state"`
	StartedAt  string  `json:"started_at"`
	FinishedAt *string `json:"finished_at,omitempty"`
	Error      string  `json:"error,omitempty"`
	ErrorCode  string  `json:"error_code,omitempty"`
}

func toOpJSON(o state.Operation) opJSON {
	j := opJSON{
		ID: o.ID, Type: o.Type, Target: o.Target, State: o.State,
		StartedAt: o.StartedAt.UTC().Format(time.RFC3339), Error: o.Error, ErrorCode: o.ErrorCode,
	}
	if o.FinishedAt != nil {
		f := o.FinishedAt.UTC().Format(time.RFC3339)
		j.FinishedAt = &f
	}
	return j
}

// --- serialization ---

type branchJSON struct {
	Name        string `json:"name"`
	State       string `json:"state"`
	EngineState string `json:"engine_state"`
	// Port / Host / User は **そのまま接続に使える** 3 つ組であること。
	// proxy が有効なら proxy 宛(固定ポート + dev@<branch>)、無効ならブランチ直結
	// (内部ポート + dev)になる。混ぜると「どう解釈しても繋がらない」値になる(#260)。
	Port int    `json:"port"`
	Host string `json:"host"`
	User string `json:"user"`
	// EnginePort はブランチ自身の listener。接続用ではなく doctor / デバッグ用。
	EnginePort     int               `json:"engine_port,omitempty"`
	OriginSnapshot string            `json:"origin_snapshot"`
	CreatedAt      string            `json:"created_at"`
	LastConnAt     *string           `json:"last_conn_at,omitempty"`
	UsedBytes      int64             `json:"used_bytes"`
	LogicalBytes   int64             `json:"logical_bytes,omitempty"`
	HookStatus     map[string]string `json:"hook_status,omitempty"`
	Error          string            `json:"error,omitempty"`
	FailedOp       string            `json:"failed_operation,omitempty"`
	ErrorCode      string            `json:"error_code,omitempty"`
	Recoverable    bool              `json:"recoverable,omitempty"`
	Suggestions    []string          `json:"suggested_actions,omitempty"`
	Profile        string            `json:"profile,omitempty"`
	Owner          string            `json:"owner,omitempty"`
	Purpose        string            `json:"purpose,omitempty"`
	Source         json.RawMessage   `json:"source,omitempty"`
	ExpiresAt      *string           `json:"expires_at,omitempty"`
	Stale          bool              `json:"stale,omitempty"` // origin < current baseline(#130)
	// BackingBaselines: promote 元として baseline 実体を保持している(#179 / #289)。
	// 空でなければ reset / recreate / delete は 412 で拒否される。
	BackingBaselines []string `json:"backing_baselines,omitempty"`
	// Engine は接続方法を決めるのに要る(mysql か postgres か)。CLI はこれを見て
	// 表示する接続コマンドを選ぶ(#238 の実機検証で postgres でも mysql と案内
	// していたのが分かったため)。
	Engine string `json:"engine,omitempty"`
}

// connPort / connUser は「接続に使う値」を返す。proxy が有効なら固定エンドポイント
// (:3306 等)と `dev@<branch>` でルーティングし、無効ならブランチの内部ポートへ
// 直結して user は `dev` のまま(直結先の mysqld に dev@<branch> は存在しない)。
func (s *Server) connPort(enginePort int) int {
	if s.proxyPort != 0 {
		return s.proxyPort
	}
	return enginePort
}

func (s *Server) connUser(branch string) string {
	if s.proxyPort != 0 {
		return s.user + "@" + branch
	}
	return s.user
}

func (s *Server) toJSON(i workspace.Info) branchJSON {
	b := branchJSON{
		Name:             i.Name,
		State:            i.State,
		EngineState:      i.EngineState,
		Port:             s.connPort(i.Port),
		Host:             s.domain,
		User:             s.connUser(i.Name),
		EnginePort:       i.Port,
		Engine:           s.engine,
		OriginSnapshot:   i.OriginSnapshot,
		CreatedAt:        i.CreatedAt.UTC().Format(time.RFC3339),
		UsedBytes:        i.UsedBytes,
		LogicalBytes:     i.LogicalBytes,
		HookStatus:       i.HookStatus,
		Error:            i.ErrorMessage,
		FailedOp:         i.FailedOp,
		ErrorCode:        i.ErrorCode,
		Recoverable:      i.Recoverable,
		Suggestions:      i.SuggestedActions,
		Profile:          i.Profile,
		Owner:            i.Owner,
		Purpose:          i.Purpose,
		Stale:            i.Stale,
		BackingBaselines: i.BackingBaselines,
	}
	if i.Source != "" {
		b.Source = json.RawMessage(i.Source)
	}
	if i.ExpiresAt != nil {
		e := i.ExpiresAt.UTC().Format(time.RFC3339)
		b.ExpiresAt = &e
	}
	if i.LastConnAt != nil {
		t := i.LastConnAt.UTC().Format(time.RFC3339)
		b.LastConnAt = &t
	}
	return b
}

func (s *Server) writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnsupportedEngine):
		writeErr(w, http.StatusNotImplemented, "unsupported_engine", err.Error())
	case errors.Is(err, workspace.ErrInvalidName):
		writeErr(w, http.StatusBadRequest, "invalid_name", err.Error())
	case errors.Is(err, workspace.ErrExists):
		writeErr(w, http.StatusConflict, "branch_exists", err.Error())
	case errors.Is(err, workspace.ErrBaselineNotFound):
		writeErr(w, http.StatusNotFound, "baseline_not_found", err.Error())
	case errors.Is(err, state.ErrNotFound):
		writeErr(w, http.StatusNotFound, "branch_not_found", err.Error())
	case errors.Is(err, workspace.ErrLimitReached), errors.Is(err, workspace.ErrNoFreePort):
		writeErr(w, http.StatusInsufficientStorage, "limit_reached", err.Error())
	case errors.Is(err, workspace.ErrPreconditionFailed):
		writeErr(w, http.StatusPreconditionFailed, "precondition_failed", err.Error())
	case errors.Is(err, workspace.ErrHookFailed):
		writeErr(w, http.StatusInternalServerError, "hook_failed", err.Error())
	case errors.Is(err, workspace.ErrEngineFailed):
		writeErr(w, http.StatusInternalServerError, "engine_error", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "storage_error", err.Error())
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, ecode, msg string) {
	writeJSON(w, code, map[string]any{
		"error": map[string]string{"code": ecode, "message": msg},
	})
}

// Listen はサーバーを起動する。
func (s *Server) Listen(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		return nil
	case err := <-errCh:
		return fmt.Errorf("api server: %w", err)
	}
}
