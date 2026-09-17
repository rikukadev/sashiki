// sashikid: sashiki デーモン。REST API を提供し、ブランチのライフサイクルを管理する。
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rikukadev/sashiki/internal/api"
	"github.com/rikukadev/sashiki/internal/config"
	"github.com/rikukadev/sashiki/internal/engine"
	enginemysql "github.com/rikukadev/sashiki/internal/engine/mysql"
	enginepostgres "github.com/rikukadev/sashiki/internal/engine/postgres"
	"github.com/rikukadev/sashiki/internal/hooks"
	"github.com/rikukadev/sashiki/internal/ops"
	"github.com/rikukadev/sashiki/internal/pgproxy"
	"github.com/rikukadev/sashiki/internal/proxy"
	"github.com/rikukadev/sashiki/internal/state"
	"github.com/rikukadev/sashiki/internal/storage"
	storageebszfs "github.com/rikukadev/sashiki/internal/storage/ebszfs"
	storagefsxzfs "github.com/rikukadev/sashiki/internal/storage/fsxzfs"
	"github.com/rikukadev/sashiki/internal/workspace"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsfsxsdk "github.com/aws/aws-sdk-go-v2/service/fsx"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"
)

// resolveAPIToken は API Bearer トークンを解決する。auth.api_token_ssm が
// 設定されていれば SSM Parameter Store(SecureString)から読み、env より
// 優先する(リモート運用向け、仕様 21章)。SSM 取得に失敗したら致命的に扱う
// (トークン未設定で無防備に起動するのを防ぐ)。
func resolveAPIToken(cfg config.Config) string {
	if name := cfg.Auth.APITokenSSM; name != "" {
		awsCfg, err := awsconfig.LoadDefaultConfig(context.Background())
		if err != nil {
			log.Fatalf("auth: aws config for api_token_ssm: %v", err)
		}
		out, err := awsssm.NewFromConfig(awsCfg).GetParameter(context.Background(), &awsssm.GetParameterInput{
			Name:           &name,
			WithDecryption: boolPtr(true),
		})
		if err != nil {
			log.Fatalf("auth: get api_token_ssm %q: %v", name, err)
		}
		if out.Parameter == nil || out.Parameter.Value == nil || *out.Parameter.Value == "" {
			log.Fatalf("auth: api_token_ssm %q is empty", name)
		}
		return *out.Parameter.Value
	}
	return os.Getenv(cfg.Auth.APITokenEnv)
}

func boolPtr(b bool) *bool { return &b }

var version = "dev" // -ldflags で埋め込む

