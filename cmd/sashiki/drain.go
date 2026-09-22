// sashiki drain: 全 running branch を sleeping にする(仕様 17章 / #28)。
// instance_class 変更などメンテ前に mysqld を安全に停止するのに使う。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
)

func cmdDrain(args []string) int {
	jsonOut := false
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
			continue
		}
		// 未知の引数を黙って無視しない(#308)。
		fmt.Fprintf(os.Stderr, "sashiki %s: 不明な引数 %s\n", "drain", a)
		return exitUsage
	}
	code, data, err := call("POST", "/v1/drain", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	if jsonOut {
		fmt.Println(string(data))
		return exitOK
	}
	var res struct {
		Slept   []string          `json:"slept"`
		Skipped []string          `json:"skipped"`
		Failed  map[string]string `json:"failed"`
	}
	_ = json.Unmarshal(data, &res)
	fmt.Printf("drained: %d slept, %d skipped", len(res.Slept), len(res.Skipped))
	if len(res.Failed) > 0 {
		fmt.Printf(", %d failed", len(res.Failed))
	}
	fmt.Println()
	for name, reason := range res.Failed {
		fmt.Printf("  failed %s: %s\n", name, reason)
	}
	if len(res.Failed) > 0 {
		return exitError
	}
	return exitOK
}
