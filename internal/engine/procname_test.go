package engine

import (
	"os"
	"os/exec"
	"testing"
)

func TestProcessIs(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	match, known := ProcessIs(cmd.Process.Pid, "sleep")
	if !known || !match {
		t.Errorf("sleep process: match=%v known=%v", match, known)
	}
	if match, known := ProcessIs(cmd.Process.Pid, "mysqld"); known && match {
		t.Error("a sleep process must not look like mysqld (pid reuse)")
	}
	if _, known := ProcessIs(os.Getpid()+1000000, "mysqld"); known {
		t.Error("a non-existent pid should be unknown")
	}
}
