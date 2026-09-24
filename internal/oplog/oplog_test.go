package oplog

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func captureJSON(t *testing.T, fn func()) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	fn()
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %q: %v", buf.String(), err)
	}
	return m
}

// operation 内のログには operation_id / operation_type / branch が付く(仕様 20-5)。
func TestLogfCarriesOperationAttrs(t *testing.T) {
	ctx := WithOperation(context.Background(), Operation{ID: "op_1", Type: "create", Branch: "pr-1", Extra: []any{"stage", "clone"}})
	m := captureJSON(t, func() { Logf(ctx, "cloned %s", "x") })
	for k, want := range map[string]string{"operation_id": "op_1", "operation_type": "create", "branch": "pr-1", "stage": "clone", "msg": "cloned x", "level": "INFO"} {
		if m[k] != want {
			t.Errorf("%s = %v, want %s (record %v)", k, m[k], want, m)
		}
	}
}

// operation の外では該当しない属性を付けない(空の operation_id を出さない)。
func TestLogfOutsideOperation(t *testing.T) {
	m := captureJSON(t, func() { Logf(context.Background(), "plain") })
	for _, k := range []string{"operation_id", "operation_type", "branch"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s must be absent outside an operation: %v", k, m)
		}
	}
	m = captureJSON(t, func() { Errorf(WithBranch(context.Background(), "pr-2"), "reaper: stop") })
	if m["branch"] != "pr-2" || m["level"] != "ERROR" {
		t.Errorf("branch-only context: %v", m)
	}
	if _, ok := m["operation_id"]; ok {
		t.Errorf("operation_id must be absent for branch-only context: %v", m)
	}
}
