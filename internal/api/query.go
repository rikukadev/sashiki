// ブランチ DB への問い合わせ API(Web UI のデータブラウザ用)。
//
//	GET  /v1/branches/{name}/schema  スキーマ(DB → テーブル → カラム)
//	POST /v1/branches/{name}/query   任意 SQL の実行(行数・時間・セルサイズに上限)
//
// ブランチは使い捨ての開発 DB であり、接続ユーザーも開発用(proxy_user)なので
// 書き込みも許可する(壊したら reset すればよい)。MySQL エンジンのみ対応。
//
// 任意 SQL 実行は loopback 無認証(仕様 13-3)の配下に入るため、SSH トンネルで
// UI を使う開発者のブラウザ経由の攻撃を browserSafe で遮断する(CSRF / DNS
// リバインディング。詳細は関数コメント)。
package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/rikukadev/sashiki/internal/state"
)

const (
	queryTimeout = 10 * time.Second
	maxRows      = 200
	maxCellBytes = 64 << 10 // 1 セルの上限。LONGBLOB 等で応答が肥大するのを防ぐ
)

// ErrUnsupportedEngine はデータブラウザ未対応エンジンへの要求。
var ErrUnsupportedEngine = errors.New("data browser supports the mysql and postgres engines only")

func (s *Server) branchDB(ctx context.Context, name string) (*sql.DB, error) {
	switch s.engine {
	case "", "mysql", "postgres":
	default:
		return nil, ErrUnsupportedEngine
	}
	info, err := s.mgr.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if info.State != state.StateRunning {
		return nil, fmt.Errorf("branch %s is %s (not running)", name, info.State)
	}
	if s.engine == "postgres" {
		// PostgreSQL の接続は 1 DB に束縛されるので、見せる DB を選んでから繋ぐ(#228)。
		dbname, err := pgBrowseDatabase(ctx, s.user, s.pass, info.Port)
		if err != nil {
			return nil, err
		}
		return pgOpen(s.user, s.pass, info.Port, dbname)
	}
	// 資格情報に記号が入っても壊れないよう FormatDSN で組む
	mcfg := mysql.NewConfig()
	mcfg.User = s.user
	mcfg.Passwd = s.pass
	mcfg.Net = "tcp"
	mcfg.Addr = fmt.Sprintf("127.0.0.1:%d", info.Port)
	mcfg.ReadTimeout = queryTimeout
	mcfg.MultiStatements = false
	db, err := sql.Open("mysql", mcfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	return db, nil
}

// loopbackHost は Host / Origin のホスト部が localhost 系かどうか。
func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// browserSafe はデータブラウザ系エンドポイントのブラウザ経由攻撃対策。
// SSH トンネル運用(localhost で UI を開く)では loopback 無認証で任意 SQL が
// 実行できてしまうため、
//   - Origin ヘッダ付き(=ブラウザの cross-site 要求)は localhost 系のみ許可(CSRF)
//   - loopback からの要求は Host も localhost 系のみ許可(DNS リバインディング。
//     トークン認証で来る非 loopback の API クライアントには影響しない)
//
// を強制する。ダメなら 403 を書いて false を返す。
//
// Bearer トークンで認証された要求は、同一オリジン(Origin のホスト == Host)も
// 許可する(#301)。トークンはブラウザが自動では送らないヘッダなので CSRF にならず、
// DNS リバインディングの攻撃ページは別オリジンでトークンを持てない。これで
// Web UI を SSH トンネル以外(trust_loopback: false の内部公開)でも使える。
func (s *Server) browserSafe(w http.ResponseWriter, r *http.Request) bool {
	tokenAuth := callerOf(r).Name != "" && callerOf(r).Name != "loopback"
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		sameOrigin := err == nil && tokenAuth && strings.EqualFold(u.Host, r.Host)
		if err != nil || (!loopbackHost(u.Host) && !sameOrigin) {
			writeErr(w, http.StatusForbidden, "cross_origin_denied",
				"cross-site browser requests are not allowed for the data browser")
			return false
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() && !loopbackHost(r.Host) {
			writeErr(w, http.StatusForbidden, "host_mismatch",
				fmt.Sprintf("unexpected Host %q on a loopback request (DNS rebinding?)", r.Host))
			return false
		}
	}
	return true
}

type schemaColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	Key      string `json:"key,omitempty"`
}

type schemaTable struct {
	Name    string         `json:"name"`
	Rows    int64          `json:"approx_rows"`
	Columns []schemaColumn `json:"columns"`
}

type schemaDB struct {
	Name   string        `json:"name"`
	Tables []schemaTable `json:"tables"`
}

