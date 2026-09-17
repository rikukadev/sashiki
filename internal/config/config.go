// Package config は /etc/sashiki/config.yaml の読み込み(仕様 12-1 の v0.1 サブセット)。
// シークレットは設定ファイルに書かない。API トークンは環境変数から読む。
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// Config は sashikid 全体の設定。
type Config struct {
	Listen    Listen   `yaml:"listen"`
	Domain    string   `yaml:"domain"`
	StateDB   string   `yaml:"state_db"`
	RunDir    string   `yaml:"run_dir"`    // 実行時の一時領域(socket / sentinel)。既定 /run/sashiki(仕様 21章)
	LogDir    string   `yaml:"log_dir"`    // ログ出力の基点。既定 /var/log/sashiki(仕様 21章)
	LogFormat string   `yaml:"log_format"` // text | json(構造化ログ)
	Storage   Storage  `yaml:"storage"`
	Engine    Engine   `yaml:"engine"`
	Proxy     Proxy    `yaml:"proxy"`
	Branches  Branches `yaml:"branches"`
	Baseline  Baseline `yaml:"baseline"`
	Hooks     Hooks    `yaml:"hooks"`
	Auth      Auth     `yaml:"auth"`
}

// Listen は各リスナーのアドレス。
type Listen struct {
	API     string `yaml:"api"`
	Proxy   string `yaml:"proxy"`
	Metrics string `yaml:"metrics"`
}

// Storage はバックエンド設定。
type Storage struct {
	Backend             string       `yaml:"backend"` // ebs-zfs | fsx-zfs | apfs | reflink
	HighWatermark       float64      `yaml:"high_watermark"`
	CriticalWatermark   float64      `yaml:"critical_watermark"`
	DefaultStorageQuota string       `yaml:"default_storage_quota"` // branch ごとの refquota(例 "10G"。空=無制限, #85)
	Zfs                 ZfsStorage   `yaml:"ebs-zfs"`
	Fsx                 FsxStorage   `yaml:"fsx-zfs"`
	Local               LocalStorage `yaml:"local"` // apfs / reflink(ローカル CoW、#113)

	// 旧キー(v0.1 互換)。Load で新フィールドへ移す。
	LegacyZfs *ZfsStorage `yaml:"zfs"`
	LegacyFsx *FsxStorage `yaml:"fsx"`
}

// LocalStorage は apfs(macOS clonefile)/ reflink(Linux cp --reflink)の設定。
type LocalStorage struct {
	Root             string `yaml:"root"`              // CoW 対応 FS 上のルート(APFS / XFS reflink)
	BaselineSnapshot string `yaml:"baseline_snapshot"` // 既定 "baseline"
}

// FsxStorage は fsx バックエンドの設定。
type FsxStorage struct {
	Region           string `yaml:"region"`
	FilesystemID     string `yaml:"filesystem_id"`
	BaseVolumeID     string `yaml:"base_volume_id"`
	ParentVolumeID   string `yaml:"parent_volume_id"`
	BaselineSnapshot string `yaml:"baseline_snapshot"`
	DNSName          string `yaml:"dns_name"`
	MountRoot        string `yaml:"mount_root"`
}

// ZfsStorage は zfs バックエンドの設定。
type ZfsStorage struct {
	Pool             string `yaml:"pool"`
	BaseDataset      string `yaml:"base_dataset"`
	BranchParent     string `yaml:"branch_parent"`
	BaselineSnapshot string `yaml:"baseline_snapshot"`
	Sudo             bool   `yaml:"sudo"`
}

// Engine はエンジン設定。
type Engine struct {
	Type     string         `yaml:"type"` // mysql | postgres
	Mysql    MysqlEngine    `yaml:"mysql"`
	Postgres PostgresEngine `yaml:"postgres"`
}

