// sashiki init のハードニング関連: sudoers 生成(#78)と AppArmor プロファイル生成(#79)。
// どちらも「pool 名を受けて内容文字列を返す」純関数に切り出し、ユニットテストで
// 必要行の存在と危険形の不在を検証できるようにしている。
package main

import (
	"fmt"
	"os"
	"strings"
)

// --- sudoers (#78) ---

const (
	sudoersPath = "/etc/sudoers.d/sashiki"
	// "." を含むファイル名は sudo が読み込まないため、検証前の一時ファイルが
	// 万一残っても sudoers として有効化されることはない。
	sudoersTmpPath = "/etc/sudoers.d/.sashiki.tmp"
)

// rootHelperBin は deb / tarball が置く helper のパス。config の root_helper と
// sudoers の両方がこれを指す。
const rootHelperBin = "/usr/local/bin/sashiki-root-helper"

// sudoersContent は /etc/sudoers.d/sashiki の内容(root-helper 方式、#276)。
// sashiki ユーザーに許すのは helper 1 本だけで、zfs / zpool / systemctl の引数は
// helper が allowlist で検証する(internal/roothelper)。sudoers の `*` は空白を
// またぐため、旧方式のパターン行では `-o mountpoint=/etc` のような追加引数を
// 防げなかった。
func sudoersContent(pool string) string {
	return strings.Join([]string{
		"# sashiki: root 操作は sashiki-root-helper だけを許可(sashiki init が生成、#276)。",
		"# 許可する zfs / zpool / systemctl の形は helper が /etc/sashiki/root-helper.yaml を見て検証する",
		"# (internal/roothelper)。pool: " + pool,
		"sashiki ALL=(root) NOPASSWD: " + rootHelperBin + " *",
	}, "\n") + "\n"
}

// sudoersLegacyContent は helper を使わない旧方式(config に root_helper が無い
// 既存ホスト向け)。実際の呼び出し形(internal/storage/ebszfs/zfs.go、
// internal/engine/{mysql,postgres})に 1:1 で対応させ、末尾をデータセット/ユニットの
// パターンで縛る。
//
// 注意: sudoers の `*` は空白をまたいでマッチする。そのため複数引数やフラグを
// 取れるコマンドは「末尾がデータセットパターンで終わる」だけでは防げない
// (例: `zfs clone <base>@x -o mountpoint=/etc <branches>/y` は
// `clone <base>@* <branches>/*` にマッチしてしまう)。ここでは実呼び出しに
// 合わせて可能な限り引数個数が固定の形で書き、`zfs set` のような
// 任意プロパティ変更系は一切許可しない。完全な閉じ込めは root-helper(sudoersContent)。
func sudoersLegacyContent(pool string) string {
	base := pool + "/base"
	br := pool + "/branches"
	lines := []string{
		"# sashiki: 限定的な root 操作のみ許可(sashiki init が生成。旧方式。root-helper は #276)",
		"# 実呼び出し形は internal/storage/ebszfs/zfs.go / internal/engine/{mysql,postgres} を参照。",
		"# 注意: sudoers の * は空白をまたぐため、末尾パターンによる制限は完全ではない。",
		// clone: baseline snapshot → branches 配下のみ(引数 2 個の固定形)。
		// base@* に加え、promote 済み baseline(branch dataset 上の @baseline-*)からの
		// clone も許可する(promote 後に新規ブランチを作れるように、#178)。
		fmt.Sprintf("sashiki ALL=(root) NOPASSWD: /usr/sbin/zfs clone %s@* %s/*", base, br),
		fmt.Sprintf("sashiki ALL=(root) NOPASSWD: /usr/sbin/zfs clone %s/*@baseline-* %s/*", br, br),
		// snapshot: branch の @init、base の新ベースライン、promote(branch@baseline-*)。
		fmt.Sprintf("sashiki ALL=(root) NOPASSWD: /usr/sbin/zfs snapshot %s/*@init", br),
		fmt.Sprintf("sashiki ALL=(root) NOPASSWD: /usr/sbin/zfs snapshot %s@*", base),
		fmt.Sprintf("sashiki ALL=(root) NOPASSWD: /usr/sbin/zfs snapshot %s/*@baseline-*", br),
		// rollback: branch の @init への巻き戻しのみ
		fmt.Sprintf("sashiki ALL=(root) NOPASSWD: /usr/sbin/zfs rollback -r %s/*@init", br),
		// destroy: branch の再帰破棄と base snapshot のみ。base snapshot 側は
		// @ を必須にしているため base データセット本体や pool は破棄できない。
		fmt.Sprintf("sashiki ALL=(root) NOPASSWD: /usr/sbin/zfs destroy -r %s/*", br),
		fmt.Sprintf("sashiki ALL=(root) NOPASSWD: /usr/sbin/zfs destroy %s@*", base),
		// rename: branches 配下同士のみ(recreate の退避スワップ)
		fmt.Sprintf("sashiki ALL=(root) NOPASSWD: /usr/sbin/zfs rename %s/* %s/*", br, br),
		// 参照系(read-only。get/list は状態を変更しない)
		"sashiki ALL=(root) NOPASSWD: /usr/sbin/zfs get *",
		"sashiki ALL=(root) NOPASSWD: /usr/sbin/zfs list *",
		"sashiki ALL=(root) NOPASSWD: /usr/sbin/zpool list *",
		// systemctl: sashiki のインスタンスユニットに限定。kill は SIGKILL 固定。
		"sashiki ALL=(root) NOPASSWD: /usr/bin/systemctl start mysqld@*, /usr/bin/systemctl stop mysqld@*, /usr/bin/systemctl kill -s SIGKILL mysqld@*, /usr/bin/systemctl is-active mysqld@*",
		"sashiki ALL=(root) NOPASSWD: /usr/bin/systemctl start postgres-sashiki@*, /usr/bin/systemctl stop postgres-sashiki@*, /usr/bin/systemctl kill -s SIGKILL postgres-sashiki@*, /usr/bin/systemctl is-active postgres-sashiki@*",
	}
	return strings.Join(lines, "\n") + "\n"
}

