package workspace

import (
	"context"
	"errors"
	"testing"

	"github.com/rikukadev/sashiki/internal/storage"
)

// promote 済みブランチの reset / recreate は拒否する(#289)。delete の保護(#179)
// と同じ理由で、ebs-zfs では baseline snapshot が branch dataset 上にあり、
// rollback -r はそれを破棄し、recreate の rename → destroy -r は current baseline を
// 存在しない名前に向ける。どちらも状態を変える前に止める。
func TestResetRefusesBranchBackingBaseline(t *testing.T) {
	ctx := context.Background()
	eng := &mockEngine{}
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}}
	m := newTestManager(t, st, eng, "")
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PromoteBranch(ctx, "pr-1", PromoteOptions{}); err != nil {
		t.Fatal(err)
	}
	rollbacks := len(st.rollbacks)
	killed := len(eng.killed)

	_, err := m.Reset(ctx, "pr-1")
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("reset of promote-source branch should be refused, got %v", err)
	}
	// 拒否は状態を変える前: rollback も Kill も走らず、state も running のまま。
	if len(st.rollbacks) != rollbacks {
		t.Errorf("rollback should not run on refused reset: %v", st.rollbacks)
	}
	if len(eng.killed) != killed {
		t.Errorf("mysqld should not be killed on refused reset: %v", eng.killed)
	}
	if b, _ := m.db.GetBranch("pr-1"); b.State != "running" {
		t.Errorf("state should stay running after refused reset, got %s", b.State)
	}
	// show / list で識別できる(#289)。
	if i, _ := m.Get(ctx, "pr-1"); len(i.BackingBaselines) != 1 {
		t.Errorf("Info.BackingBaselines should list the promoted snapshot, got %v", i.BackingBaselines)
	}

	// 別 baseline に切り替えれば(実体がこのブランチ上に無くなれば)通る。
	// ここでは promote 前の base 側 baseline に set し直す。
	if err := m.db.SetCurrentBaseline("pool/base@baseline"); err != nil {
		t.Fatal(err)
	}
	// baselines テーブルの行はまだ pr-1@ を指しているので依然拒否される
	// (current でなくても登録済みなら実体は必要)。
	if _, err := m.Reset(ctx, "pr-1"); !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("registered (non-current) baseline on the dataset should still refuse reset, got %v", err)
	}
	// baseline 自体を消せば reset できる。
	bls, _ := m.db.ListBaselines()
	for _, bl := range bls {
		if err := m.db.DeleteBaseline(bl.Snapshot); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Reset(ctx, "pr-1"); err != nil {
		t.Errorf("reset should succeed once no baseline lives on the dataset: %v", err)
	}
}

func TestRecreateRefusesBranchBackingBaseline(t *testing.T) {
	ctx := context.Background()
	eng := &mockEngine{}
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}}
	m := newTestManager(t, st, eng, "")
	if _, err := m.Create(ctx, "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PromoteBranch(ctx, "pr-1", PromoteOptions{}); err != nil {
		t.Fatal(err)
	}
	renamed := len(st.renamed)
	cloned := len(st.cloned)

	_, err := m.Recreate(ctx, "pr-1")
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("recreate of promote-source branch should be refused, got %v", err)
	}
	// rename(退避)も clone も走っていない。
	if len(st.renamed) != renamed {
		t.Errorf("dataset should not be renamed on refused recreate: %v", st.renamed)
	}
	if len(st.cloned) != cloned {
		t.Errorf("clone should not run on refused recreate: %v", st.cloned)
	}
	if b, _ := m.db.GetBranch("pr-1"); b.State != "running" {
		t.Errorf("state should stay running after refused recreate, got %s", b.State)
	}

	// baseline を持たない別ブランチは普通に recreate できる。
	if _, err := m.Create(ctx, "pr-2", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Recreate(ctx, "pr-2"); err != nil {
		t.Errorf("non-backing branch should recreate cleanly: %v", err)
	}
}
