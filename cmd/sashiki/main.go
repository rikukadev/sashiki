// sashiki: CLI。sashikid の REST API を叩く(仕様 14-5 の v0.1 サブセット)。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"
)

const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitNotFound = 3
	exitExists   = 4
	exitCapacity = 5 // 507: capacity 不足(仕様 18章)
	exitTimeout  = 6 // --wait / op wait のタイムアウト(容量不足と区別する、#307)
)

var version = "dev" // -ldflags で埋め込む

func main() {
	os.Exit(run(os.Args[1:]))
}

func usage() int {
	fmt.Fprint(os.Stderr, `Usage:
  sashiki create <name> [--exist-ok] [--port N] [--owner O] [--purpose P] [--profile P] [--ttl D] [--baseline B] [--source JSON] [--json]
  sashiki delete <name> [--json]
  sashiki reset  <name> [--json]
  sashiki recreate <name> [--json]
  sashiki retry <name> [--json]
  sashiki sleep  <name> [--json]
  sashiki wake   <name> [--json]
  sashiki lease renew <name> --for <dur>   (例 7d, 1h)
  sashiki hooks run <name> <event>
  sashiki list   [--json]
  sashiki show   <name> [--json]
  sashiki connect <name>
  sashiki env    <name> [--prefix P]        接続情報を KEY=VALUE で出す
  sashiki init   --pool <p> [--device <dev>] [--engine mysql|postgres] [--app-pass <pw>]
                 [--platform darwin] [--root <dir>] [--skip-packages] [--yes]
  sashiki baseline import|list|refresh|promote|set|delete|build|validate|publish|gc   (詳細は sashiki baseline)
  sashiki token create|list|revoke
  sashiki op list | show <id> | wait <id>
  sashiki capacity [--json]
  sashiki doctor [--json]
  sashiki gc --orphans
  sashiki drain
  sashiki version

非同期な変更(create/delete/reset/recreate/retry、baseline build|validate)は既定で完了まで待つ。
  baseline refresh は開始だけ返す(進捗は sashiki baseline list)。baseline promote は同期。
  --no-wait で待たずに operation を返す / --timeout <dur> / --interval <dur> で待機を調整。

終了コード: 0=成功 1=エラー 2=使い方 3=不在(404) 4=競合(409) 5=容量不足(507) 6=待機タイムアウト。
コマンド・API・config の一覧は docs/REFERENCE.md。
`)
	return exitUsage
}

func run(args []string) int {
	if len(args) < 1 {
		return usage()
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "create":
		return cmdCreate(rest)
	case "delete":
		return cmdDelete(rest)
	case "reset":
		return cmdSimpleBranch(rest, "reset")
	case "recreate":
		return cmdSimpleBranch(rest, "recreate")
	case "retry":
		return cmdSimpleBranch(rest, "retry")
	case "sleep":
		return cmdSyncBranch(rest, "sleep")
	case "wake":
		return cmdSyncBranch(rest, "wake")
	case "lease":
		return cmdLease(rest)
	case "hooks":
		return cmdHooks(rest)
	case "list":
		return cmdList(rest)
	case "show":
		return cmdShow(rest)
	case "connect":
		return cmdConnect(rest)
	case "env":
		return cmdEnv(rest)
	case "init":
		return cmdInit(rest)
	case "baseline":
		return cmdBaseline(rest)
	case "token":
		return cmdToken(rest)
	case "op":
		return cmdOp(rest)
	case "capacity":
		return cmdCapacity(rest)
	case "doctor":
		return cmdDoctor(rest)
	case "gc":
		return cmdGC(rest)
	case "drain":
		return cmdDrain(rest)
	case "version":
		fmt.Println("sashiki", version)
		return exitOK
	default:
		return usage()
	}
}

// --- API client ---

func apiURL() string {
	if v := os.Getenv("SASHIKI_API_URL"); v != "" {
		return v
	}
	return "http://127.0.0.1:8080"
}