// PostgresEngine は postgres エンジンの設定。
// 注意: プロトコルプロキシは現状 MySQL 専用のため、postgres ブランチへの接続は
// 直接ポート(sashiki show <name>)になる(#222 で pgproxy 予定)。リモート接続する
// 場合は listen_addresses を広げ、base の pg_hba.conf に host 行を入れておくこと。
type PostgresEngine struct {
	PortRange       [2]int `yaml:"port_range"` // 既定 [5433, 5632]
	BinDir          string `yaml:"bin_dir"`    // 既定 /usr/lib/postgresql/16/bin
	EnvDir          string `yaml:"env_dir"`
	ListenAddresses string `yaml:"listen_addresses"` // 既定 127.0.0.1
	Sudo            bool   `yaml:"sudo"`
	// AppUser / AppPass はブランチへ接続するアプリ用ロール(mysql の app_user 相当)。
	// baseline import / provisioning がこの名前でロールを作り、将来 pgproxy が
	// `<app_user>@<branch>` のルーティングとパスワード検証に使う(#222/#223/#224)。
	AppUser string `yaml:"app_user"`
	AppPass string `yaml:"app_pass"`
	// Mode は起動方式。"systemd"(既定)か "process"(systemd の無い環境で postgres を
	// 直接 spawn、#227)。
	Mode string `yaml:"mode"`
	// RunUser は root で sashikid を動かすときに降格する OS ユーザー。postgres は
	// root では起動を拒むため既定 "postgres"。
	RunUser string `yaml:"run_user"`
	// SharedBuffers は mysql の buffer_pool_size 相当。メモリ admission の見積もりと
	// 起動パラメータに使う。
	SharedBuffers string `yaml:"shared_buffers"`
	// ExpectedRSS / MemoryHeadroom / MaxRunning は mysql と同じ意味のメモリ admission。
	ExpectedRSS    string `yaml:"expected_rss"`
	MemoryHeadroom string `yaml:"memory_headroom"`
	MaxRunning     int    `yaml:"max_running"`
	// InitdbArgs は baseline 構築時の initdb 追加引数(locale / encoding / checksums 等、#223)。
	InitdbArgs []string `yaml:"initdb_args"`
}

// MysqlEngine は mysql エンジンの設定。
type MysqlEngine struct {
	PortRange      [2]int `yaml:"port_range"`
	BufferPoolSize string `yaml:"buffer_pool_size"`
	ExpectedRSS    string `yaml:"expected_rss"`
	MemoryHeadroom string `yaml:"memory_headroom"`
	MaxRunning     int    `yaml:"max_running"`
	// app_user / app_pass は方式A(#51)の app credential(仕様 21章の新名)。
	// 旧名 proxy_user / proxy_pass は normalize で alias 維持する。
	AppUser   string `yaml:"app_user"`
	AppPass   string `yaml:"app_pass"`
	ProxyUser string `yaml:"proxy_user"`
	ProxyPass string `yaml:"proxy_pass"`
	EnvDir    string `yaml:"env_dir"`
	Sudo      bool   `yaml:"sudo"`
	// Mode は起動方式。"systemd"(既定)か "process"(systemd の無い macOS ネイティブ /
	// コンテナで mysqld を直接 spawn、#113)。
	Mode      string `yaml:"mode"`
	MysqldBin string `yaml:"mysqld_bin"` // process モードの mysqld パス(既定 "mysqld")
	RunUser   string `yaml:"run_user"`   // root 起動時に mysqld へ渡す --user(既定 "mysql")
	// ExtraCnf はプロジェクト固有の my.cnf。process モードで
	// --defaults-extra-file として読み込む(datadir/port 等は sashiki が上書き)。
	ExtraCnf string `yaml:"extra_cnf"`
}

// Proxy はプロトコルプロキシの設定。
type Proxy struct {
	MaxConnPerBranch int    `yaml:"max_conn_per_branch"`
	TLSCert          string `yaml:"tls_cert"` // 空なら TLS 終端しない(平文)。方式A #51
	TLSKey           string `yaml:"tls_key"`
	// AllowedUser は `<user>@<branch>` の user 部の許可。未設定(nil)なら
	// app_user のみ許可(既定)。空文字 "" を明示すると任意のユーザー名を許可する
	// (管理ユーザーで接続したい場合など、#131)。特定名を入れればその名前だけ許可。
	AllowedUser *string `yaml:"allowed_user"`
}

// Branches はブランチのポリシー。
type Branches struct {
	NamePattern        string        `yaml:"name_pattern"`
	MaxBranches        int           `yaml:"max_branches"`
	LazyCreate         bool          `yaml:"lazy_create"`
	LazyCreateMaxWait  time.Duration `yaml:"lazy_create_max_wait"`
	IdleStopAfter      time.Duration `yaml:"idle_stop_after"`
	DeleteAfterIdle    time.Duration `yaml:"delete_after_idle"`
	ReaperInterval     time.Duration `yaml:"reaper_interval"`
	OperationRetention time.Duration `yaml:"operation_retention"` // 完了 operation の保持期間(#83)。0=無期限
	// ErrorRetention: error 状態になってからこの期間が過ぎたブランチを reaper が削除する
	// (#298)。調べる時間を残しつつ、max_branches と port を占有し続けないように。
	// 0 = 削除しない(従来の挙動)。lease(--ttl)はこれと無関係に error でも効く。
	ErrorRetention time.Duration `yaml:"error_retention"`

	// profile: 用途ごとに idle lifecycle を変える(仕様 11-3)。
	// branch は create 時に profile を1つ持ち、reaper はその profile の
	// idle_stop_after / delete_after_idle を使う。未設定フィールドは上の
	// global 値にフォールバックする。lease(expires_at)は profile ではなく
	// create --ttl / lease renew で明示的に付ける絶対期限(仕様 13-6)。
	Profiles       map[string]ProfilePolicy `yaml:"profiles"`
	DefaultProfile string                   `yaml:"default_profile"`
}

