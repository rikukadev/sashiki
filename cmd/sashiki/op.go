// sashiki op: operation の照会(仕様 17章)。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"
	"time"
)

func cmdOp(args []string) int {
	if len(args) < 1 {
		return usageOp()
	}
	switch args[0] {
	case "list":
		return opList()
	case "show":
		if len(args) < 2 {
			return usageOp()
		}
		return opShow(args[1])
	case "wait":
		if len(args) < 2 {
			return usageOp()
		}
		return opWait(args[1:])
	default:
		return usageOp()
	}
}

func usageOp() int {
	fmt.Fprint(os.Stderr, "Usage:\n  sashiki op list\n  sashiki op show <id>\n  sashiki op wait <id> [--timeout <dur>] [--interval <dur>]\n")
	return exitUsage
}

type opView struct {
	ID         string  `json:"operation_id"`
	Type       string  `json:"type"`
	Target     string  `json:"target"`
	State      string  `json:"state"`
	StartedAt  string  `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
	Error      string  `json:"error"`
	ErrorCode  string  `json:"error_code"`
}

func opList() int {
	code, data, err := call("GET", "/v1/operations", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	var resp struct {
		Operations []opView `json:"operations"`
	}
	_ = json.Unmarshal(data, &resp)
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tTYPE\tTARGET\tSTATE\tSTARTED")
	for _, o := range resp.Operations {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", o.ID, o.Type, o.Target, o.State, o.StartedAt)
	}
	_ = tw.Flush()
	return exitOK
}

func opShow(id string) int {
	code, data, err := call("GET", "/v1/operations/"+id, nil)
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

// opWait は operation の完了を待つ。exit code は
//
//	0=completed / 1(exitError)=operation 失敗 / 6(exitTimeout)=タイムアウト。
func opWait(args []string) int {
	timeout := 10 * time.Minute
	interval := 200 * time.Millisecond
	var id string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--timeout":
			if i+1 >= len(args) {
				return usageOp()
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				fmt.Fprintln(os.Stderr, "sashiki: --timeout:", err)
				return exitUsage
			}
			timeout = d
		case "--interval":
			if i+1 >= len(args) {
				return usageOp()
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				fmt.Fprintln(os.Stderr, "sashiki: --interval:", err)
				return exitUsage
			}
			interval = d
		default:
			id = args[i]
		}
	}
	if id == "" {
		return usageOp()
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		code, data, err := call("GET", "/v1/operations/"+id, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "sashiki:", err)
			return exitError
		}
		if code != http.StatusOK {
			fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
			return statusToExit(code)
		}
		var o opView
		_ = json.Unmarshal(data, &o)
		if o.State != "running" {
			fmt.Printf("%s: %s\n", o.ID, o.State)
			if o.State == "failed" {
				fmt.Fprintln(os.Stderr, o.Error)
				return exitError
			}
			return exitOK
		}
		time.Sleep(interval)
	}
	fmt.Fprintf(os.Stderr, "sashiki: wait timed out after %s (operation still running)\n", timeout)
	return exitTimeout
}
