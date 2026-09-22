//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// fsTypeOf は path を含むファイルシステムの種類(apfs / hfs / exfat / smbfs …)を返す。
func fsTypeOf(path string) (string, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return "", err
	}
	b := make([]byte, 0, len(st.Fstypename))
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b), nil
}

// prepareDarwinRoot は root を作り、APFS 上かを確かめ、Time Machine の対象から外す(#302)。
//
//   - clonefile は APFS でしか効かない。exFAT / HFS+ / ネットワーク volume に root を
//     置くと init が base を作り終えた後の snapshot で失敗していたので、先に止める
//   - Time Machine はバックアップ先で clone の共有を保てず、base + snapshot +
//     ブランチ数ぶんをフルサイズで複製する。tmutil addexclusion は失敗しても続ける
func prepareDarwinRoot(root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	fs, err := fsTypeOf(root)
	if err != nil {
		return fmt.Errorf("statfs %s: %w", root, err)
	}
	if fs != "apfs" {
		return fmt.Errorf("%s は %s 上にあります。sashiki の macOS ネイティブ構成は APFS(clonefile)が必要です。"+
			"--root で APFS の場所を指定してください", root, fs)
	}
	abs, _ := filepath.Abs(root)
	if out, err := exec.Command("tmutil", "addexclusion", abs).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "sashiki init: 警告 Time Machine の除外に失敗しました(%v: %s)。"+
			"バックアップ先ではブランチがフルサイズで複製されます。手動で `tmutil addexclusion %q` を実行してください\n",
			err, strings.TrimSpace(string(out)), abs)
	} else {
		fmt.Printf("  ✓ Time Machine の対象から除外: %s\n", abs)
	}
	return nil
}
