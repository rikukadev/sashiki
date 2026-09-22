// sashiki doctor / gc --orphans(仕様 20-2)。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
)

type doctorResp struct {
	BranchCount int `json:"branch_count"`
	Checks      []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	} `json:"checks"`
}

// cmdDoctor は健全性チェックを [OK]/[WARN]/[ERROR] で整形表示する(仕様 20-2)。
// --json で API 生レスポンスを素通しする。error が 1 つでもあれば非ゼロ終了。
func cmdDoctor(args []string) int {
	jsonOut := false
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
			continue
		}
		// 未知の引数を黙って無視しない(#308)。
		fmt.Fprintf(os.Stderr, "sashiki %s: 不明な引数 %s\n", "doctor", a)
		return exitUsage
	}
	code, data, err := call("GET", "/v1/doctor", nil)
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
	var d doctorResp
	if err := json.Unmarshal(data, &d); err != nil {
		// 整形できないときは素通しにフォールバック
		fmt.Println(string(data))
		return exitOK
	}
	hasError := false
	for _, c := range d.Checks {
		label := "[OK]   "
		switch c.Status {
		case "warn":
			label = "[WARN] "
		case "error":
			label = "[ERROR]"
			hasError = true
		}
		line := label + " " + c.Name
		if c.Detail != "" {
			line += ": " + c.Detail
		}
		fmt.Println(line)
	}
	fmt.Printf("branches: %d\n", d.BranchCount)
	if hasError {
		return exitError
	}
	return exitOK
}

func cmdGC(args []string) int {
	orphans := false
	for _, a := range args {
		if a == "--orphans" {
			orphans = true
			continue
		}
		fmt.Fprintf(os.Stderr, "sashiki gc: 不明な引数 %s\n", a)
		return exitUsage
	}
	if !orphans {
		fmt.Fprintln(os.Stderr, "Usage: sashiki gc --orphans")
		return exitUsage
	}
	code, data, err := call("POST", "/v1/gc/orphans", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	fmt.Println(string(data))
	return exitOK
}
