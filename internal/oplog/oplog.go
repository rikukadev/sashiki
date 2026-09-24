// Package oplog は operation の相関情報(operation_id / operation_type / branch)を
// context に載せ、途中のログにも同じ属性を付けるための小さな橋渡し(仕様 20-5、#284)。
//
// ops.Runner が operation を起動するときに WithOperation で ctx に載せ、
// workspace / hooks などの途中ログは Logf(ctx, ...) を使う。operation の外
// (reaper / reconcile / connpoll)は WithBranch で branch だけを付ける。該当しない
// 属性は無理に付けない(空文字の operation_id を出さない)。
//
// 出力は slog.Default() に流す。sashikid は log_format: json のとき JSON handler を
// 既定にしているので、そのまま構造化される。text のときは slog の既定 handler
// (log パッケージ経由の "INFO msg key=value")になる。
package oplog

import (
	"context"
	"fmt"
	"log/slog"
)

// Operation は ctx に載せる相関情報。ID / Type / Branch は空なら出力しない。
// Extra は追加の属性(key, value の交互)で、baseline refresh の tag などに使う。
type Operation struct {
	ID     string
	Type   string
	Branch string
	Extra  []any
}

type ctxKey struct{}

// WithOperation は ctx に operation の相関情報を載せる。
func WithOperation(ctx context.Context, op Operation) context.Context {
	return context.WithValue(ctx, ctxKey{}, op)
}

// WithBranch は operation の外(reaper 等)で branch だけを載せる。既に operation が
// 載っていればその情報を保ちつつ branch を差し替える。
func WithBranch(ctx context.Context, branch string) context.Context {
	op, _ := FromContext(ctx)
	op.Branch = branch
	return WithOperation(ctx, op)
}

// FromContext は ctx の相関情報を返す。
func FromContext(ctx context.Context) (Operation, bool) {
	if ctx == nil {
		return Operation{}, false
	}
	op, ok := ctx.Value(ctxKey{}).(Operation)
	return op, ok
}

// Attrs は ctx の相関情報を slog の属性列にする。無いものは含めない。
func Attrs(ctx context.Context) []any {
	op, ok := FromContext(ctx)
	if !ok {
		return nil
	}
	var attrs []any
	if op.ID != "" {
		attrs = append(attrs, "operation_id", op.ID)
	}
	if op.Type != "" {
		attrs = append(attrs, "operation_type", op.Type)
	}
	if op.Branch != "" {
		attrs = append(attrs, "branch", op.Branch)
	}
	return append(attrs, op.Extra...)
}

// Logf は Info レベルで、ctx の相関属性を付けて出す。
func Logf(ctx context.Context, format string, args ...any) {
	slog.Default().Info(fmt.Sprintf(format, args...), Attrs(ctx)...)
}

// Errorf は Error レベルで、ctx の相関属性を付けて出す。
func Errorf(ctx context.Context, format string, args ...any) {
	slog.Default().Error(fmt.Sprintf(format, args...), Attrs(ctx)...)
}