func apiToken() string {
	if v := os.Getenv("SASHIKI_API_TOKEN"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(home + "/.config/sashiki/token")
	if err != nil {
		return ""
	}
	return string(bytes.TrimSpace(b))
}

func call(method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, apiURL()+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if t := apiToken(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	client := &http.Client{Timeout: httpTimeout()} // create/reset はストレージ次第で長い
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

// httpTimeout は 1 リクエストの上限。--timeout がそれより長ければ広げる(#308)。
var httpTimeoutValue atomic.Int64

func httpTimeout() time.Duration {
	if v := httpTimeoutValue.Load(); v > 0 {
		return time.Duration(v)
	}
	return defaultWaitTimeout
}

func setHTTPTimeout(d time.Duration) {
	if d > defaultWaitTimeout {
		httpTimeoutValue.Store(int64(d))
	}
}

// callErr は call の失敗を人が読める 1 行にする。err が nil のときだけ API の
// エラーボディを使う(#308: 通信失敗で `sashiki: ` と空行だけ出していた)。
func callErr(err error, data []byte) string {
	if err != nil {
		return err.Error()
	}
	return apiError(data)
}

func apiError(data []byte) string {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return string(data)
}

func statusToExit(code int) int {
	switch code {
	case http.StatusNotFound:
		return exitNotFound
	case http.StatusConflict:
		return exitExists
	case http.StatusInsufficientStorage:
		return exitCapacity
	default:
		return exitError
	}
}

type branchView struct {
	Name  string `json:"name"`
	State string `json:"state"`
	// Port / Host / User は接続にそのまま使える 3 つ組(#260)。proxy が有効なら
	// proxy 宛、無効ならブランチ直結。EnginePort はブランチ自身の listener で、
	// 接続用ではなく調査用。
	Port         int      `json:"port"`
	EnginePort   int      `json:"engine_port"`
	Host         string   `json:"host"`
	User         string   `json:"user"`
	Profile      string   `json:"profile"`
	CreatedAt    string   `json:"created_at"`
	LastConnAt   *string  `json:"last_conn_at"`
	ExpiresAt    *string  `json:"expires_at"`
	UsedBytes    int64    `json:"used_bytes"`
	LogicalBytes int64    `json:"logical_bytes"`
	Engine       string   `json:"engine"`
	Error        string   `json:"error"`
	FailedOp     string   `json:"failed_operation"`
	ErrorCode    string   `json:"error_code"`
	Recoverable  bool     `json:"recoverable"`
	Suggestions  []string `json:"suggested_actions"`
	Stale        bool     `json:"stale"`
	// promote 元として baseline 実体を保持している。reset / recreate / delete 不可(#289)。
	BackingBaselines []string `json:"backing_baselines"`
}

// --- commands ---

func parseFlags(args []string) (pos []string, port int, jsonOut bool, err error) {
	pos, port, jsonOut, _, err = parseFlagsKV(args)
	return
}

// parseFlagsKV は --json / --port に加え、--owner/--purpose/--source/--profile を拾う。
func parseFlagsKV(args []string) (pos []string, port int, jsonOut bool, kv map[string]string, err error) {
	kv = map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--json":
			jsonOut = true
		case "--port":
			if i+1 >= len(args) {
				return nil, 0, false, nil, fmt.Errorf("--port requires a value")
			}
			i++
			port, err = strconv.Atoi(args[i])
			if err != nil {
				return nil, 0, false, nil, fmt.Errorf("--port: %w", err)
			}
		case "--exist-ok":
			kv["exist-ok"] = "true"
		case "--owner", "--purpose", "--source", "--profile", "--ttl", "--baseline":
			if i+1 >= len(args) {
				return nil, 0, false, nil, fmt.Errorf("%s requires a value", a)
			}
			i++
			kv[a[2:]] = args[i]
		default:
			// 未知のフラグを黙って位置引数として飲まない(#308)。
			if strings.HasPrefix(a, "-") && a != "-" {
				return nil, 0, false, nil, fmt.Errorf("unknown flag %s", a)
			}
			pos = append(pos, a)
		}
	}
	return pos, port, jsonOut, kv, nil
}

func cmdCreate(args []string) int {
	args, noWait, timeout, interval, werr := extractWaitFlags(args)
	if werr != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", werr)
		return exitUsage
	}
	pos, port, jsonOut, kv, err := parseFlagsKV(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitUsage
	}
	if len(pos) != 1 {
		return usage()
	}
	body := map[string]any{"name": pos[0], "port": port}
	for _, k := range []string{"owner", "purpose", "profile", "ttl", "baseline"} {
		if v := kv[k]; v != "" {
			body[k] = v
		}
	}
	if src := kv["source"]; src != "" {
		body["source"] = json.RawMessage(src)
	}
	// --exist-ok: 既にあれば 200 + 既存を返す(API は前から対応していて、
	// CLI から渡す口が無かった)。up のたびに走る hook から使うためのもので、
	// 無いと 2 回目が 409 で落ち、`|| true` で **実エラーまで握り潰す** 回避に
	// 追い込まれる(kagerou#9 / この issue)。
	path := "/v1/branches"
	if kv["exist-ok"] == "true" {
		path += "?exist_ok=true"
	}
	code, data, err := call("POST", path, body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	switch code {
	case http.StatusOK: // exist_ok で既存 → branch がそのまま返る
		return printCreatedBranch(data, jsonOut)
	case http.StatusAccepted: // 非同期。既定で完了を待ってから branch を取得して表示
		done, exit := awaitMutation(data, noWait, timeout, interval)
		if !done {
			return exit
		}
		bcode, bdata, berr := call("GET", "/v1/branches/"+pos[0], nil)
		if berr != nil || bcode != http.StatusOK {
			// 作成自体は成功している。接続情報だけ取れなかったことを黙らない(#308)。
			fmt.Fprintf(os.Stderr, "sashiki: 警告 作成後の取得に失敗しました(接続情報は sashiki show %s): %s\n",
				pos[0], callErr(berr, bdata))
			if jsonOut {
				return exitError
			}
			fmt.Printf("branch '%s' ready\n", pos[0])
			return exitOK
		}
		return printCreatedBranch(bdata, jsonOut)
	default:
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
}

func printCreatedBranch(data []byte, jsonOut bool) int {
	if jsonOut {
		fmt.Println(string(data))
		return exitOK
	}
	var b branchView
	_ = json.Unmarshal(data, &b)
	fmt.Printf("branch '%s' ready: %s\n", b.Name, connectHint(b))
	return exitOK
}

func cmdDelete(args []string) int {
	args, noWait, timeout, interval, werr := extractWaitFlags(args)
	if werr != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", werr)
		return exitUsage
	}
	if len(args) != 1 {
		return usage()
	}
	name := args[0]
	code, data, err := call("DELETE", "/v1/branches/"+name, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusAccepted {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	done, exit := awaitMutation(data, noWait, timeout, interval)
	if !done {
		return exit
	}
	fmt.Printf("branch '%s' deleted\n", name)
	return exitOK
}

// cmdLease は `sashiki lease renew <name> --for 7d`。expires_at を now+for に設定する。
func cmdLease(args []string) int {
	if len(args) < 1 || args[0] != "renew" {
		fmt.Fprintln(os.Stderr, "usage: sashiki lease renew <name> --for <dur>  (例 7d, 1h)")
		return usage()
	}
	rest := args[1:]
	var name, dur string
	var jsonOut bool
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--for":
			if i+1 >= len(rest) {
				fmt.Fprintln(os.Stderr, "sashiki: --for requires a value")
				return exitError
			}
			i++
			dur = rest[i]
		case "--json":
			jsonOut = true
		default:
			name = rest[i]
		}
	}
	if name == "" || dur == "" {
		fmt.Fprintln(os.Stderr, "usage: sashiki lease renew <name> --for <dur>  (例 7d, 1h)")
		return usage()
	}
	code, data, err := call("POST", "/v1/branches/"+name+"/lease", map[string]any{"for": dur})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	if jsonOut {
		fmt.Println(string(data))
		return exitOK
	}
	var b branchView
	_ = json.Unmarshal(data, &b)
	exp := "(none)"
	if b.ExpiresAt != nil {
		exp = *b.ExpiresAt
	}
	fmt.Printf("branch '%s' lease renewed: expires_at=%s\n", name, exp)
	return exitOK
}

// cmdSyncBranch は同期の変更操作(sleep / wake)を実行し branch を表示する(#87)。
func cmdSyncBranch(args []string, action string) int {
	pos, _, jsonOut, err := parseFlags(args)
	if err != nil || len(pos) != 1 {
		return usage()
	}
	code, data, err := call("POST", "/v1/branches/"+pos[0]+"/"+action, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	if jsonOut {
		fmt.Println(string(data))
	} else {
		fmt.Printf("branch '%s' %s\n", pos[0], action)
	}
	return exitOK
}

func cmdSimpleBranch(args []string, action string) int {
	args, noWait, timeout, interval, werr := extractWaitFlags(args)
	if werr != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", werr)
		return exitUsage
	}
	pos, _, jsonOut, err := parseFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitUsage
	}
	if len(pos) != 1 {
		return usage()
	}
	// reset は origin(作成時の baseline)に戻す。baseline が更新済み(stale)なら
	// 最新化には recreate が要ることを警告する(#130)。
	if action == "reset" {
		if _, bdata, e := call("GET", "/v1/branches/"+pos[0], nil); e == nil {
			var bv branchView
			if json.Unmarshal(bdata, &bv) == nil && bv.Stale {
				fmt.Fprintf(os.Stderr, "警告: '%s' は origin が current baseline より古いです。reset は作成時点(古い baseline)に戻します。最新化には recreate を使ってください。\n", pos[0])
			}
		}
	}
	code, data, err := call("POST", "/v1/branches/"+pos[0]+"/"+action, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusAccepted {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	done, exit := awaitMutation(data, noWait, timeout, interval)
	if !done {
		return exit
	}
	if jsonOut {
		bcode, bdata, e := call("GET", "/v1/branches/"+pos[0], nil)
		if e != nil || bcode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "sashiki: %s は完了しましたが取得に失敗しました: %s\n", action, callErr(e, bdata))
			return exitError
		}
		fmt.Println(string(bdata))
	} else {
		fmt.Printf("branch '%s' %s\n", pos[0], action)
	}
	return exitOK
}