func main() {
	configPath := flag.String("config", "/etc/sashiki/config.yaml", "path to config.yaml")
	flag.Parse()
	log.Printf("sashikid %s", version)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// 構造化ログ(仕様 20-5)。json では既存の log.Printf も slog 経由で JSON になる
	if cfg.LogFormat == "json" {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
		log.SetFlags(0)
		log.SetOutput(slogWriter{})
	}

	db, err := state.Open(cfg.StateDB)
	if err != nil {
		log.Fatalf("state db: %v", err)
	}
	defer func() { _ = db.Close() }()

	var st storage.Storage
	var bp workspace.BaselineProvider
	switch cfg.Storage.Backend {
	case "fsx-zfs":
		awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
			awsconfig.WithRegion(cfg.Storage.Fsx.Region))
		if err != nil {
			log.Fatalf("aws config: %v", err)
		}
		fbe := storagefsxzfs.New(storagefsxzfs.Config{
			FileSystemID:     cfg.Storage.Fsx.FilesystemID,
			BaseVolumeID:     cfg.Storage.Fsx.BaseVolumeID,
			ParentVolumeID:   cfg.Storage.Fsx.ParentVolumeID,
			BaselineSnapshot: cfg.Storage.Fsx.BaselineSnapshot,
			DNSName:          cfg.Storage.Fsx.DNSName,
			MountRoot:        cfg.Storage.Fsx.MountRoot,
		}, awsfsxsdk.NewFromConfig(awsCfg))
		st, bp = fbe, fbe
	case "apfs", "reflink":
		lbe, lbp, err := localBackend(cfg.Storage.Backend, cfg.Storage.Local.Root, cfg.Storage.Local.BaselineSnapshot)
		if err != nil {
			log.Fatalf("storage: %v", err)
		}
		st, bp = lbe, lbp
	default:
		zbe := storageebszfs.New(storageebszfs.Config{
			Pool:             cfg.Storage.Zfs.Pool,
			BaseDataset:      cfg.Storage.Zfs.BaseDataset,
			BranchParent:     cfg.Storage.Zfs.BranchParent,
			BaselineSnapshot: cfg.Storage.Zfs.BaselineSnapshot,
			Sudo:             cfg.Storage.Zfs.Sudo,
		})
		st, bp = zbe, zbe
	}

	// quota が設定されているのに backend が refquota 未対応なら明示ログ(#85、無言スキップ回避)。
	if cfg.Storage.DefaultStorageQuota != "" {
		if _, ok := st.(storage.Quota); !ok {
			log.Printf("sashikid: backend=%s は refquota 未対応のため default_storage_quota は適用されません", cfg.Storage.Backend)
		}
	}

	var eng engine.Engine
	switch cfg.Engine.Type {
	case "postgres":
		eng = enginepostgres.New(enginepostgres.Config{
			EnvDir:          cfg.Engine.Postgres.EnvDir,
			BinDir:          cfg.Engine.Postgres.BinDir,
			ListenAddresses: cfg.Engine.Postgres.ListenAddresses,
			Sudo:            cfg.Engine.Postgres.Sudo,
			Mode:            cfg.Engine.Postgres.Mode,
			RunUser:         cfg.Engine.Postgres.RunUser,
			SharedBuffers:   cfg.Engine.Postgres.SharedBuffers,
			LogDir:          cfg.LogDir,
		})
		// postgres の idle 回収は #41 の capability 判定(下)に一本化した。
		// postgres は ConnCounter を実装しているので connpoll が last_conn_at を
		// 更新でき、idle_stop_after / delete_after_idle / profile を無効化する必要はない。
	default:
		eng = enginemysql.New(enginemysql.Config{
			EnvDir:    cfg.Engine.Mysql.EnvDir,
			ProxyUser: cfg.Engine.Mysql.ProxyUser,
			ProxyPass: cfg.Engine.Mysql.ProxyPass,
			Sudo:      cfg.Engine.Mysql.Sudo,
			Mode:      cfg.Engine.Mysql.Mode,
			MysqldBin: cfg.Engine.Mysql.MysqldBin,
			RunUser:   cfg.Engine.Mysql.RunUser,
			ExtraCnf:  cfg.Engine.Mysql.ExtraCnf,
		})
	}

	// idle 回収は「接続の有無が分かる」ことが前提。engine が接続数を取得できる
	// (engine.ConnCounter)なら connpoll が last_conn_at を更新するので、proxy を
	// 通らない postgres / fsx 直続でも安全に回収できる(#41: 旧 postgres 無効化の解除)。
	// 取得手段が無い engine のときだけ idle_stop_after / delete_after_idle を無効化する。
	if _, ok := eng.(engine.ConnCounter); !ok {
		if cfg.Branches.IdleStopAfter > 0 || cfg.Branches.DeleteAfterIdle > 0 {
			log.Printf("sashikid: engine=%s は接続数を取得できない(ConnCounter 未実装)ため idle_stop_after / delete_after_idle を無効化します", cfg.Engine.Type)
			cfg.Branches.IdleStopAfter = 0
			cfg.Branches.DeleteAfterIdle = 0
		}
	}

	hr := hooks.NewRunner(cfg.Hooks.Dir, cfg.Hooks.LogDir, cfg.Hooks.Timeout)
	// API トークンを hook に見せない(#295)。auth.api_token_env で名前を変えて
	// いても落とす。
	hr.StripEnv = append(hr.StripEnv, cfg.Auth.APITokenEnv)

	mgr, err := workspace.New(workspace.Config{
		NamePattern:        cfg.Branches.NamePattern,
		MaxBranches:        cfg.Branches.MaxBranches,
		PortLow:            cfg.PortRange()[0],
		PortHigh:           cfg.PortRange()[1],
		EngineType:         cfg.Engine.Type,
		MysqldBin:          cfg.Engine.Mysql.MysqldBin,
		MysqlExtraCnf:      cfg.Engine.Mysql.ExtraCnf,
		PgBinDir:           cfg.Engine.Postgres.BinDir,
		PgRunUser:          cfg.Engine.Postgres.RunUser,
		StateDir:           "/var/lib/sashiki/branches",
		LazyCreate:         cfg.Branches.LazyCreate,
		LazyMaxWait:        cfg.Branches.LazyCreateMaxWait,
		IdleStopAfter:      cfg.Branches.IdleStopAfter,
		DeleteAfterIdle:    cfg.Branches.DeleteAfterIdle,
		OperationRetention: cfg.Branches.OperationRetention,
		Profiles:           profilePolicies(cfg.Branches.Profiles),
		DefaultProfile:     cfg.Branches.DefaultProfile,
		AvailableMem:       availableMem,
		// メモリ admission は engine 非依存のアクセサ経由で取る(#225)。
		// 直接 Engine.Mysql.* を読むと postgres で 0 になり admission が無効化される。
		ExpectedRSSBytes:         parseSize(cfg.ExpectedRSS()),
		MemoryHeadroomBytes:      parseSize(cfg.MemoryHeadroom()),
		BufferPoolBytes:          parseSize(cfg.MemoryBaselineSize()),
		MaxRunning:               cfg.MaxRunning(),
		HighWatermark:            cfg.Storage.HighWatermark,
		CriticalWatermark:        cfg.Storage.CriticalWatermark,
		BaselineKeepLast:         cfg.Baseline.KeepLast,
		BaselineRetention:        cfg.Baseline.Retention,
		DefaultStorageQuotaBytes: parseSize(cfg.Storage.DefaultStorageQuota),
	}, st, bp, eng, hr, db)
	if err != nil {
		log.Fatalf("manager: %v", err)
	}

	token := resolveAPIToken(cfg)
	// app credential も engine 非依存に取る(#225)。postgres では
	// engine.postgres.app_user/app_pass を使う(従来は mysql 側を読んでいた)。
	srv := api.New(mgr, cfg.Domain, cfg.Engine.Type, cfg.AppUser(), cfg.AppPass(), token, db)
	srv.SetOps(ops.New(db))
	srv.SetTrustLoopback(cfg.Auth.TrustLoopback)
	// 接続情報(host/port/user)を proxy 宛にするためのポート。proxy 無効なら 0 で、
	// その場合はブランチへ直結する形の値を返す(#260)。
	srv.SetProxyListen(cfg.Listen.Proxy)
	mgr.SetBaselinePolicy(workspace.RefreshConfig{
		Script:           cfg.Baseline.RefreshScript,
		Timeout:          cfg.Baseline.RefreshTimeout,
		SourceDir:        cfg.Baseline.SourceDir,
		SourceDB:         cfg.Baseline.SourceDB,
		RequireMasked:    cfg.Baseline.RequireMasked,
		RequireValidated: cfg.Baseline.RequireValidated,
		ValidatePort:     cfg.Baseline.ValidatePort,
		MaskedSentinel:   cfg.Baseline.MaskedSentinel,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 前回の再起動/クラッシュで running のまま残った operation を failed に回収する
	// (#53。放置すると op wait がタイムアウトまで待つ)。
	if n, err := db.RecoverInterruptedOperations(); err != nil {
		log.Printf("recover interrupted operations: %v", err)
	} else if n > 0 {
		log.Printf("recovered %d interrupted operation(s) from previous run", n)
	}

	// 起動時に state.db と ZFS/engine を突き合わせる(仕様 20-1)。
	if rep, err := mgr.Reconcile(context.Background()); err != nil {
		log.Printf("reconcile: %v", err)
	} else if len(rep.Demoted)+len(rep.Errored)+len(rep.Orphans)+len(rep.Interrupted) > 0 {
		log.Printf("reconcile: demoted=%d errored=%d orphans=%d interrupted=%d",
			len(rep.Demoted), len(rep.Errored), len(rep.Orphans), len(rep.Interrupted))
	}

	go mgr.RunReaper(ctx, cfg.Branches.ReaperInterval)
	// engine を定期ポーリングして last_conn_at を更新する(#41)。engine が
	// ConnCounter 未実装なら内部で即 return する。
	go mgr.RunConnPoller(ctx, cfg.Branches.ReaperInterval)

	if cfg.Listen.Metrics != "" {
		go func() {
			mux := http.NewServeMux()
			mux.Handle("GET /metrics", api.MetricsHandler(mgr))
			srv := &http.Server{Addr: cfg.Listen.Metrics, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
			log.Printf("sashikid: metrics listening on %s", cfg.Listen.Metrics)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("metrics: %v", err)
			}
		}()
	}

	if cfg.Listen.Proxy != "" {
		// 方式A(#51): proxy が app 認証を終端する。app credential は engine 別の
		// app_user / app_pass(本番は Secrets 由来)を使う(#225 のアクセサ経由)。
		var tlsCfg *tls.Config
		if cfg.Proxy.TLSCert != "" && cfg.Proxy.TLSKey != "" {
			cert, cerr := tls.LoadX509KeyPair(cfg.Proxy.TLSCert, cfg.Proxy.TLSKey)
			if cerr != nil {
				log.Fatalf("proxy tls: %v", cerr)
			}
			tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
			log.Printf("sashikid: proxy TLS 終端を有効化しました")
		}
		// allowed_user: 未設定(nil)なら app_user のみ許可、明示指定(空=任意)なら
		// その値を使う(管理ユーザー接続などのため、#131)。
		allowedUser := cfg.AppUser()
		if cfg.Proxy.AllowedUser != nil {
			allowedUser = *cfg.Proxy.AllowedUser
		}
		// エンジンごとにワイヤプロトコルが違うので実装を選ぶ(#222)。
		// どちらも Router(RouteBranch/TouchConn)と ActiveConns の形は同じ。
		var listen func(context.Context) error
		var activeConns func(string) int
		switch cfg.Engine.Type {
		case "postgres":
			px, err := pgproxy.New(pgproxy.Config{
				Listen:           cfg.Listen.Proxy,
				NamePattern:      cfg.Branches.NamePattern,
				MaxConnPerBranch: cfg.Proxy.MaxConnPerBranch,
				AllowedUser:      allowedUser,
				AppUser:          cfg.AppUser(),
				AppPassword:      cfg.AppPass(),
				TLSConfig:        tlsCfg,
			}, mgr)
			if err != nil {
				log.Fatalf("pgproxy: %v", err)
			}
			listen, activeConns = px.Listen, px.ActiveConns
		default:
			px, err := proxy.New(proxy.Config{
				Listen:           cfg.Listen.Proxy,
				NamePattern:      cfg.Branches.NamePattern,
				MaxConnPerBranch: cfg.Proxy.MaxConnPerBranch,
				AllowedUser:      allowedUser,
				AppUser:          cfg.AppUser(),
				AppPassword:      cfg.AppPass(),
				TLSConfig:        tlsCfg,
			}, mgr)
			if err != nil {
				log.Fatalf("proxy: %v", err)
			}
			listen, activeConns = px.Listen, px.ActiveConns
		}
		mgr.SetActiveConns(activeConns)
		go func() {
			if err := listen(ctx); err != nil {
				log.Fatalf("proxy: %v", err)
			}
		}()
		log.Printf("sashikid: proxy listening on %s (engine=%s)", cfg.Listen.Proxy, cfg.Engine.Type)
	}

	log.Printf("sashikid: listening on %s (backend=%s engine=%s)",
		cfg.Listen.API, cfg.Storage.Backend, cfg.Engine.Type)
	if err := srv.Listen(ctx, cfg.Listen.API); err != nil {
		log.Fatal(err)
	}
}

// slogWriter は既存の log.Printf 出力を slog(JSON)へ橋渡しする。
type slogWriter struct{}

func (slogWriter) Write(p []byte) (int, error) {
	slog.Info(strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}

// profilePolicies は config の profile 定義を workspace の型へ変換する(#34)。
func profilePolicies(in map[string]config.ProfilePolicy) map[string]workspace.ProfilePolicy {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]workspace.ProfilePolicy, len(in))
	for name, p := range in {
		out[name] = workspace.ProfilePolicy{
			IdleStopAfter:   p.IdleStopAfter,
			DeleteAfterIdle: p.DeleteAfterIdle,
		}
	}
	return out
}
