// baseline のストリーム配布(#243)。`zfs send` で baseline snapshot を書き出し、
// 別ホスト(や作り直したホスト)で `zfs recv` して current baseline に登録する。
//
// 狙いは「1 台で作って配る」。各ホストで毎朝 import を回す(数十分)代わりに、
// 1 台が作った baseline をストリームとして配れば、受け側は展開するだけで済む。
// EC2 を作り直したときの復旧経路にもなる。
//
// ZFS 固有の機能なので ebs-zfs / fsx-zfs のみ。apfs / reflink では使えない。
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/rikukadev/sashiki/internal/config"
)

type baselineStreamOpts struct {
	path       string // --to / --from
	configPath string
	snapshot   string // export: 送出する snapshot タグ(既定 = 現在の baseline)
	force      string // import-stream: --force(既存 base を置き換える)
}

// cmdBaselineExport は `sashiki baseline export --to <dest>`。
func cmdBaselineExport(args []string) int {
	opts := baselineStreamOpts{configPath: defaultConfigPath()}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--to":
			i++
			if i >= len(args) {
				return usageBaseline()
			}
			opts.path = args[i]
		case "--config":
			i++
			if i >= len(args) {
				return usageBaseline()
			}
			opts.configPath = args[i]
		case "--snapshot":
			i++
			if i >= len(args) {
				return usageBaseline()
			}
			opts.snapshot = args[i]
		default:
			return usageBaseline()
		}
	}
	if opts.path == "" {
		fmt.Fprintln(os.Stderr, "sashiki baseline export: --to <path|s3://...|-> が必要です")
		return exitError
	}
	cfg, err := config.Load(opts.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sashiki baseline export: config: %v\n", err)
		return exitError
	}
	if err := runBaselineExport(cfg, opts); err != nil {
		fmt.Fprintf(os.Stderr, "sashiki baseline export: %v\n", err)
		return exitError
	}
	return exitOK
}

func runBaselineExport(cfg config.Config, opts baselineStreamOpts) error {
	if err := requireZfsBackend(cfg, "export"); err != nil {
		return err
	}
	base := cfg.Storage.Zfs.BaseDataset
	tag := opts.snapshot
	if tag == "" {
		tag = cfg.Storage.Zfs.BaselineSnapshot
	}
	snap := base + "@" + tag
	if err := exec.Command("zfs", "list", "-t", "snapshot", snap).Run(); err != nil {
		return fmt.Errorf("snapshot %s がありません(baseline list で確認)", snap)
	}

	sink, err := createSink(opts.path)
	if err != nil {
		return err
	}
	// 進捗は stderr へ。標準出力は "-" のときストリーム本体に使うので汚さない。
	fmt.Fprintf(os.Stderr, "→ zfs send %s → %s\n", snap, opts.path)

	cmd := exec.Command("zfs", "send", snap)
	cmd.Stdout = sink
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		_ = sink.Close()
		return fmt.Errorf("zfs send %s: %w", snap, err)
	}
	// Close で S3 アップロードの完了(と失敗)まで見る。
	if err := sink.Close(); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "baseline export 完了")
	return nil
}

// cmdBaselineImportStream は `sashiki baseline import-stream --from <src>`。
func cmdBaselineImportStream(args []string) int {
	opts := baselineStreamOpts{configPath: defaultConfigPath()}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--from":
			i++
			if i >= len(args) {
				return usageBaseline()
			}
			opts.path = args[i]
		case "--config":
			i++
			if i >= len(args) {
				return usageBaseline()
			}
			opts.configPath = args[i]
		case "--force":
			opts.force = "yes"
		default:
			return usageBaseline()
		}
	}
	if opts.path == "" {
		fmt.Fprintln(os.Stderr, "sashiki baseline import-stream: --from <path|s3://...|-> が必要です")
		return exitError
	}
	cfg, err := config.Load(opts.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sashiki baseline import-stream: config: %v\n", err)
		return exitError
	}
	if err := runBaselineImportStream(cfg, opts); err != nil {
		fmt.Fprintf(os.Stderr, "sashiki baseline import-stream: %v\n", err)
		return exitError
	}
	return exitOK
}

