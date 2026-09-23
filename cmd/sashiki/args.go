package main

import (
	"fmt"
	"os"
	"slices"
	"strings"
)

// parseArgs は CLI の引数を「そのコマンドが許可したフラグだけ」で解析する(#325)。
// values は値を取るフラグ、bools は値を取らないフラグ。それ以外の `-` 始まりは
// 未知のフラグ、値の無い values はエラーにして、呼び出し側が終了コード 2 で
// 返す。`--scop admin` のようなタイポを黙って既定で通さないためのもの。
// "-" 単独は位置引数(stdin / stdout の慣例)。
func parseArgs(args, values, bools []string) (pos []string, opts map[string]string, err error) {
	opts = map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case slices.Contains(bools, a):
			opts[a] = "true"
		case slices.Contains(values, a):
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("%s requires a value", a)
			}
			i++
			opts[a] = args[i]
		case strings.HasPrefix(a, "-") && a != "-":
			return nil, nil, fmt.Errorf("unknown flag %s", a)
		default:
			pos = append(pos, a)
		}
	}
	return pos, opts, nil
}

// parseNoFlags は引数を取らない(または位置引数だけの)コマンド用。
func parseNoFlags(args []string) (pos []string, err error) {
	pos, _, err = parseArgs(args, nil, nil)
	return pos, err
}

// argError は引数エラーを「sashiki <cmd>: <理由>」で出して終了コード 2 を返す。
// usage 全文だけ出して理由を伏せない(#325: show / sleep / wake がそうだった)。
func argError(cmd string, err error) int {
	fmt.Fprintf(os.Stderr, "sashiki %s: %v\n", cmd, err)
	return exitUsage
}