// ProfilePolicy は profile ごとの idle lifecycle。0 のフィールドは global にフォールバック。
type ProfilePolicy struct {
	IdleStopAfter   time.Duration `yaml:"idle_stop_after"`
	DeleteAfterIdle time.Duration `yaml:"delete_after_idle"`
}

// Baseline は baseline publish のポリシー(仕様 12-3/12-4)。
type Baseline struct {
	// refresh の実行方法(#101): refresh_script があればそれを実行。無ければ
	// source_dir/*.sql を組み込みローダーが冪等適用する(mysql エンジンのみ)。
	RefreshScript    string        `yaml:"refresh_script"`  // 既定 /etc/sashiki/refresh.sh
	RefreshTimeout   time.Duration `yaml:"refresh_timeout"` // 既定 1h
	SourceDir        string        `yaml:"source_dir"`      // 例 /etc/sashiki/baseline-src
	SourceDB         string        `yaml:"source_db"`       // ローダー実行時に選択する DB(任意)
	RequireMasked    bool          `yaml:"require_masked"`
	RequireValidated bool          `yaml:"require_validated"`
	ValidatePort     int           `yaml:"validate_port"`   // validate 用の一時ポート(既定 3999)
	MaskedSentinel   string        `yaml:"masked_sentinel"` // build script が touch する印(既定 /run/sashiki/baseline-masked)
	KeepLast         int           `yaml:"keep_last"`       // GC で残す直近 N(既定 3、#86)
	Retention        time.Duration `yaml:"retention"`       // GC で残す期間(0=無期限、#86)
}

// Hooks はフック設定。
type Hooks struct {
	Dir     string        `yaml:"dir"`
	LogDir  string        `yaml:"log_dir"`
	Timeout time.Duration `yaml:"timeout"`
}

// Auth は認証設定。
type Auth struct {
	APITokenEnv string `yaml:"api_token_env"`
	// APITokenSSM を設定すると、起動時に SSM Parameter Store(SecureString)から
	// API トークンを読み、環境変数より優先する(仕様 21章、リモート運用向け)。
	APITokenSSM string `yaml:"api_token_ssm"`
	// TrustLoopback が true(既定)なら loopback からのリクエストを無認証で通す。
	// リバースプロキシ越しに公開すると接続元が 127.0.0.1 に見えて素通しになるため、
	// 外部公開時は false にして loopback でも Bearer トークンを必須にする(#198)。
	TrustLoopback bool `yaml:"trust_loopback"`
}

// Default は既定値。
func Default() Config {
	return Config{
		Listen:    Listen{API: "127.0.0.1:8080", Proxy: "0.0.0.0:3306", Metrics: "127.0.0.1:9100"},
		Domain:    "sashiki.internal",
		StateDB:   "/var/lib/sashiki/state.db",
		RunDir:    "/run/sashiki",
		LogDir:    "/var/log/sashiki",
		LogFormat: "text",
		Storage: Storage{
			Backend: "ebs-zfs",
			Zfs: ZfsStorage{
				Pool:             "dbpool",
				BaseDataset:      "dbpool/base",
				BranchParent:     "dbpool/branches",
				BaselineSnapshot: "baseline",
				Sudo:             true,
			},
		},
		Engine: Engine{
			Type: "mysql",
			Mysql: MysqlEngine{
				PortRange:      [2]int{3401, 3600},
				BufferPoolSize: "256M",
				ProxyUser:      "dev",
				ProxyPass:      "dev",
				EnvDir:         "/etc/sashiki",
				Sudo:           true,
			},
			Postgres: PostgresEngine{
				PortRange:       [2]int{5433, 5632},
				EnvDir:          "/etc/sashiki",
				ListenAddresses: "127.0.0.1",
				Sudo:            true,
				AppUser:         "dev",
				AppPass:         "dev",
				RunUser:         "postgres",
				SharedBuffers:   "128M",
			},
		},
		Proxy: Proxy{MaxConnPerBranch: 50},
		Branches: Branches{
			NamePattern:        `^[a-z0-9-]{1,32}$`,
			MaxBranches:        50,
			LazyCreate:         true,
			LazyCreateMaxWait:  20 * time.Second,
			IdleStopAfter:      30 * time.Minute,
			DeleteAfterIdle:    168 * time.Hour,
			ReaperInterval:     time.Minute,
			OperationRetention: 168 * time.Hour, // 7日
			ErrorRetention:     72 * time.Hour,  // 3日(#298)

			Profiles: map[string]ProfilePolicy{
				"preview": {IdleStopAfter: 30 * time.Minute, DeleteAfterIdle: 168 * time.Hour},
				"ci":      {IdleStopAfter: 5 * time.Minute, DeleteAfterIdle: time.Hour},
				"sandbox": {IdleStopAfter: time.Hour, DeleteAfterIdle: 720 * time.Hour},
			},
			DefaultProfile: "preview",
		},
		// MaskedSentinel / Hooks.LogDir は空のままにし、normalize で run_dir /
		// log_dir から派生させる(仕様 21章)。
		Baseline: Baseline{ValidatePort: 3999, KeepLast: 3},
		Hooks: Hooks{
			Dir: "/etc/sashiki/hooks",
		},
		Auth: Auth{APITokenEnv: "SASHIKI_API_TOKEN", TrustLoopback: true},
	}
}