func runBaselineImportStream(cfg config.Config, opts baselineStreamOpts) error {
	if err := requireZfsBackend(cfg, "import-stream"); err != nil {
		return err
	}
	base := cfg.Storage.Zfs.BaseDataset
	branchParent := cfg.Storage.Zfs.BranchParent

	// 既存ブランチがあると受け入れは危険。ブランチは base の snapshot の clone なので、
	// base を作り直すと origin を失う(zfs 側も clone があると destroy を拒む)。
	if branches, err := listChildDatasets(branchParent); err == nil && len(branches) > 0 {
		return fmt.Errorf("ブランチが %d 個残っています。先に削除してください(clone があると base を置き換えられません): %s",
			len(branches), strings.Join(branches, ", "))
	}

	baseExists := exec.Command("zfs", "list", base).Run() == nil
	if baseExists {
		if opts.force == "" {
			return fmt.Errorf("%s が既に存在します。置き換えるなら --force を付けてください"+
				"(既存の baseline snapshot は失われます)", base)
		}
		// 完全ストリームの受け入れは、宛先に snapshot があると -F でも
		// "destination has snapshots ... must destroy them to overwrite it" で
		// 拒否される。clone(ブランチ)が無いことは上で確認済みなので、
		// ここで base を破棄してから受け入れる。
		fmt.Fprintf(os.Stderr, "→ 既存の %s を破棄して受け入れます(--force)\n", base)
		if out, err := exec.Command("zfs", "destroy", "-r", base).CombinedOutput(); err != nil {
			return fmt.Errorf("既存 base の破棄に失敗: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}

	src, err := openSource(opts.path)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	fmt.Fprintf(os.Stderr, "→ zfs recv %s ← %s\n", base, opts.path)
	// ここに来た時点で base は存在しない(--force なら直前に破棄した)ので、
	// 素の recv でよい。
	cmd := exec.Command("zfs", "recv", base)
	cmd.Stdin = src
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("zfs recv %s: %w", base, err)
	}
	if err := src.Close(); err != nil {
		return err
	}

	// 受け取った snapshot を current baseline として登録する。送出側のタグが
	// そのまま来るので、実際に何が来たかを見て決める(決め打ちにしない)。
	snap, err := latestSnapshot(base)
	if err != nil {
		return err
	}
	tag := snap
	if i := strings.Index(snap, "@"); i >= 0 {
		tag = snap[i+1:]
	}
	registerImportedBaseline(cfg.StateDB, snap, tag)
	fmt.Fprintf(os.Stderr, "baseline import-stream 完了(current = %s)。sashiki create <name> でブランチを作れます\n", snap)
	return nil
}

// requireZfsBackend は zfs 系 backend でのみ使える機能であることを明示する。
func requireZfsBackend(cfg config.Config, what string) error {
	switch cfg.Storage.Backend {
	case "ebs-zfs", "fsx-zfs":
		return nil
	default:
		return fmt.Errorf("baseline %s は zfs backend 専用です(backend=%s)。"+
			"apfs / reflink では baseline import を使ってください", what, cfg.Storage.Backend)
	}
}

// listChildDatasets は親の直下のデータセット名を返す(親自身は含めない)。
func listChildDatasets(parent string) ([]string, error) {
	out, err := exec.Command("zfs", "list", "-H", "-o", "name", "-t", "filesystem", "-r", parent).Output()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name := strings.TrimSpace(line)
		if name == "" || name == parent {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

// latestSnapshot はデータセットの最新 snapshot(作成順)を返す。
func latestSnapshot(dataset string) (string, error) {
	out, err := exec.Command("zfs", "list", "-H", "-o", "name", "-t", "snapshot",
		"-s", "creation", "-r", dataset).Output()
	if err != nil {
		return "", fmt.Errorf("受信した snapshot を確認できません: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		s := strings.TrimSpace(lines[i])
		// 子データセットの snapshot は拾わない(base 自身のもののみ)。
		if strings.HasPrefix(s, dataset+"@") {
			return s, nil
		}
	}
	return "", fmt.Errorf("%s に snapshot が見つかりません(ストリームが空?)", dataset)
}
