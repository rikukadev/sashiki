// sashiki capacity: memory / storage / ports の空き表示(仕様 14-2)。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
)

func cmdCapacity(args []string) int {
	jsonOut := false
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
			continue
		}
		// 未知の引数を黙って無視しない(#308)。
		fmt.Fprintf(os.Stderr, "sashiki %s: 不明な引数 %s\n", "capacity", a)
		return exitUsage
	}
	code, data, err := call("GET", "/v1/capacity", nil)
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
	var c struct {
		Storage struct {
			Introspectable bool    `json:"introspectable"`
			PoolUsedBytes  int64   `json:"pool_used_bytes"`
			PoolTotalBytes int64   `json:"pool_total_bytes"`
			PoolUsedRatio  float64 `json:"pool_used_ratio"`
		} `json:"storage"`
		Ports  struct{ Used, Total int } `json:"ports"`
		Memory struct {
			AvailableBytes int64 `json:"available_bytes"`
		} `json:"memory"`
		Branches struct {
			Running, MaxRunning, MaxBranches int
		} `json:"branches"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		fmt.Println(string(data)) // 整形できなければ素通し
		return exitOK
	}
	pct := "-"
	if c.Storage.Introspectable && c.Storage.PoolUsedRatio >= 0 {
		pct = fmt.Sprintf("%.0f%%", c.Storage.PoolUsedRatio*100)
	}
	fmt.Printf("storage:  %s / %s  (%s used)\n",
		humanBytes(c.Storage.PoolUsedBytes), humanBytes(c.Storage.PoolTotalBytes), pct)
	fmt.Printf("memory:   %s available\n", humanBytes(c.Memory.AvailableBytes))
	fmt.Printf("ports:    %d / %d used\n", c.Ports.Used, c.Ports.Total)
	fmt.Printf("branches: %d running (max_running %d, max_branches %d)\n",
		c.Branches.Running, c.Branches.MaxRunning, c.Branches.MaxBranches)
	return exitOK
}