// Load はファイルから読み込み、既定値の上に重ねて検証する。
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// normalize は旧名(zfs / fsx)を新名(ebs-zfs / fsx-zfs)へ移す。
func (c *Config) normalize() {
	switch c.Storage.Backend {
	case "zfs":
		c.Storage.Backend = "ebs-zfs"
	case "fsx":
		c.Storage.Backend = "fsx-zfs"
	}
	if c.Storage.LegacyZfs != nil {
		c.Storage.Zfs = *c.Storage.LegacyZfs
		c.Storage.LegacyZfs = nil
	}
	if c.Storage.LegacyFsx != nil {
		c.Storage.Fsx = *c.Storage.LegacyFsx
		c.Storage.LegacyFsx = nil
	}
	// app_user / app_pass(新名)が指定されていれば proxy_user / proxy_pass
	// (旧名。下流はこちらを読む)へ反映する。両方省略時は Default の proxy_* が残る。
	if c.Engine.Mysql.AppUser != "" {
		c.Engine.Mysql.ProxyUser = c.Engine.Mysql.AppUser
	}
	if c.Engine.Mysql.AppPass != "" {
		c.Engine.Mysql.ProxyPass = c.Engine.Mysql.AppPass
	}
	// run_dir / log_dir が空なら既定へ戻す(明示的に空指定された場合の保険)。
	if c.RunDir == "" {
		c.RunDir = "/run/sashiki"
	}
	if c.LogDir == "" {
		c.LogDir = "/var/log/sashiki"
	}
	// hooks.log_dir / baseline.masked_sentinel は未設定なら run_dir / log_dir から派生。
	if c.Hooks.LogDir == "" {
		c.Hooks.LogDir = c.LogDir + "/hooks"
	}
	if c.Baseline.MaskedSentinel == "" {
		c.Baseline.MaskedSentinel = c.RunDir + "/baseline-masked"
	}
}

// Validate は設定の整合性チェック。
func (c Config) Validate() error {
	switch c.Storage.Backend {
	case "ebs-zfs", "fsx-zfs", "apfs", "reflink":
	default:
		return fmt.Errorf("storage.backend %q is not supported (ebs-zfs | fsx-zfs | apfs | reflink)", c.Storage.Backend)
	}
	if c.Storage.Backend == "fsx-zfs" {
		f := c.Storage.Fsx
		// parent_volume_id は省略可(filesystem のルートボリュームを自動発見)
		if f.Region == "" || f.FilesystemID == "" || f.BaseVolumeID == "" || f.DNSName == "" {
			return fmt.Errorf("storage.fsx-zfs requires region, filesystem_id, base_volume_id, dns_name")
		}
	}
	if c.Storage.Backend == "apfs" || c.Storage.Backend == "reflink" {
		if c.Storage.Local.Root == "" {
			return fmt.Errorf("storage.local.root is required for backend %q", c.Storage.Backend)
		}
	}
	if c.LogFormat != "text" && c.LogFormat != "json" {
		return fmt.Errorf("log_format %q is not supported (text | json)", c.LogFormat)
	}
	if c.Engine.Type != "mysql" && c.Engine.Type != "postgres" {
		return fmt.Errorf("engine.type %q is not supported (mysql | postgres)", c.Engine.Type)
	}
	if _, err := regexp.Compile(c.Branches.NamePattern); err != nil {
		return fmt.Errorf("branches.name_pattern: %w", err)
	}
	if len(c.Branches.Profiles) > 0 && c.Branches.DefaultProfile != "" {
		if _, ok := c.Branches.Profiles[c.Branches.DefaultProfile]; !ok {
			return fmt.Errorf("branches.default_profile %q is not defined in branches.profiles", c.Branches.DefaultProfile)
		}
	}
	if c.Engine.Mysql.PortRange[0] <= 0 || c.Engine.Mysql.PortRange[1] < c.Engine.Mysql.PortRange[0] {
		return fmt.Errorf("engine.mysql.port_range must be [low, high]")
	}
	if c.Engine.Type == "postgres" {
		if c.Engine.Postgres.PortRange[0] <= 0 || c.Engine.Postgres.PortRange[1] < c.Engine.Postgres.PortRange[0] {
			return fmt.Errorf("engine.postgres.port_range must be [low, high]")
		}
	}
	// mode は未設定(= systemd)を許しつつ、綴り間違いは弾く(#225)。
	if m := c.Engine.Mysql.Mode; m != "" && m != "systemd" && m != "process" {
		return fmt.Errorf("engine.mysql.mode %q is not supported (systemd | process)", m)
	}
	if m := c.Engine.Postgres.Mode; m != "" && m != "systemd" && m != "process" {
		return fmt.Errorf("engine.postgres.mode %q is not supported (systemd | process)", m)
	}
	return nil
}

