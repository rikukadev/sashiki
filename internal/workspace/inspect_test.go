package workspace

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/rikukadev/sashiki/internal/state"
)

// doctor / gc --orphans は稼働中の遷移(creating / resetting / deleting)を
// 「中断された」とみなして書き換えない(#303)。起動時の Reconcile だけが回収する。
func TestDoctorAndGCDoNotMutateInFlightBranches(t *testing.T) {
	ctx := context.Background()
	st := &mockStorage{}
	m := newTestManager(t, st, &mockEngine{}, "")
	for name, s := range map[string]string{"pr-c": state.StateCreating, "pr-r": state.StateResetting, "pr-d": state.StateDeleting} {
		if err := m.db.CreateBranch(name, map[string]int{"pr-c": 3401, "pr-r": 3402, "pr-d": 3403}[name], "pool/base@baseline"); err != nil {
			t.Fatal(err)
		}
		_ = m.db.SetState(name, s, "")
	}
	if _, err := m.Doctor(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GCOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"pr-c": state.StateCreating, "pr-r": state.StateResetting, "pr-d": state.StateDeleting} {
		b, err := m.db.GetBranch(name)
		if err != nil {
			t.Fatalf("%s must not be deleted by doctor/gc: %v", name, err)
		}
		if b.State != want {
			t.Errorf("%s state = %s, want %s (doctor/gc must not mutate)", name, b.State, want)
		}
	}
	if len(st.destroyed) != 0 {
		t.Errorf("doctor/gc must not resume deletes: %v", st.destroyed)
	}
}

// 別名の同時 create でポートが衝突せず、max_branches も超えない(#303)。
func TestConcurrentCreateRespectsLimitsAndPorts(t *testing.T) {
	ctx := context.Background()
	m := newTestManagerCfg(t, &mockStorage{}, &mockEngine{}, "", func(c *Config) {
		c.MaxBranches = 3
		c.PortLow, c.PortHigh = 3401, 3410
	})
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = m.Create(ctx, fmt.Sprintf("pr-%d", i), 0)
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		}
	}
	all, _ := m.db.ListBranches()
	if ok != 3 || len(all) != 3 {
		t.Errorf("created %d (rows %d), want exactly max_branches=3; errs=%v", ok, len(all), errs)
	}
	ports := map[int]bool{}
	for _, b := range all {
		if ports[b.Port] {
			t.Errorf("port %d allocated twice", b.Port)
		}
		ports[b.Port] = true
	}
}
