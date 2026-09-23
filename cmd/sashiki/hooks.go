// sashiki hooks run: 指定 event の hook を再実行(仕様 11-1)。
package main

import (
	"fmt"
	"net/http"
	"os"
)

func cmdHooks(args []string) int {
	if len(args) < 1 || args[0] != "run" {
		fmt.Fprintln(os.Stderr, "Usage:\n  sashiki hooks run <name> <event>")
		return exitUsage
	}
	pos, err := parseNoFlags(args[1:])
	if err != nil {
		return argError("hooks run", err)
	}
	if len(pos) != 2 {
		fmt.Fprintln(os.Stderr, "Usage:\n  sashiki hooks run <name> <event>")
		return exitUsage
	}
	name, event := pos[0], pos[1]
	code, data, err := call("POST", "/v1/branches/"+name+"/hooks/"+event, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	fmt.Printf("hook %s ran for %s\n", event, name)
	return exitOK
}
