// sashiki-root-helper は sashikid が要る root 操作(zfs / zpool / systemctl)を
// allowlist で検証してから実行する小さなヘルパー(仕様 20-3、#276)。
//
//	sudo -n /usr/local/bin/sashiki-root-helper zfs clone dbpool/base@baseline dbpool/branches/pr-1
//
// sudoers は `sashiki ALL=(root) NOPASSWD: /usr/local/bin/sashiki-root-helper *`
// の 1 行だけ(sashiki init が生成)。許可する形は internal/roothelper が持ち、
// 設定は /etc/sashiki/root-helper.yaml(root 0600、引数で変えられない)。
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/rikukadev/sashiki/internal/roothelper"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 1 && (args[0] == "--version" || args[0] == "version") {
		fmt.Println("sashiki-root-helper")
		return 0
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "sashiki-root-helper: root で実行してください(sudo 経由で呼ばれる想定)")
		return 2
	}
	cfg, err := roothelper.Load(roothelper.ConfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sashiki-root-helper: 設定を読めません(sashiki init が生成する): %v\n", err)
		return 2
	}
	if err := cfg.Validate(args); err != nil {
		fmt.Fprintf(os.Stderr, "sashiki-root-helper: 拒否: %v\n", err)
		return 3
	}
	bin := roothelper.Binaries[args[0]]
	cmd := exec.Command(bin, args[1:]...)
	// 呼び出し元の環境は引き継がない(sudo が落とすが二重に)。
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
				return ws.ExitStatus()
			}
			return 1
		}
		fmt.Fprintf(os.Stderr, "sashiki-root-helper: %s: %v\n", bin, err)
		return 1
	}
	return 0
}
