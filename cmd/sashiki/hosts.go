// 名前付きホスト(#386)。~/.config/sashiki/hosts.yaml に複数の sashikid を
// 登録し、--host / SASHIKI_HOST で切り替える。list / capacity は --all-hosts で
// 全ホストへ fan-out できる。
//
// これは**純粋に CLI だけの機能**で、daemon 側は何も知らない(連合はしない、
// ADR-013)。環境変数 2 つ(URL / トークン)を差し替えて回る運用は、間違えると
// 別のホストに向かって操作する(delete の誤爆が最悪ケース)ため、名前で選ぶ
// 口を用意する。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// hostEntry は 1 ホスト分の接続情報。token は平文を書かない口を 3 つ用意する
// (config の app_pass と同じ思想、#354)。優先順位は token_file > token_env > token。
type hostEntry struct {
	URL       string `yaml:"url"`
	Token     string `yaml:"token"`
	TokenEnv  string `yaml:"token_env"`
	TokenFile string `yaml:"token_file"`
}

type hostsFile struct {
	Default string               `yaml:"default"`
	Hosts   map[string]hostEntry `yaml:"hosts"`
}

// selectedHost は --host / SASHIKI_HOST / hosts.yaml の default で解決した
// 接続先。run() の冒頭で 1 回だけ設定され、以降 apiURL() / apiToken() が参照する。
//
// explicit は「名前で明示的に選んだ」印。優先順位の判定は設定時ではなく
// **参照時(apiURL / apiToken)に行う** — 設定時に環境変数を見て分岐すると、
// その後に環境が変わったとき(テストの t.Setenv が典型)に、hosts.yaml の
// default が環境変数より強く残ってしまう。
var selectedHost struct {
	url      string
	token    string
	explicit bool
}

func hostsFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "sashiki", "hosts.yaml")
}

// loadHostsFrom は hosts.yaml を読む。無いのはエラーではない(nil を返す)。
// あるのに読めない・壊れているのはエラー — 黙って単一ホストの挙動に落ちると、
// 意図と違うホストへ操作が飛ぶ。
func loadHostsFrom(path string) (*hostsFile, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("hosts.yaml: %w", err)
	}
	var hf hostsFile
	if err := yaml.Unmarshal(b, &hf); err != nil {
		return nil, fmt.Errorf("hosts.yaml: %w", err)
	}
	for name, h := range hf.Hosts {
		if h.URL == "" {
			return nil, fmt.Errorf("hosts.yaml: hosts.%s に url がありません", name)
		}
	}
	if hf.Default != "" {
		if _, ok := hf.Hosts[hf.Default]; !ok {
			return nil, fmt.Errorf("hosts.yaml: default %q が hosts にありません", hf.Default)
		}
	}
	return &hf, nil
}

