// Package engine は DB エンジン依存の操作(起動・停止・ready 判定)を閉じ込める。
// Postgres 対応時はこのインターフェースの実装を1つ足すだけにする。
package engine

import "context"

// Instance はブランチ1つ分の DB プロセスの識別情報。
type Instance struct {
	Branch  string
	DataDir string // <volume path>/data
	Port    int
}

// Engine は DB エンジンが実装するインターフェース。
type Engine interface {
	// Start はインスタンスを起動する(非同期でよい)。
	Start(ctx context.Context, ins Instance) error
	// Stop は正常終了(graceful)させる。@init/@baseline の一貫性はこれに依存する。
	// snapshot を取得する前は必ずこちらを使う。
	Stop(ctx context.Context, ins Instance) error
	// Kill は即時停止(dirty state を捨てる場合。例: rollback 直前)。
	// snapshot 前には使わない。PoC では reset 時間の大半が graceful shutdown だった。
	Kill(ctx context.Context, ins Instance) error
	// WaitReady は接続可能になるまで待つ。
	WaitReady(ctx context.Context, ins Instance) error
	// IsRunning は起動中かどうか。
	IsRunning(ctx context.Context, ins Instance) (bool, error)
}

// ConnCounter は稼働中インスタンスの現在のクライアント接続数を返せる engine(#41)。
// proxy を通らない接続(postgres / fsx 直続)でも last_conn_at を更新できるよう、
// reaper 用のポーラがこれを type assertion で使う。未実装の engine では
// idle 回収を無効化する(接続の有無が判定できないため)。
// 返す値は「自分(ポーラ)の接続を除いた」クライアント接続数。
type ConnCounter interface {
	ConnCount(ctx context.Context, ins Instance) (int, error)
}

// ListenerExposureChecker は branch の DB listener が loopback 以外のローカル
// アドレスから到達できないかを診断する。proxy が認証を終端する構成では、直結
// ポートの外部公開は認証・接続数制限を迂回するため doctor が警告する(#288)。
// 戻り値は "branch (address)" 形式の到達可能な listener。
type ListenerExposureChecker interface {
	ExposedListeners(ctx context.Context, instances []Instance) ([]string, error)
}
