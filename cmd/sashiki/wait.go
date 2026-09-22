// 変更操作(create/reset/recreate/retry/delete)の 202 + operation を待つ
// 共通処理(#82)。CLI は既定で operation の完了を待ち、体感を同期に保つ。
// --no-wait で operation_id だけ表示して即戻る。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

// 待機の既定値。op wait(cmd/sashiki/op.go)と揃える(#308: 以前は 15m/300ms と
// 10m/200ms で食い違っていた)。
const (
	defaultWaitTimeout  = 15 * time.Minute
	defaultWaitInterval = 300 * time.Millisecond
)

// extractWaitFlags は --no-wait / --timeout / --interval を取り出し、残りの引数を返す。
// 値が壊れていれば黙って既定に戻さずエラーにする(#308: `--timeout 30` のような
// 指定が無視され、待っているつもりが既定で切れていた)。
func extractWaitFlags(args []string) (rest []string, noWait bool, timeout, interval time.Duration, err error) {
	timeout = defaultWaitTimeout
	interval = defaultWaitInterval
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--no-wait":
			noWait = true
		case "--wait": // 既定。明示指定を許容(no-op)
		case "--timeout", "--interval":
			flag := args[i]
			if i+1 >= len(args) {
				return nil, false, 0, 0, fmt.Errorf("%s requires a value (例 %s 30m)", flag, flag)
			}
			i++
			d, perr := time.ParseDuration(args[i])
			if perr != nil {
				return nil, false, 0, 0, fmt.Errorf("%s %q: %w(例 30m / 500ms)", flag, args[i], perr)
			}
			if d <= 0 {
				return nil, false, 0, 0, fmt.Errorf("%s %q: 正の値にする", flag, args[i])
			}
			if flag == "--timeout" {
				timeout = d
			} else {
				interval = d
			}
		default:
			rest = append(rest, args[i])
		}
	}
	// HTTP クライアント側のタイムアウトも合わせる(#308: 15 分固定だったので
	// --timeout 30m にしてもポーリングの各リクエストが先に切れていた)。
	setHTTPTimeout(timeout)
	return rest, noWait, timeout, interval, nil
}

// operationIDFrom は 202 応答ボディ({"operation_id":...})から id を取る。
func operationIDFrom(data []byte) string {
	var r struct {
		OperationID string `json:"operation_id"`
	}
	_ = json.Unmarshal(data, &r)
	return r.OperationID
}

// errWaitTimeout は「期限までに operation が終わらなかった」。API エラーや通信断と
// 区別して、終了コード 6 はこれだけに使う(#308 review)。
var errWaitTimeout = errors.New("wait timed out")

// statusError は API が非 200 を返したときのエラー。終了コードを status から決める。
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

// pollOperationFn はテストで差し替えるための間接参照。
var pollOperationFn = pollOperation

// pollOperation は operation が完了するまでポーリングし、最終 state と error を返す。
func pollOperation(id string, timeout, interval time.Duration) (state, errText string, err error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		code, data, cerr := call("GET", "/v1/operations/"+id, nil)
		if cerr != nil {
			return "", "", cerr
		}
		if code != http.StatusOK {
			return "", "", &statusError{code: code, msg: apiError(data)}
		}
		var o opView
		_ = json.Unmarshal(data, &o)
		if o.State != "running" && o.State != "" {
			return o.State, o.Error, nil
		}
		time.Sleep(interval)
	}
	return "running", "", fmt.Errorf("%w after %s (operation %s is still running)", errWaitTimeout, timeout, id)
}

// awaitMutation は 202 応答の operation を(既定で)待つ。
// done=true は operation が completed。exit は CLI の終了コード。
func awaitMutation(data []byte, noWait bool, timeout, interval time.Duration) (done bool, exit int) {
	opID := operationIDFrom(data)
	if opID == "" {
		fmt.Fprintln(os.Stderr, "sashiki: 応答に operation_id がありません")
		return false, exitError
	}
	if noWait {
		fmt.Println(opID)
		return false, exitOK
	}
	st, errMsg, err := pollOperationFn(opID, timeout, interval)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		// 6 は「期限までに終わらなかった」だけに使う。通信断や 401 / 404 / 500 まで
		// 6 にすると、CI が「詰まっている」と「容量不足」を区別する意味が無くなる。
		var se *statusError
		switch {
		case errors.Is(err, errWaitTimeout):
			return false, exitTimeout
		case errors.As(err, &se):
			return false, statusToExit(se.code)
		default:
			return false, exitError
		}
	}
	if st != "completed" {
		if errMsg != "" {
			fmt.Fprintln(os.Stderr, "sashiki:", errMsg)
		}
		return false, exitError
	}
	return true, exitOK
}