func cmdList(args []string) int {
	pos, _, jsonOut, err := parseFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitUsage
	}
	if len(pos) > 0 {
		fmt.Fprintf(os.Stderr, "sashiki list: 余分な引数 %v(ブランチ 1 件は sashiki show <name>)\n", pos)
		return exitUsage
	}
	code, data, err := call("GET", "/v1/branches", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	if jsonOut {
		fmt.Println(string(data))
		return exitOK
	}
	var resp struct {
		Branches []branchView `json:"branches"`
	}
	_ = json.Unmarshal(data, &resp)
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tPORT\tSTATE\tLAST_CONN\tUSED")
	var stale, backing []string
	for _, b := range resp.Branches {
		last := "-"
		if b.LastConnAt != nil {
			last = *b.LastConnAt
		}
		name := b.Name
		if b.Stale {
			name += "*"
			stale = append(stale, b.Name)
		}
		if len(b.BackingBaselines) > 0 {
			name += "!"
			backing = append(backing, b.Name)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", name, b.Port, b.State, last, humanBytes(b.UsedBytes))
	}
	_ = tw.Flush()
	if len(stale) > 0 {
		fmt.Printf("\n* origin が current baseline より古い(recreate で最新化): %s\n", strings.Join(stale, ", "))
	}
	if len(backing) > 0 {
		fmt.Printf("\n! baseline の実体を保持(promote 元。reset / recreate / delete 不可): %s\n", strings.Join(backing, ", "))
	}
	return exitOK
}

func cmdShow(args []string) int {
	pos, _, jsonOut, err := parseFlags(args)
	if err != nil || len(pos) != 1 {
		return usage()
	}
	code, data, err := call("GET", "/v1/branches/"+pos[0], nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	if jsonOut {
		fmt.Println(string(data))
		return exitOK
	}
	var b branchView
	_ = json.Unmarshal(data, &b)
	fmt.Printf("name:    %s\nstate:   %s\nport:    %d\nuser:    %s\nprivate: %s (CoW差分)\nlogical: %s\n",
		b.Name, b.State, b.Port, b.User, humanBytes(b.UsedBytes), humanBytes(b.LogicalBytes))
	// proxy 経由のときは内部ポートも出す。ログや ss の出力と突き合わせるのに要る。
	if b.EnginePort != 0 && b.EnginePort != b.Port {
		fmt.Printf("engine:  :%d (ブランチ自身の listener。接続には使わない)\n", b.EnginePort)
	}
	if b.Stale {
		fmt.Println("stale:   true (origin が current baseline より古い。reset は作成時点に戻る/最新化は recreate)")
	}
	if len(b.BackingBaselines) > 0 {
		fmt.Printf("baseline: %s を保持(promote 元。reset / recreate / delete は別 baseline を promote/set するまで拒否)\n",
			strings.Join(b.BackingBaselines, ", "))
	}
	if b.Error != "" {
		fmt.Printf("error: %s\n", b.Error)
		if b.ErrorCode != "" {
			fmt.Printf("code:  %s (recoverable=%v)\n", b.ErrorCode, b.Recoverable)
		}
		for _, sug := range b.Suggestions {
			fmt.Printf("  → %s\n", sug)
		}
	}
	return exitOK
}

// cmdEnv は接続情報を dotenv(KEY=VALUE)で出す。
//
// show --json + jq の配管を各所に書かせないためのもの。環境変数として
// 渡す先(CI・アプリ)は、JSON ではなく KEY=VALUE を求める。
//
// **パスワードは出さない。** 払い出されるのは接続先とユーザー名までで、
// app_user のパスワードは baseline 由来のブランチ共通値。sashiki が
// 知っている値ではあるが、ここで出すと「接続情報を表示しただけ」の
// つもりの操作がログや CI の出力に秘密を残す。
func cmdEnv(args []string) int {
	prefix := "DB_"
	var pos []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--prefix" {
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "sashiki: --prefix requires a value")
				return exitError
			}
			i++
			prefix = args[i]
			continue
		}
		pos = append(pos, args[i])
	}
	if len(pos) != 1 {
		return usage()
	}
	code, data, err := call("GET", "/v1/branches/"+pos[0], nil)
	if err != nil || code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "sashiki:", callErr(err, data))
		return statusToExit(code)
	}
	var b branchView
	if err := json.Unmarshal(data, &b); err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	// host/port/user は「そのまま繋がる 3 つ組」(#260)。解釈せずに出す。
	fmt.Printf("%sHOST=%s\n", prefix, b.Host)
	fmt.Printf("%sPORT=%d\n", prefix, b.Port)
	fmt.Printf("%sUSER=%s\n", prefix, b.User)
	return exitOK
}