// sudoersFor は helper を使うかどうかで内容を選ぶ。既存 config に root_helper が
// 無いホストは旧方式のまま(config を書き換えないと sashikid が動かなくなるため)。
func sudoersFor(pool string, useHelper bool) string {
	if useHelper {
		return sudoersContent(pool)
	}
	return sudoersLegacyContent(pool)
}

// rootHelperConfigContent は helper が読む /etc/sashiki/root-helper.yaml。
func rootHelperConfigContent(pool string) string {
	return fmt.Sprintf("# sashiki-root-helper が許可する対象(sashiki init が生成、#276)\npool: %s\nbase_dataset: %s/base\nbranch_parent: %s/branches\nunits: [mysqld, postgres-sashiki]\n", pool, pool, pool)
}

// installSudoers は sudoers を一時ファイルに書き、visudo -cf で構文検証してから
// 本置きする。検証に失敗した場合は本体を書き換えずエラーを返す。
func installSudoers(pool string, useHelper bool) error {
	content := sudoersFor(pool, useHelper)
	if err := os.WriteFile(sudoersTmpPath, []byte(content), 0o440); err != nil {
		return err
	}
	if err := runCmd(nil, "visudo", "-cf", sudoersTmpPath); err != nil {
		_ = os.Remove(sudoersTmpPath)
		return fmt.Errorf("sudoers の visudo 検証に失敗(本体は変更していない): %w", err)
	}
	return os.Rename(sudoersTmpPath, sudoersPath)
}

// --- AppArmor (#79) ---

const (
	apparmorProfilePath = "/etc/apparmor.d/sashiki-mysqld"
	// 旧 PoC が置いていた disable symlink。残っていると再起動でプロファイルが
	// 無効化されるため init が掃除する。
	apparmorLegacyDisableLink = "/etc/apparmor.d/disable/usr.sbin.mysqld"
)

