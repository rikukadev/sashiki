package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// 16 文字以上の実行ファイル名でも一致する(#327: Linux の comm は 15 文字で切れる)。
func TestProcessIsLongName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip(err)
	}
	const long = "mysqld-wrapper-with-a-long-name"
	link := filepath.Join(t.TempDir(), long)
	if err := os.Symlink(sleep, link); err != nil {
		t.Skip(err)
	}
	cmd := exec.Command(link, "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if match, known := ProcessIs(cmd.Process.Pid, long); !known || !match {
		t.Errorf("long name: match=%v known=%v", match, known)
	}
	if match, known := ProcessIs(cmd.Process.Pid, "mysqld-wrapper-with-another-name"); known && match {
		t.Error("a different long name must not match")
	}
}