func cmdConnect(args []string) int {
	if len(args) != 1 {
		return usage()
	}
	code, data, err := call("GET", "/v1/branches/"+args[0], nil)
	if err != nil || code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "sashiki:", callErr(err, data))
		return statusToExit(code)
	}
	var b branchView
	_ = json.Unmarshal(data, &b)
	// engine に合わせたクライアントを exec する(#308: postgres でも mysql を
	// 起動し、host は 127.0.0.1、パスワードは dev 固定だった)。
	// パスワードは環境変数で渡す。無ければクライアントに尋ねさせる。
	client, argv := "mysql", []string{"mysql", "-u" + b.User, "-h" + connectHost(b), "-P" + strconv.Itoa(b.Port)}
	env := os.Environ()
	pass := os.Getenv("SASHIKI_DB_PASSWORD")
	if b.Engine == "postgres" {
		client = "psql"
		argv = []string{"psql", "-U", b.User, "-h", connectHost(b), "-p", strconv.Itoa(b.Port)}
		if pass != "" {
			env = append(env, "PGPASSWORD="+pass)
		}
	} else {
		if pass != "" {
			env = append(env, "MYSQL_PWD="+pass)
		} else {
			argv = append(argv, "-p") // 対話で尋ねる
		}
	}
	path, err := exec.LookPath(client)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sashiki: %s が PATH にありません\n", client)
		return exitError
	}
	// user は必ず API が返した値を使う。proxy 経由では dev@<branch> でないと
	// ルーティングできず、直結では dev でないと認証が通らない(#260)。
	// CLI はそのままクライアントに化ける。
	if err := syscall.Exec(path, argv, env); err != nil {
		fmt.Fprintf(os.Stderr, "sashiki: exec %s: %v\n", client, err)
		return exitError
	}
	return exitOK
}

// connectHost は接続先ホスト。API の host が名前解決できない構成もあるので、
// SASHIKI_DB_HOST で上書きできる。
func connectHost(b branchView) string {
	if h := os.Getenv("SASHIKI_DB_HOST"); h != "" {
		return h
	}
	if b.Host == "" {
		return "127.0.0.1"
	}
	return b.Host
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < 0 {
		return "-" // 不明(取得できない値。CoW 差分が取れない apfs 等)
	}
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGTPE"[exp])
}

// connectHint は engine に応じた接続コマンド例を返す(#238)。
// postgres に mysql のコマンドを案内していると、そのまま貼って失敗する。
func connectHint(b branchView) string {
	if b.Engine == "postgres" {
		return fmt.Sprintf("psql -h %s -p %d -U '%s' -d <db>", b.Host, b.Port, b.User)
	}
	return fmt.Sprintf("mysql -u%s -h %s -P%d", b.User, b.Host, b.Port)
}
