package main

import (
	"net/http"
	"testing"
)

func TestStatusToExit(t *testing.T) {
	cases := map[int]int{
		http.StatusNotFound:            exitNotFound,
		http.StatusConflict:            exitExists,
		http.StatusInsufficientStorage: exitCapacity, // 仕様 18章: capacity 不足は 5
		http.StatusInternalServerError: exitError,
	}
	for in, want := range cases {
		if got := statusToExit(in); got != want {
			t.Errorf("statusToExit(%d) = %d, want %d", in, got, want)
		}
	}
}

// 容量不足(507)と待機タイムアウトは別の終了コードにする(#307)。
// CI が「詰まっている」のか「いっぱい」なのかを区別できるようにするため。
func TestTimeoutAndCapacityExitCodesDiffer(t *testing.T) {
	if exitTimeout == exitCapacity {
		t.Fatalf("exitTimeout(%d) と exitCapacity(%d) は別の値にする", exitTimeout, exitCapacity)
	}
	if got := statusToExit(507); got != exitCapacity {
		t.Errorf("507 -> %d, want %d", got, exitCapacity)
	}
}
