package main

import (
	"strings"
	"testing"
)

// 各コマンドが「未知のフラグ・余分な引数は終了コード 2」を守る(#325)。
// API を叩く前に落ちるので sashikid 不要。
func TestStrictArgsExitUsage(t *testing.T) {
	cases := map[string][]string{
		"show --typo":           {"show", "pr-1", "--typo"},
		"show create-only flag": {"show", "pr-1", "--owner", "me"},
		"sleep --port":          {"sleep", "pr-1", "--port", "1"},
		"reset --ttl":           {"reset", "pr-1", "--ttl", "1h", "--no-wait"},
		"list --exist-ok":       {"list", "--exist-ok"},
		"env --foo":             {"env", "--foo", "pr-1"},
		"env --prefix no value": {"env", "pr-1", "--prefix"},
		"connect --foo":         {"connect", "--foo"},
		"hooks run extra":       {"hooks", "run", "pr-1", "on-create", "extra"},
		"hooks run --json":      {"hooks", "run", "--json", "pr-1", "on-create"},
		"op list --json":        {"op", "list", "--json"},
		"op show extra":         {"op", "show", "id", "extra"},
		"baseline set extra":    {"baseline", "set", "snap", "extra"},
		"baseline gc --typo":    {"baseline", "gc", "--typo"},
		"baseline refresh arg":  {"baseline", "refresh", "anything"},
		"baseline list extra":   {"baseline", "list", "x"},
		"lease --for no value":  {"lease", "renew", "pr-1", "--for"},
		"init --engine bad":     {"init", "--pool", "p", "--engine", "oracle", "--yes"},
		"export without --to":   {"baseline", "export"},
	}
	for name, args := range cases {
		if got := run(args); got != exitUsage {
			t.Errorf("%s: exit %d, want %d", name, got, exitUsage)
		}
	}
}

func TestParseArgs(t *testing.T) {
	pos, opts, err := parseArgs([]string{"a", "--name", "x", "--json", "-"}, []string{"--name"}, []string{"--json"})
	if err != nil || len(pos) != 2 || pos[1] != "-" || opts["--name"] != "x" || opts["--json"] != "true" {
		t.Errorf("parseArgs = %v %v %v", pos, opts, err)
	}
	if _, _, err := parseArgs([]string{"--scop", "admin"}, []string{"--scope"}, nil); err == nil || !strings.Contains(err.Error(), "--scop") {
		t.Errorf("typo flag should be reported, got %v", err)
	}
	if _, _, err := parseArgs([]string{"--scope"}, []string{"--scope"}, nil); err == nil {
		t.Error("value flag without a value should fail")
	}
}
