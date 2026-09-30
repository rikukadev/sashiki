// sashiki capacity: memory / storage / ports の空き表示(仕様 14-2)。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"
)

func cmdCapacity(args []string) int {
	jsonOut, allHosts := false, false
	for _, a := range args {
		switch a {
		case "--json":
			jsonOut = true
		case "--all-hosts":
			allHosts = true
		default:
			// 未知の引数を黙って無視しない(#308)。
			fmt.Fprintf(os.Stderr, "sashiki %s: 不明な引数 %s\n", "capacity", a)
			return exitUsage
		}
	}
	if allHosts {
		return capacityAllHosts(jsonOut)
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

// capacityAllHosts は全ホストの容量を並べる(#386)。どのホストに空きがあるか
// (= 次のブランチをどこに作るか)を 1 コマンドで見るためのもの。
func capacityAllHosts(jsonOut bool) int {
	results, err := fanOut("/v1/capacity")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitUsage
	}
	type hostCap struct {
		Host     string          `json:"host"`
		HostName string          `json:"host_name,omitempty"`
		Capacity json.RawMessage `json:"capacity"`
	}
	var merged []hostCap
	for _, r := range results {
		if r.Err != nil || r.Code != http.StatusOK {
			continue
		}
		var probe struct {
			HostName string `json:"host_name"`
		}
		_ = json.Unmarshal(r.Data, &probe)
		merged = append(merged, hostCap{Host: r.Name, HostName: probe.HostName, Capacity: r.Data})
	}
	if reportPartialFailure(results) {
		return exitError
	}
	if jsonOut {
		b, _ := json.Marshal(map[string]any{"hosts": merged})
		fmt.Println(string(b))
		return exitOK
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "HOST\tBRANCHES\tRUNNING\tPORTS\tPOOL_USED")
	for _, h := range merged {
		var c struct {
			Branches struct {
				Running     int `json:"running"`
				MaxBranches int `json:"max_branches"`
			} `json:"branches"`
			Ports struct {
				Used  int `json:"used"`
				Total int `json:"total"`
			} `json:"ports"`
			Storage struct {
				PoolUsedRatio float64 `json:"pool_used_ratio"`
			} `json:"storage"`
		}
		_ = json.Unmarshal(h.Capacity, &c)
		label := h.HostName
		if label == "" {
			label = h.Host
		}
		pool := "-"
		if c.Storage.PoolUsedRatio >= 0 {
			pool = fmt.Sprintf("%.0f%%", c.Storage.PoolUsedRatio*100)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d/%d\t%d\t%d/%d\t%s\n",
			label, c.Branches.Running, c.Branches.MaxBranches, c.Branches.Running,
			c.Ports.Used, c.Ports.Total, pool)
	}
	_ = tw.Flush()
	return exitOK
}
