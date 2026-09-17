package postgres

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rikukadev/sashiki/internal/engine"
)

// mockRun は実行されたコマンドを記録し、固定の応答を返す。
type mockRun struct {
	calls [][]string
	out   string
	err   error
}

func (m *mockRun) run(_ context.Context, name string, args ...string) (string, error) {
	m.calls = append(m.calls, append([]string{name}, args...))
	return m.out, m.err
}

func newTestEngine(t *testing.T, cfg Config) (*Engine, *mockRun) {
	t.Helper()
	if cfg.EnvDir == "" {
		cfg.EnvDir = t.TempDir()
	}
	e := New(cfg)
	m := &mockRun{}
	e.run = m.run
	return e, m
}

func TestStartWritesEnvAndStartsUnit(t *testing.T) {
	dir := t.TempDir()
	e, m := newTestEngine(t, Config{EnvDir: dir, BinDir: "/opt/pg/bin", ListenAddresses: "*"})

	ins := engine.Instance{Branch: "pg-1", DataDir: "/tpgpool/branches/pg-1/data", Port: 5433}
	if err := e.Start(context.Background(), ins); err != nil {
		t.Fatalf("Start: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "pg-1.env"))
	if err != nil {
		t.Fatalf("env file: %v", err)
	}
	env := string(data)
	for _, want := range []string{
		"PORT=5433\n",
		"DATADIR=/tpgpool/branches/pg-1/data\n",
		"PGBIN=/opt/pg/bin\n",
		"LISTEN_ADDRESSES=*\n",
		"SHARED_BUFFERS=\n", // 未指定なら空(unit 側の既定 128MB)
	} {
		if !strings.Contains(env, want) {
			t.Errorf("env file should contain %q, got:\n%s", want, env)
		}
	}

	if len(m.calls) != 1 {
		t.Fatalf("expected 1 command, got %v", m.calls)
	}
	got := strings.Join(m.calls[0], " ")
	if got != "systemctl start postgres-sashiki@pg-1" {
		t.Errorf("unexpected command: %s", got)
	}
}

func TestStartDefaultsListenAddressesToLoopback(t *testing.T) {
	dir := t.TempDir()
	e, _ := newTestEngine(t, Config{EnvDir: dir})

	if err := e.Start(context.Background(), engine.Instance{Branch: "pg-1", Port: 5433}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "pg-1.env"))
	if !strings.Contains(string(data), "LISTEN_ADDRESSES=127.0.0.1\n") {
		t.Errorf("listen_addresses should default to 127.0.0.1, got:\n%s", data)
	}
}

func TestStopStopsUnitAndRemovesEnv(t *testing.T) {
	dir := t.TempDir()
	e, m := newTestEngine(t, Config{EnvDir: dir})
	envPath := filepath.Join(dir, "pg-1.env")
	if err := os.WriteFile(envPath, []byte("PORT=5433\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := e.Stop(context.Background(), engine.Instance{Branch: "pg-1"}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	got := strings.Join(m.calls[0], " ")
	if got != "systemctl stop postgres-sashiki@pg-1" {
		t.Errorf("unexpected command: %s", got)
	}
	if _, err := os.Stat(envPath); !os.IsNotExist(err) {
		t.Errorf("env file should be removed after Stop")
	}
}

func TestStopKeepsEnvOnFailure(t *testing.T) {
	dir := t.TempDir()
	e, m := newTestEngine(t, Config{EnvDir: dir})
	m.err = errors.New("unit failed")
	envPath := filepath.Join(dir, "pg-1.env")
	if err := os.WriteFile(envPath, []byte("PORT=5433\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := e.Stop(context.Background(), engine.Instance{Branch: "pg-1"}); err == nil {
		t.Fatal("Stop should propagate systemctl error")
	}
	if _, err := os.Stat(envPath); err != nil {
		t.Errorf("env file should remain when stop fails (retry needs it)")
	}
}

func TestIsRunning(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{"active", true},
		{"inactive", false},
		{"failed", false},
	} {
		e, m := newTestEngine(t, Config{})
		m.out = tc.out
		got, err := e.IsRunning(context.Background(), engine.Instance{Branch: "pg-1"})
		if err != nil {
			t.Fatalf("IsRunning(%s): %v", tc.out, err)
		}
		if got != tc.want {
			t.Errorf("IsRunning(%s) = %v, want %v", tc.out, got, tc.want)
		}
	}
}

func TestWaitReadyTimesOut(t *testing.T) {
	// pg_isready が存在しないパスを指せば常に失敗し、タイムアウトに達する。
	e, _ := newTestEngine(t, Config{BinDir: "/nonexistent", ReadyTimeout: 300 * time.Millisecond})
	err := e.WaitReady(context.Background(), engine.Instance{Branch: "pg-1", Port: 5433})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestWaitReadyHonorsContextCancel(t *testing.T) {
	e, _ := newTestEngine(t, Config{BinDir: "/nonexistent", ReadyTimeout: 10 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := e.WaitReady(ctx, engine.Instance{Branch: "pg-1", Port: 5433})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline error, got %v", err)
	}
}