func (s *Server) handleSchema(w http.ResponseWriter, r *http.Request) {
	if !s.browserSafe(w, r) {
		return
	}
	name := r.PathValue("name")
	db, err := s.openDB(r.Context(), name)
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()

	rows, err := db.QueryContext(ctx, s.schemaQuery())
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer func() { _ = rows.Close() }()

	var dbs []schemaDB
	for rows.Next() {
		var schema, table, col, ctype, nullable, key string
		var approxRows int64
		if err := rows.Scan(&schema, &table, &approxRows, &col, &ctype, &nullable, &key); err != nil {
			s.writeError(w, err)
			return
		}
		if len(dbs) == 0 || dbs[len(dbs)-1].Name != schema {
			dbs = append(dbs, schemaDB{Name: schema})
		}
		d := &dbs[len(dbs)-1]
		if len(d.Tables) == 0 || d.Tables[len(d.Tables)-1].Name != table {
			d.Tables = append(d.Tables, schemaTable{Name: table, Rows: approxRows})
		}
		t := &d.Tables[len(d.Tables)-1]
		t.Columns = append(t.Columns, schemaColumn{
			Name: col, Type: ctype, Nullable: nullable == "YES", Key: key,
		})
	}
	if err := rows.Err(); err != nil {
		s.writeError(w, err)
		return
	}
	if dbs == nil {
		dbs = []schemaDB{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"databases": dbs})
}

type queryReq struct {
	SQL string `json:"sql"`
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	if !s.browserSafe(w, r) {
		return
	}
	// Content-Type を必須化する。これだけでブラウザの cross-site "simple request"
	// (text/plain 等で preflight なしに飛ぶ POST)は遮断される。
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "invalid_content_type",
			"Content-Type must be application/json")
		return
	}
	name := r.PathValue("name")
	var req queryReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SQL == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "body must be {\"sql\": \"...\"}")
		return
	}
	// 監査: app 資格情報で任意 SQL を流せる経路なので、誰が何を流したか残す(#294)。
	// 本文はログに書かない(INSERT/UPDATE のリテラルに秘密や個人情報が入る、
	// #310 review)。種別・長さ・digest で照合できる形にする。
	log.Printf("api: query branch=%s by=%s kind=%s bytes=%d sha256=%s",
		name, callerOf(r).Name, sqlKind(req.SQL), len(req.SQL), sqlDigest(req.SQL))
	db, err := s.openDB(r.Context(), name)
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()

	rows, err := db.QueryContext(ctx, req.SQL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "query_error", err.Error())
		return
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		s.writeError(w, err)
		return
	}
	var out [][]any
	truncated := false
	for rows.Next() {
		if len(out) >= maxRows {
			truncated = true
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			s.writeError(w, err)
			return
		}
		row := make([]any, len(cols))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				// 巨大セル(LONGBLOB/LONGTEXT)で応答とメモリが肥大しないよう切り詰める
				if len(b) > maxCellBytes {
					row[i] = string(b[:maxCellBytes]) + "…(truncated)"
				} else {
					row[i] = string(b)
				}
			} else {
				row[i] = v
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusBadRequest, "query_error", err.Error())
		return
	}
	if out == nil {
		out = [][]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"columns": cols, "rows": out, "truncated": truncated,
	})
}

// mysqlSchemaQuery は MySQL 用のスキーマ問い合わせ。
const mysqlSchemaQuery = `
	SELECT c.table_schema, c.table_name, COALESCE(t.table_rows, 0),
	       c.column_name, c.column_type, c.is_nullable, c.column_key
	FROM information_schema.columns c
	JOIN information_schema.tables t
	  ON t.table_schema = c.table_schema AND t.table_name = c.table_name
	WHERE c.table_schema NOT IN ('mysql','sys','information_schema','performance_schema')
	ORDER BY c.table_schema, c.table_name, c.ordinal_position`

// schemaQuery は engine に応じたスキーマ問い合わせを返す(#228)。
// 返す列は engine を問わず同じ(schema, table, 概算行数, column, type, nullable, key)。
func (s *Server) schemaQuery() string {
	if s.engine == "postgres" {
		return pgSchemaQuery
	}
	return mysqlSchemaQuery
}

// sqlKind は文の先頭キーワード(SELECT / INSERT / …)を返す。リテラルは含まない。
func sqlKind(sql string) string {
	f := strings.Fields(strings.TrimLeft(sql, " \t\r\n("))
	if len(f) == 0 {
		return "?"
	}
	k := strings.ToUpper(f[0])
	if len(k) > 16 {
		k = k[:16]
	}
	return k
}

// sqlDigest は本文の sha256 先頭 12 桁。同じ文の再発を突き合わせる用。
func sqlDigest(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(sum[:])[:12]
}
