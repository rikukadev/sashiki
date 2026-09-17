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
	}
	return false, true
}

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
