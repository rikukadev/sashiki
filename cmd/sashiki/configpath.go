package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// defaultConfigPath は CLI が sashikid の config を読むときの既定パス(#293)。
//
// Linux は /etc/sashiki/config.yaml。macOS ネイティブは init が
// ~/Library/Application Support/sashiki[-pg]/config.yaml に書くので、そこに
// あればそれを使う。README の手順(baseline import / token)を Mac で --config
// 無しに通すため。SASHIKI_CONFIG があれば何より優先する。
//
// MySQL 版と Postgres 版の両方があるときは黙ってどちらかを選ばず、空文字を返す
// (呼び出し側は requireConfigPath で --config / SASHIKI_CONFIG を促す、#310 review)。
func defaultConfigPath() string {
	if p := os.Getenv("SASHIKI_CONFIG"); p != "" {
		return p
	}
	if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err == nil {
			var found []string
			for _, dir := range []string{"sashiki", "sashiki-pg"} {
				p := filepath.Join(home, "Library", "Application Support", dir, "config.yaml")
				if _, err := os.Stat(p); err == nil {
					found = append(found, p)
				}
			}
			switch len(found) {
			case 1:
				return found[0]
			case 2:
				return ""
			}
		}
	}
	return "/etc/sashiki/config.yaml"
}

// requireConfigPath は defaultConfigPath が決められなかった(複数候補)ときに
// 分かるエラーを返す。
func requireConfigPath(p string) (string, error) {
	if p != "" {
		return p, nil
	}
	return "", fmt.Errorf("config が複数あります(~/Library/Application Support/sashiki と sashiki-pg)。" +
		"--config <path> か SASHIKI_CONFIG で指定してください")
}

// localBaselineTag は apfs / reflink の import で使う snapshot tag を返す(#293)。
// 初回は config の baseline_snapshot(既定 "baseline")。既に存在するなら失敗
// させず、新しい tag で取って current を切り替える(zfs の refresh と同じ意味)。
// 既存 snapshot は残るので、そこから作ったブランチの reset / recreate は壊れない。
func localBaselineTag(snapDir, preferred string) (tag string, replacing bool) {
	if _, err := os.Stat(filepath.Join(snapDir, preferred)); err != nil {
		return preferred, false
	}
	return "baseline-" + time.Now().UTC().Format("20060102T150405Z"), true
}
