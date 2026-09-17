// メモリガード用のヘルパー。
package main

import (
	"os"
	"regexp"
	"strconv"

	"github.com/rikukadev/sashiki/internal/config"
)

// availableMem は /proc/meminfo の MemAvailable(kB)を返す。
// Linux 以外では判定不能としてエラーを返す(ガードは best-effort)。
func availableMem() (int64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	m := regexp.MustCompile(`MemAvailable:\s+(\d+) kB`).FindSubmatch(data)
	if m == nil {
		return 0, os.ErrNotExist
	}
	kb, err := strconv.ParseInt(string(m[1]), 10, 64)
	if err != nil {
		return 0, err
	}
	return kb * 1024, nil
}

// parseSize はサイズ文字列をバイト数にする。書式の検証は config.Load が済ませて
// いる(#300)ので、ここでは解釈できなければ 0 を返すだけ。
func parseSize(s string) int64 {
	n, err := config.ParseSize(s)
	if err != nil {
		return 0
	}
	return n
}
