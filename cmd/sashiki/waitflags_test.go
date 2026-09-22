package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// --timeout / --interval の値が壊れていれば黙って既定に戻さずエラーにする(#308)。
func TestExtractWaitFlagsRejectsBadValues(t *testing.T) {
	for _, args := range [][]string{
		{"pr-1", "--timeout"},
		{"pr-1", "--timeout", "30"},
		{"pr-1", "--interval", "abc"},
		{"pr-1", "--timeout", "0s"},
	} {
		if _, _, _, _, err := extractWaitFlags(args); err == nil {
			t.Errorf("%v should be rejected", args)
		}
	}
	rest, noWait, timeout, interval, err := extractWaitFlags([]string{"pr-1", "--no-wait", "--timeout", "30m", "--interval", "1s"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || rest[0] != "pr-1" || !noWait || timeout != 30*time.Minute || interval != time.Second {
		t.Errorf("rest=%v noWait=%v timeout=%s interval=%s", rest, noWait, timeout, interval)
	}
	// HTTP クライアント側の上限も --timeout に追従する(既定 15 分のままだと
	// ポーリングの各リクエストが先に切れる)。
	if httpTimeout() < 30*time.Minute {
		t.Errorf("httpTimeout = %s, want >= 30m", httpTimeout())
	}
}

// 未知のフラグを位置引数として飲み込まない(#308)。
func TestParseFlagsRejectsUnknownFlag(t *testing.T) {
	if _, _, _, _, err := parseFlagsKV([]string{"pr-1", "--owner", "me", "--typo"}); err == nil ||
		!strings.Contains(err.Error(), "--typo") {
		t.Errorf("unknown flag should be reported, got %v", err)
	}
	if _, _, _, _, err := parseFlagsKV([]string{"pr-1", "--owner", "me", "--json"}); err != nil {
		t.Errorf("known flags should pass: %v", err)
	}
}

// 通信失敗のときは API のエラーボディではなく err を出す(空行にしない)。
func TestCallErr(t *testing.T) {
	if got := callErr(nil, []byte(`{"error":{"message":"branch not found"}}`)); got != "branch not found" {
		t.Errorf("callErr(api) = %q", got)
	}
	if got := callErr(errTest, nil); got != "boom" {
		t.Errorf("callErr(network) = %q, want boom", got)
	}
}

var errTest = testErr("boom")

type testErr string

func (e testErr) Error() string { return string(e) }

// 終了コード 6 は「期限までに終わらなかった」だけに使う。API エラーや通信断は
// それぞれの終了コードにする(#308 review)。
func TestAwaitMutationExitCodes(t *testing.T) {
	accepted := []byte(`{"operation_id":"op_1"}`)
	restore := pollOperationFn
	defer func() { pollOperationFn = restore }()

	pollOperationFn = func(string, time.Duration, time.Duration) (string, string, error) {
		return "running", "", fmt.Errorf("%w after 1s (operation op_1 is still running)", errWaitTimeout)
	}
	if _, exit := awaitMutation(accepted, false, time.Second, time.Millisecond); exit != exitTimeout {
		t.Errorf("timeout -> %d, want %d", exit, exitTimeout)
	}
	pollOperationFn = func(string, time.Duration, time.Duration) (string, string, error) {
		return "", "", &statusError{code: 404, msg: "branch not found"}
	}
	if _, exit := awaitMutation(accepted, false, time.Second, time.Millisecond); exit != exitNotFound {
		t.Errorf("404 -> %d, want %d", exit, exitNotFound)
	}
	pollOperationFn = func(string, time.Duration, time.Duration) (string, string, error) {
		return "", "", errTest // 通信断
	}
	if _, exit := awaitMutation(accepted, false, time.Second, time.Millisecond); exit != exitError {
		t.Errorf("network error -> %d, want %d", exit, exitError)
	}
	pollOperationFn = func(string, time.Duration, time.Duration) (string, string, error) {
		return "completed", "", nil
	}
	if done, exit := awaitMutation(accepted, false, time.Second, time.Millisecond); !done || exit != exitOK {
		t.Errorf("completed -> done=%v exit=%d", done, exit)
	}
}