// resolveToken は hostEntry からトークンを取り出す。無ければ空(loopback の
// 無認証や、従来の SASHIKI_API_TOKEN / token ファイルのフォールバックに任せる)。
func (h hostEntry) resolveToken() (string, error) {
	if h.TokenFile != "" {
		p := h.TokenFile
		if strings.HasPrefix(p, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			p = filepath.Join(home, p[2:])
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("token_file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	if h.TokenEnv != "" {
		v := os.Getenv(h.TokenEnv)
		if v == "" {
			// 参照先が空なら平文へは落ちない(config の app_pass と同じ判断)。
			// 指した先が壊れているのに黙って別の資格情報で繋がると原因が
			// 分からなくなる。
			return "", fmt.Errorf("token_env %s が空です", h.TokenEnv)
		}
		return v, nil
	}
	return h.Token, nil
}

// selectHost は名前で接続先を選ぶ(--host / SASHIKI_HOST)。
func selectHost(name string) error {
	hf, err := loadHostsFrom(hostsFilePath())
	if err != nil {
		return err
	}
	if hf == nil {
		return fmt.Errorf("--host %s: %s がありません", name, hostsFilePath())
	}
	h, ok := hf.Hosts[name]
	if !ok {
		known := make([]string, 0, len(hf.Hosts))
		for k := range hf.Hosts {
			known = append(known, k)
		}
		sort.Strings(known)
		return fmt.Errorf("--host %s: hosts.yaml にありません(登録済み: %s)", name, strings.Join(known, ", "))
	}
	tok, err := h.resolveToken()
	if err != nil {
		return fmt.Errorf("host %s: %w", name, err)
	}
	selectedHost.url = h.URL
	selectedHost.token = tok
	selectedHost.explicit = true
	return nil
}

// selectDefaultHost は hosts.yaml の default を候補として積む。explicit には
// しない — SASHIKI_API_URL より弱い優先度で apiURL() が参照する。hosts.yaml を
// 後から置いても、既存のスクリプト(環境変数運用)の接続先は変わらない。
func selectDefaultHost() error {
	hf, err := loadHostsFrom(hostsFilePath())
	if err != nil {
		return err
	}
	if hf == nil || hf.Default == "" {
		return nil
	}
	h := hf.Hosts[hf.Default]
	tok, terr := h.resolveToken()
	if terr != nil {
		return fmt.Errorf("host %s: %w", hf.Default, terr)
	}
	selectedHost.url = h.URL
	selectedHost.token = tok
	selectedHost.explicit = false
	return nil
}

// extractHostFlag は args から --host <name> を取り除いて返す。全コマンド共通の
// グローバルフラグで、各コマンドの parseArgs(未知フラグをエラーにする)より
// 先に処理する。
func extractHostFlag(args []string) (rest []string, host string, err error) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--host" {
			if i+1 >= len(args) {
				return nil, "", fmt.Errorf("--host requires a value")
			}
			i++
			host = args[i]
			continue
		}
		rest = append(rest, args[i])
	}
	return rest, host, nil
}

// hostResult は fan-out の 1 ホスト分の結果。
type hostResult struct {
	Name string // hosts.yaml のキー
	Code int
	Data []byte
	Err  error
}

// fanOut は hosts.yaml の全ホストへ同じ GET を投げる(#386)。読み取り専用の
// コマンド(list / capacity)だけが使う。**変更操作には使わない** — 一括削除の
// ような口を作らないため。
func fanOut(path string) ([]hostResult, error) {
	hf, err := loadHostsFrom(hostsFilePath())
	if err != nil {
		return nil, err
	}
	if hf == nil || len(hf.Hosts) == 0 {
		return nil, fmt.Errorf("--all-hosts: %s にホストが登録されていません", hostsFilePath())
	}
	names := make([]string, 0, len(hf.Hosts))
	for k := range hf.Hosts {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]hostResult, 0, len(names))
	for _, name := range names {
		h := hf.Hosts[name]
		tok, terr := h.resolveToken()
		if terr != nil {
			out = append(out, hostResult{Name: name, Err: terr})
			continue
		}
		code, data, cerr := callWith(h.URL, tok, "GET", path, nil)
		out = append(out, hostResult{Name: name, Code: code, Data: data, Err: cerr})
	}
	return out, nil
}

// reportPartialFailure は届かなかったホストを warning で出す。部分結果を黙って
// 全体として見せない(#386)。全滅なら true(呼び出し側は exitError)。
func reportPartialFailure(results []hostResult) (allFailed bool) {
	var failed []string
	ok := 0
	for _, r := range results {
		if r.Err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", r.Name, r.Err))
		} else if r.Code != 200 {
			failed = append(failed, fmt.Sprintf("%s: %s", r.Name, apiError(r.Data)))
		} else {
			ok++
		}
	}
	if len(failed) > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d 台中 %d 台から応答:\n", len(results), ok)
		for _, f := range failed {
			fmt.Fprintf(os.Stderr, "  %s\n", f)
		}
	}
	return ok == 0
}
