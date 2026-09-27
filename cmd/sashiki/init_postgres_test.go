package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// 生成 config が実際に読める YAML で、postgres として妥当なこと。
func TestRenderPostgresConfig(t *testing.T) {
	out, err := renderPostgresConfig("mypool")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("生成 config が YAML として不正: %v", err)
	}

	eng, _ := doc["engine"].(map[string]any)
	if eng["type"] != "postgres" {
		t.Errorf("engine.type = %v, want postgres", eng["type"])
	}
	pg, _ := eng["postgres"].(map[string]any)
	if pg["app_user"] != "dev" {
		t.Errorf("app_user = %v, want dev", pg["app_user"])
	}
	// パスワードは config に入れず、置き場所だけを書く(#354)
	if _, ok := pg["app_pass"]; ok {
		t.Errorf("config に app_pass が入っている: %v", pg["app_pass"])
	}
	if pg["app_pass_file"] != appPassFilePath {
		t.Errorf("app_pass_file = %v, want %s", pg["app_pass_file"], appPassFilePath)
	}
	if bd, _ := pg["bin_dir"].(string); !strings.HasPrefix(bd, "/usr/lib/postgresql/") {
		t.Errorf("bin_dir = %q, want /usr/lib/postgresql/<ver>/bin", bd)
	}
	// #222 で proxy に対応したので、postgres でも proxy を有効にして配る。
	lis, _ := doc["listen"].(map[string]any)
	if p, _ := lis["proxy"].(string); !strings.HasSuffix(p, ":5432") {
		t.Errorf("listen.proxy = %q, want :5432", p)
	}
	// pool がテンプレートに反映されていること
	st, _ := doc["storage"].(map[string]any)
	zfs, _ := st["ebs-zfs"].(map[string]any)
	if zfs["pool"] != "mypool" || zfs["base_dataset"] != "mypool/base" {
		t.Errorf("pool 展開が不正: %v", zfs)
	}
}

// postgres は 8KB ページなので base / branches とも recordsize=8k で作る。
// branches 側は「クローンは名前空間上の親からプロパティを継承する」ため必須で、
// ここが抜けると全ブランチが既定 128k で動いてしまう。
func TestPostgresInitStepsUse8kRecordsize(t *testing.T) {
	steps := initStepsPostgres(initOpts{pool: "p", skipPackages: true})
	var base, branches bool
	for _, s := range steps {
		if strings.Contains(s.name, "p/base") {
			base = true
			if !strings.Contains(s.name, "recordsize=8k") {
				t.Errorf("base データセットは recordsize=8k であるべき: %q", s.name)
			}
		}
		if strings.Contains(s.name, "p/branches") {
			branches = true
			if !strings.Contains(s.name, "recordsize=8k") {
				t.Errorf("branches データセットも recordsize=8k であるべき: %q", s.name)
			}
		}
	}
	if !base || !branches {
		t.Fatalf("base/branches のステップが見つからない (base=%v branches=%v)", base, branches)
	}
}

// postgres の init は mysqld@ ではなく postgres-sashiki@ を入れること。
func TestPostgresInitInstallsPostgresUnit(t *testing.T) {
	steps := initStepsPostgres(initOpts{pool: "p", skipPackages: true})
	found := false
	for _, s := range steps {
		if strings.Contains(s.name, "postgres-sashiki@.service") {
			found = true
		}
		if strings.Contains(s.name, "mysqld@") {
			t.Errorf("postgres の init に mysql 用ステップが混ざっている: %q", s.name)
		}
	}
	if !found {
		t.Error("postgres-sashiki@.service を入れるステップが無い")
	}
}

func TestPgVersionOrdering(t *testing.T) {
	// 辞書順だと "9" > "16" になってしまうので数値順であること。
	if !pgVersionLess("9", "16") {
		t.Error("9 は 16 より古いはず")
	}
	if !pgVersionLess("9.6", "10") {
		t.Error("9.6 は 10 より古いはず")
	}
	if pgVersionLess("17", "16") {
		t.Error("17 は 16 より新しいはず")
	}
}

