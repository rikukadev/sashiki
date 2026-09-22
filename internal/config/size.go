package config

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseSize はサイズ文字列をバイト数にする(#300)。
//
// 受け付ける単位(大文字小文字は問わない、K = KB = KiB = 1024 のように 2 進で扱う):
// 無し / B / K KB KiB / M MB MiB / G GB GiB / T TB TiB。空文字は 0(未設定)。
// 以前は末尾 K/M/G だけを見ていて、"256MB" や SPEC に書いてあった "20GiB" は
// エラーにならず 0(= 無制限 / admission 無効)になっていた。
func ParseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, nil
	}
	i := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("size %q: 数値で始まっていません(例 256M / 20GiB)", s)
	}
	n, err := strconv.ParseInt(t[:i], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	var mult int64
	switch strings.ToLower(strings.TrimSpace(t[i:])) {
	case "", "b":
		mult = 1
	case "k", "kb", "kib":
		mult = 1 << 10
	case "m", "mb", "mib":
		mult = 1 << 20
	case "g", "gb", "gib":
		mult = 1 << 30
	case "t", "tb", "tib":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("size %q: 単位 %q は使えません(B / K / M / G / T、KB・KiB 形も可)", s, t[i:])
	}
	if n > (1<<62)/mult {
		return 0, fmt.Errorf("size %q: 大きすぎます", s)
	}
	return n * mult, nil
}