// PortRange は選択中エンジンのポートレンジを返す。
func (c Config) PortRange() [2]int {
	if c.Engine.Type == "postgres" {
		return c.Engine.Postgres.PortRange
	}
	return c.Engine.Mysql.PortRange
}

// 以降は「選択中エンジンの設定」を engine 非依存に取り出すアクセサ(#225)。
// 呼び出し側(sashikid の admission 配線など)が Engine.Mysql.* を直接読むと
// postgres で常に既定値/ゼロになってしまうため、ここで一段挟む。

// AppUser はブランチへ接続するアプリ用ユーザー/ロール名を返す。
func (c Config) AppUser() string {
	if c.Engine.Type == "postgres" {
		return c.Engine.Postgres.AppUser
	}
	return c.Engine.Mysql.ProxyUser // normalize で app_user が反映済み
}

// AppPass は AppUser のパスワードを返す。
func (c Config) AppPass() string {
	if c.Engine.Type == "postgres" {
		return c.Engine.Postgres.AppPass
	}
	return c.Engine.Mysql.ProxyPass
}

// EngineMode は起動方式("systemd" か "process")を返す。未設定は "systemd"。
func (c Config) EngineMode() string {
	m := c.Engine.Mysql.Mode
	if c.Engine.Type == "postgres" {
		m = c.Engine.Postgres.Mode
	}
	if m == "" {
		return "systemd"
	}
	return m
}

// EngineEnvDir は per-branch の env ファイルを書くディレクトリを返す。
func (c Config) EngineEnvDir() string {
	if c.Engine.Type == "postgres" {
		return c.Engine.Postgres.EnvDir
	}
	return c.Engine.Mysql.EnvDir
}

// EngineRunUser は root 起動時に降格する OS ユーザーを返す。
func (c Config) EngineRunUser() string {
	if c.Engine.Type == "postgres" {
		return c.Engine.Postgres.RunUser
	}
	return c.Engine.Mysql.RunUser
}

// ExpectedRSS は 1 インスタンスあたりの想定 RSS(メモリ admission 用)を返す。
func (c Config) ExpectedRSS() string {
	if c.Engine.Type == "postgres" {
		return c.Engine.Postgres.ExpectedRSS
	}
	return c.Engine.Mysql.ExpectedRSS
}

// MemoryHeadroom は空けておくメモリ量を返す。
func (c Config) MemoryHeadroom() string {
	if c.Engine.Type == "postgres" {
		return c.Engine.Postgres.MemoryHeadroom
	}
	return c.Engine.Mysql.MemoryHeadroom
}

// MemoryBaselineSize はインスタンスが確保する主バッファのサイズを返す
// (mysql は buffer_pool_size、postgres は shared_buffers)。
func (c Config) MemoryBaselineSize() string {
	if c.Engine.Type == "postgres" {
		return c.Engine.Postgres.SharedBuffers
	}
	return c.Engine.Mysql.BufferPoolSize
}

// MaxRunning は同時に起動してよいインスタンス数の上限(0 は無制限)を返す。
func (c Config) MaxRunning() int {
	if c.Engine.Type == "postgres" {
		return c.Engine.Postgres.MaxRunning
	}
	return c.Engine.Mysql.MaxRunning
}
