// sashiki token: API トークン管理(仕様 13-3)。
// state.db を直接開くため root で実行する(sashikid の稼働中でも WAL で共存できる)。
// 平文トークンは作成時に 1 回だけ表示し、保存するのは SHA-256 のみ。
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/rikukadev/sashiki/internal/config"
	"github.com/rikukadev/sashiki/internal/state"
)

func cmdToken(args []string) int {
	if len(args) < 1 {
		return usageToken()
	}
	sub, rest := args[0], args[1:]
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "sashiki token: root で実行してください")
		return exitError
	}
	cfg, err := config.Load(tokenConfigPath(rest))
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki token: config:", err)
		return exitError
	}
	db, err := state.Open(cfg.StateDB)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki token: state db:", err)
		return exitError
	}
	defer func() { _ = db.Close() }()

	switch sub {
	case "create":
		return cmdTokenCreate(db, rest)
	case "list":
		return cmdTokenList(db)
	case "revoke":
		return cmdTokenRevoke(db, rest)
	default:
		return usageToken()
	}
}

func usageToken() int {
	fmt.Fprint(os.Stderr, `Usage:
  sashiki token create --name <name> [--scope branches|admin]
                                       発行 (平文は 1 回だけ表示)。既定 branches (#294):
                                         branches = ブランチの create/reset/recreate/delete/lease と読み取り
                                         admin    = 上に加え baseline publish/promote、drain、gc、
                                                    データブラウザ(任意 SQL)、hook 手動実行
  sashiki token list
  sashiki token revoke <name>
`)
	return exitUsage
}

func tokenConfigPath(args []string) string {
	for i, a := range args {
		if a == "--config" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return "/etc/sashiki/config.yaml"
}

func cmdTokenCreate(db *state.DB, args []string) int {
	name, scope := "", state.ScopeBranches
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--name" && i+1 < len(args):
			name = args[i+1]
			i++
		case args[i] == "--scope" && i+1 < len(args):
			scope = args[i+1]
			i++
		}
	}
	if name == "" {
		return usageToken()
	}
	if scope != state.ScopeBranches && scope != state.ScopeAdmin {
		fmt.Fprintf(os.Stderr, "sashiki token: --scope は branches | admin (got %q)\n", scope)
		return exitUsage
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		fmt.Fprintln(os.Stderr, "sashiki token:", err)
		return exitError
	}
	tok := "sashiki_" + hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(tok))
	if err := db.CreateToken(name, hex.EncodeToString(sum[:]), scope); err != nil {
		fmt.Fprintln(os.Stderr, "sashiki token:", err)
		return exitError
	}
	fmt.Printf("token '%s' (scope %s) を発行しました。この平文は二度と表示されません:\n\n  %s\n\nGitHub Secrets 等に保存してください。\n", name, scope, tok)
	return exitOK
}

func cmdTokenList(db *state.DB) int {
	tokens, err := db.ListTokens()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki token:", err)
		return exitError
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tSCOPE\tCREATED\tLAST_USED")
	for _, t := range tokens {
		last := "-"
		if t.LastUsedAt != nil {
			last = t.LastUsedAt.Format("2006-01-02 15:04")
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t.Name, t.Scope, t.CreatedAt.Format("2006-01-02 15:04"), last)
	}
	_ = tw.Flush()
	return exitOK
}

func cmdTokenRevoke(db *state.DB, args []string) int {
	if len(args) < 1 {
		return usageToken()
	}
	if err := db.RevokeToken(args[0]); err != nil {
		fmt.Fprintln(os.Stderr, "sashiki token:", err)
		return exitNotFound
	}
	fmt.Printf("token '%s' を無効化しました\n", args[0])
	return exitOK
}
