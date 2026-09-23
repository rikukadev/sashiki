package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// ProcessIs は pid のプロセス名(実行ファイルの basename)が names のどれかと一致するかを返す。
// 判定できなかった(ps が無い等)ときは known=false で、呼び出し側は従来どおり
// 「生きている」とみなす。
//
// process モードは pidfile の pid に kill(pid, 0) を送るだけで生存を判定していたので、
// Mac を再起動した後に pidfile が残り、その pid を別プロセスが再利用していると
// running と誤認していた(#302)。
func ProcessIs(pid int, names ...string) (match, known bool) {
	comm := processName(pid)
	if comm == "" {
		return false, false
	}
	base := filepath.Base(strings.TrimSpace(comm))
	for _, n := range names {
		if base == n {
			return true, true
		}
		// Linux の /proc/<pid>/comm は 15 文字(TASK_COMM_LEN-1)で切れる。ちょうど
		// 15 文字なら切り詰められた可能性があるので prefix で判定する(#327: 16 文字
		// 以上のラッパー名だと「動いていない」と誤判定して Stop を飛ばしていた)。
		if runtime.GOOS == "linux" && len(base) == commMaxLen && strings.HasPrefix(n, base) {
			return true, true
		}
	}
	return false, true
}

// commMaxLen は Linux の comm に入る最大文字数(TASK_COMM_LEN 16 - NUL)。
const commMaxLen = 15

func processName(pid int) string {
	if runtime.GOOS == "linux" {
		if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm"); err == nil {
			return string(b)
		}
		return ""
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return ""
	}
	return string(out)
}
