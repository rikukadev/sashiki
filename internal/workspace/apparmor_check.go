package workspace

import (
	"os"
	"path/filepath"
)

// apparmorRoot は AppArmor 設定を探す起点。テストで差し替える。
var apparmorRoot = "/"

const (
	apparmorOwnProfile       = "etc/apparmor.d/sashiki-mysqld"
	apparmorStockProfile     = "etc/apparmor.d/usr.sbin.mysqld"
	apparmorStockDisableLink = "etc/apparmor.d/disable/usr.sbin.mysqld"
)

// apparmorStockConflict は、mysql-server 同梱のプロファイルが sashiki のものと
// 競合する状態かを返す(#376)。applicable が false なら対象外(sashiki のプロファイルが
// 無い、または同梱プロファイルが無い)。
//
// **ロード中のプロファイルではなくディスクを見る。** 理由は 2 つ。
//   - /sys/kernel/security/apparmor/profiles は root でないと読めず、sashikid
//     (User=sashiki)からは確認できない
//   - ディスクの状態は「次の再起動で壊れる」を再起動の前に言える。ロード状態は
//     壊れた後にしか分からない
func apparmorStockConflict(root string) (applicable, conflict bool) {
	if _, err := os.Stat(filepath.Join(root, apparmorOwnProfile)); err != nil {
		return false, false
	}
	if _, err := os.Stat(filepath.Join(root, apparmorStockProfile)); err != nil {
		return false, false
	}
	if _, err := os.Lstat(filepath.Join(root, apparmorStockDisableLink)); err != nil {
		return true, true
	}
	return true, false
}