// #246: init の zpool ステップは、既存 pool があれば create ではなく import に
// 倒す。インスタンス差し替えでデータ EBS が残っている状況で init が止まらない
// ようにするための分岐で、mysql / postgres の両方に入っていること。
func TestZpoolStepMentionsImportForBothEngines(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []initStep
	}{
		{"mysql", initSteps(initOpts{pool: "p", skipPackages: true})},
		{"postgres", initStepsPostgres(initOpts{pool: "p", skipPackages: true})},
	} {
		found := false
		for _, s := range tc.steps {
			if strings.Contains(s.name, "zpool p") {
				found = true
				// device 未指定なら、import を試す前に分かるエラーで止まること
				err := s.run()
				if err == nil || !strings.Contains(err.Error(), "--device") {
					t.Errorf("%s: device 未指定時は --device を促すエラーになるべき: %v", tc.name, err)
				}
			}
		}
		if !found {
			t.Errorf("%s: zpool ステップが見つからない", tc.name)
		}
	}
}

// Homebrew の postgresql formula は版ごとに分かれるので、数値順で最新を選ぶこと
// (辞書順だと "postgresql@9" が "postgresql@17" より後に来てしまう)。
func TestBrewPostgresFormulaeOrder(t *testing.T) {
	// brew が無い環境では既定リストが返る。新しい版が先頭に来ていること。
	got := brewPostgresFormulae()
	if len(got) == 0 {
		t.Fatal("候補が空")
	}
	first := got[0]
	for _, f := range got[1:] {
		if f == "postgresql" {
			continue // 版なしは最後
		}
		a := strings.TrimPrefix(first, "postgresql@")
		b := strings.TrimPrefix(f, "postgresql@")
		if pgVersionLess(a, b) {
			t.Errorf("新しい版が先に来るべき: %v", got)
			break
		}
	}
}

// postgres 用の darwin config が妥当で、process モード + apfs + proxy になっていること。
func TestDarwinPostgresConfigTemplate(t *testing.T) {
	out, err := renderTmpl(configDarwinPostgresTmpl, map[string]string{
		"Root": "/tmp/r", "PgBinDir": "/opt/homebrew/opt/postgresql@17/bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("生成 config が YAML として不正: %v", err)
	}
	eng, _ := doc["engine"].(map[string]any)
	if eng["type"] != "postgres" {
		t.Errorf("engine.type = %v", eng["type"])
	}
	pg, _ := eng["postgres"].(map[string]any)
	if pg["mode"] != "process" {
		t.Errorf("macOS は systemd が無いので process モードであるべき: %v", pg["mode"])
	}
	if pg["bin_dir"] != "/opt/homebrew/opt/postgresql@17/bin" {
		t.Errorf("bin_dir = %v", pg["bin_dir"])
	}
	st, _ := doc["storage"].(map[string]any)
	if st["backend"] != "apfs" {
		t.Errorf("storage.backend = %v, want apfs", st["backend"])
	}
	lis, _ := doc["listen"].(map[string]any)
	if p, _ := lis["proxy"].(string); !strings.HasSuffix(p, ":5432") {
		t.Errorf("listen.proxy = %q", p)
	}
}

// engine に応じて接続コマンド例を変えること(postgres に mysql を案内しない)。
func TestConnectHintPerEngine(t *testing.T) {
	pg := connectHint(branchView{Name: "pr-1", User: "dev@pr-1", Host: "h", Port: 5433, Engine: "postgres"})
	if !strings.HasPrefix(pg, "psql ") {
		t.Errorf("postgres = %q, want psql ...", pg)
	}
	my := connectHint(branchView{Name: "pr-1", User: "dev@pr-1", Host: "h", Port: 3401, Engine: "mysql"})
	if !strings.HasPrefix(my, "mysql ") {
		t.Errorf("mysql = %q, want mysql ...", my)
	}
	// engine 未設定(旧サーバー)は従来どおり mysql
	if !strings.HasPrefix(connectHint(branchView{Host: "h"}), "mysql ") {
		t.Error("engine 未設定は mysql にフォールバックすべき")
	}
}