// apparmorProfile は mysqld を閉じ込める AppArmor プロファイル全文を生成する。
// Ubuntu 24.04 の mysql-server 同梱 /etc/apparmor.d/usr.sbin.mysqld は中身が
// 空のプレースホルダ(プロファイルブロック無し)で、local override 方式は
// どこからも include されず no-op になるため、sashiki が完全なプロファイルを
// 配布して enforce でロードする(#79)。
// 許可パスは sashiki が mysqld を動かす全形態を網羅する:
//   - mysqld@<branch>(datadir=/<pool>/branches/<name>/data、/tmp/mysql-*.sock)
//   - baseline import(datadir=/<pool>/base/data、/tmp/sashiki-baseline.*)
//   - refresh スクリプト(/tmp/sashiki-refresh.*、/var/log/sashiki/refresh.err)
func apparmorProfile(pool string) string {
	return fmt.Sprintf(`# sashiki が生成する mysqld の AppArmor プロファイル(sashiki init #79)。
# datadir(/%[1]s/branches, /%[1]s/base)の外への書込を enforce で防ぐ。
abi <abi/3.0>,

#include <tunables/global>

profile sashiki-mysqld /usr/sbin/mysqld flags=(attach_disconnected) {
  #include <abstractions/base>
  #include <abstractions/nameservice>
  #include <abstractions/user-tmp>
  #include <abstractions/mysql>

  capability chown,
  capability dac_override,
  capability dac_read_search,
  capability fowner,
  capability fsetid,
  capability setgid,
  capability setuid,
  capability sys_nice,
  capability sys_resource,

  network inet stream,
  network inet6 stream,
  network inet dgram,
  network inet6 dgram,
  network unix stream,
  network unix dgram,
  network netlink raw,

  /usr/sbin/mysqld mr,
  /usr/lib/mysql/plugin/ r,
  /usr/lib/mysql/plugin/** mr,
  /usr/share/mysql*/ r,
  /usr/share/mysql*/** r,
  /etc/mysql/ r,
  /etc/mysql/** r,
  /etc/sashiki/ r,
  /etc/sashiki/*.cnf r,
  /etc/hosts.allow r,
  /etc/hosts.deny r,
  /etc/ssl/openssl.cnf r,

  # datadir: branch と base(baseline import / refresh)のみ書込可
  /%[1]s/ r,
  /%[1]s/branches/ r,
  /%[1]s/branches/** rwk,
  /%[1]s/base/ r,
  /%[1]s/base/** rwk,

  # mysqld のエラーログと実行時ファイル
  /var/log/sashiki/ r,
  /var/log/sashiki/** rw,
  /run/sashiki/ r,
  /run/sashiki/** rw,

  # deb 版 mysqld の secure_file_priv 既定ディレクトリ(起動時の存在チェックのみ)
  /var/lib/mysql-files/ r,

  # deb 版 /etc/mysql の既定 log_error(--log-error を渡し忘れた場合の逃げ道。
  # ユーザーデータは含まれない)
  /var/log/mysql/ rw,
  /var/log/mysql/** rw,

  # mysqlx プラグインの既定ソケット位置(--skip-mysqlx を渡さない
  # baseline import / refresh の mysqld が触れることがある。ユーザーデータ無し)
  /run/mysqld/ rw,
  /run/mysqld/** rwk,

  # ソケット・pid(mysqld@<branch> / baseline import / refresh)
  /tmp/mysql-*.sock* rwk,
  /tmp/mysql-*.pid rwk,
  /tmp/sashiki-*.sock* rwk,
  /tmp/sashiki-*.pid rwk,

  # InnoDB / パフォーマンススキーマが参照するカーネル情報
  @{PROC}/ r,
  @{PROC}/@{pid}/status r,
  @{PROC}/@{pid}/mounts r,
  @{PROC}/@{pid}/cgroup r,
  @{PROC}/sys/vm/overcommit_memory r,
  @{PROC}/sys/fs/aio-max-nr r,
  /sys/devices/system/cpu/ r,
  /sys/devices/system/cpu/** r,
  /sys/devices/system/node/ r,
  /sys/devices/system/node/** r,
  /sys/fs/cgroup/ r,
  /sys/fs/cgroup/** r,
}
`, pool)
}

// installApparmorProfile はプロファイルを書き出して apparmor_parser -r でロードする。
// ロード失敗は init 全体を止めず、警告を stderr に出して続行する
// (mysqld を起動不能にして壊すより、閉じ込めを緩める方を選ぶ。ただし無言にしない)。
func installApparmorProfile(pool string) error {
	// 旧 e2e / PoC がロードした "/usr/sbin/mysqld" プロファイルがカーネルに残って
	// いると attach が競合して本プロファイルが効かないため、先にアンロードする
	if f, err := os.OpenFile("/sys/kernel/security/apparmor/.remove", os.O_WRONLY, 0); err == nil {
		_, _ = f.WriteString("/usr/sbin/mysqld")
		_ = f.Close()
	}
	// 旧 PoC の disable symlink を掃除(残っていると再起動で無効化される)
	if fi, err := os.Lstat(apparmorLegacyDisableLink); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(apparmorLegacyDisableLink); err != nil {
			fmt.Fprintf(os.Stderr, "sashiki init: 警告: 旧 disable symlink の削除に失敗: %v\n", err)
		}
	}
	if err := os.WriteFile(apparmorProfilePath, []byte(apparmorProfile(pool)), 0o644); err != nil {
		return err
	}
	if err := runCmd(nil, "apparmor_parser", "-r", apparmorProfilePath); err != nil {
		fmt.Fprintf(os.Stderr, "sashiki init: 警告: AppArmor プロファイルのロードに失敗しました。mysqld は閉じ込めなしで動きます: %v\n", err)
	}
	return nil
}
