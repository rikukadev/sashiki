//go:build darwin

package main

import "testing"

// 一時ディレクトリ(macOS では APFS)のファイルシステム種別を読める。
func TestFsTypeOfTempDir(t *testing.T) {
	fs, err := fsTypeOf(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if fs == "" {
		t.Error("fs type should not be empty")
	}
}
