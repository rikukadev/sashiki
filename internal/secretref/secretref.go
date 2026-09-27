// Package secretref は「秘密の値そのもの」ではなく「どこにあるか」を設定に
// 書けるようにする(#354)。
//
// 秘密を config.yaml に平文で置くと、設定ファイルのコピーがそのまま漏洩経路に
// なる。サポート用に貼る、バックアップを取る、ログに出す — どれも値を運ぶ。
// 実際に app_pass は SSM Run Command で config を grep しただけで、削除 API の
// 無いコマンド履歴に平文で残った。
//
// 参照は 4 通りで、**優先順位は SSM > Env > File > Literal**。
//
//	SSM      SSM Parameter Store の SecureString。値はホストのディスクに残らない
//	Env      環境変数の **名前**。値は systemd の EnvironmentFile(0600)等に置く
//	File     0600 のファイルのパス。中身が値そのもの
//	Literal  従来どおりの平文。後方互換のために残す
//
// SSM を最優先にするのは、設定を配る運用で「env を上書きされて古い値で動く」を
// 避けるため。api_token の api_token_ssm / api_token_env と同じ順序にしてある。
//
// File があるのは、**同じ秘密を systemd の中と外の両方から読む**必要があるため。
// sashikid は systemd 下なので EnvironmentFile で env を渡せるが、
// `sashiki baseline import` は root のシェルから直接動くので env が無い。
// env だけにすると import が読めなくなる。
package secretref

import (
	"context"
	"fmt"
	"os"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"
)

// Ref は秘密の在り処。4 つとも空なら未設定。
type Ref struct {
	// SSM は SSM Parameter Store のパラメータ名(SecureString)。
	SSM string
	// Env は環境変数の名前。値そのものではない。
	Env string
	// File は値が入ったファイルのパス。末尾の改行は落とす。
	File string
	// Literal は平文。
	Literal string
}

// IsZero は 1 つも設定されていないことを返す。
func (r Ref) IsZero() bool {
	return r.SSM == "" && r.Env == "" && r.File == "" && r.Literal == ""
}

// UsesSSM は SSM からの取得が要るかを返す。呼び出し側が AWS 呼び出しの有無を
// 判断できるようにする(要らない場面でネットワークに出ないため)。
func (r Ref) UsesSSM() bool { return r.SSM != "" }

// ResolveLocal は AWS を呼ばずに解決する。env / file / literal のときに使う。
// SSM が設定されている場合はエラーを返す。呼び出し側は Resolve を使う。
func (r Ref) ResolveLocal(what string) (string, error) {
	if r.SSM != "" {
		return "", fmt.Errorf("%s: %s は SSM からの取得が必要です", what, r.SSM)
	}
	if r.Env != "" {
		v := os.Getenv(r.Env)
		if v == "" {
			// 名前は出すが値は出さない。名前が出ないと「どの変数を設定すれば
			// よいか」が分からず、結局 config を覗くことになる
			return "", fmt.Errorf("%s: 環境変数 %s が空です", what, r.Env)
		}
		return v, nil
	}
	if r.File != "" {
		b, err := os.ReadFile(r.File)
		if err != nil {
			return "", fmt.Errorf("%s: %w", what, err)
		}
		// 末尾の改行はエディタや echo が勝手に付ける。値に含めると
		// 認証が通らず、原因が見えない(値を出して比べられないため)
		v := strings.TrimRight(string(b), "\r\n")
		if v == "" {
			return "", fmt.Errorf("%s: %s が空です", what, r.File)
		}
		return v, nil
	}
	if r.Literal != "" {
		return r.Literal, nil
	}
	return "", Unset(what)
}

// Resolve は SSM も含めて解決する。
func (r Ref) Resolve(ctx context.Context, what string) (string, error) {
	if r.SSM == "" {
		return r.ResolveLocal(what)
	}
	v, err := getParameter(ctx, r.SSM)
	if err != nil {
		return "", fmt.Errorf("%s: SSM パラメータ %s: %w", what, r.SSM, err)
	}
	if v == "" {
		return "", fmt.Errorf("%s: SSM パラメータ %s が空です", what, r.SSM)
	}
	return v, nil
}

// Unset は 4 つとも未設定のときのエラー。**選択肢を並べて返す**。
// 「app_pass が未設定です」だけだと、平文で書くしかないと読めてしまう。
func Unset(what string) error {
	return fmt.Errorf(`%[1]s が設定されていません。次のどれかを config.yaml に書いてください
  %[1]s_ssm: /path/to/param   SSM Parameter Store の SecureString(値をディスクに残さない)
  %[1]s_env: ENV_NAME         環境変数の名前(値は EnvironmentFile 等に置く)
  %[1]s_file: /path/to/file   0600 のファイル。中身が値そのもの(systemd の外からも読める)
  %[1]s: <value>              平文(後方互換。設定ファイルのコピーが漏洩経路になる)`, what)
}

// getParameter は SecureString を復号して取る。差し替え可能にしてあるのは
// テストで AWS を呼ばないため。
var getParameter = func(ctx context.Context, name string) (string, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return "", fmt.Errorf("aws config: %w", err)
	}
	decrypt := true
	out, err := awsssm.NewFromConfig(awsCfg).GetParameter(ctx, &awsssm.GetParameterInput{
		Name:           &name,
		WithDecryption: &decrypt,
	})
	if err != nil {
		return "", err
	}
	if out.Parameter == nil || out.Parameter.Value == nil {
		return "", fmt.Errorf("parameter has no value")
	}
	return *out.Parameter.Value, nil
}
