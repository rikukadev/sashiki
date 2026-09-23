# sashiki 設計仕様 v2

> 2026-09-05 の設計レビューを反映した改訂版。旧名 twig 時代の「設計・手順書 v2」の Part B(設計仕様)を収録したもの。
> Part A(EBS-ZFS PoC 手順)は `sashiki init` と E2E に置き換えられたため収録しない(背景は
> [EBS 版 PoC 記事](https://rikuka.dev/blog/db-branch-zfs-mysql-poc/) を参照)。
>
> 23 章(プロキシ)は issue #31 の決定(2026-09-06、認証終端=方式 A の採用)を反映して原文から改訂済み。
> 仕様と実装の既知の差分は issue #44 / #48(および精査で起票した #78〜#90)で追跡する。個別の設計判断は [DECISIONS.md](DECISIONS.md)(ADR)へ。

-----

## 10. 位置づけとスコープ

### 10-1. sashiki とは

> **任意の baseline から、独立した使い捨ての database workspace を秒単位で払い出す control plane。**

ZFS の clone が速いことを利用したスクリプトではなく、「巨大な開発 DB を cheap-to-store / cheap-to-recreate / expensive-only-while-running な資源に変える」基盤。

- **コアは汎用、最初の体験は PR に特化。** GitHub PR は sashiki core の外側にある adapter にすぎない
- **EBS-ZFS が default backend。** FSx-ZFS は「速い版の上位互換」ではなく、storage と compute を分離するための別特性の backend

### 10-2. ユースケース

|用途                |例                                                                              |
|------------------|-------------------------------------------------------------------------------|
|PR Preview        |`sashiki create pr-123`                                                        |
|CI / E2E          |`sashiki create e2e-8472 --profile ci`                                         |
|個人 sandbox        |`sashiki create alice --profile sandbox`                                       |
|migration 検証      |`sashiki create migration-user-index` — ALTER / CREATE INDEX / backfill を本番相当データ量で|
|障害再現 / デバッグ       |`sashiki create bug-4821`                                                      |
|release 候補検証、デモ、研修|`sashiki create release-202609`                                                |

**PR ごとに異なる schema を持てる**ことは主要ユースケース。clone 後は独立した filesystem なので、migration 案 A/B の比較、rollback 検証、3000 万行 UPDATE の dry-run が互いに影響なくできる。

### 10-3. 向かない用途(README に明記)

sashiki は**データ量・内容・schema の再現性は高いが、インフラトポロジーの再現性は目的にしない**。

- HA / failover 試験
- Aurora / RDS 固有挙動の検証
- replication topology
- 本番相当の I/O ベンチマーク
- 複数 branch 同時実行による厳密な性能比較(共有ホスト / EBS / zpool / NFS の影響を受ける)

### 10-4. 設計原則

1. **PII masking は security invariant。** sashiki はマスクされていない本番データから branch を作らない。マスクは baseline 作成パイプラインで行い、branch ごとに後からマスクする設計にはしない
1. **baseline は immutable。** 更新は新しい baseline の publish であり、既存 branch の origin は変わらない
1. **既存 branch を勝手に最新 baseline へ追従させない。** 追従は明示的な `recreate`
1. **稼働中の datadir をスナップショットにしない。** baseline も `@init` も graceful shutdown 後に取得する
1. **1 deployment ＝ 1 backend ＝ 1 アプリ系統。** backend も baseline も混ぜない
1. **sashiki core にアプリ固有処理を入れない。** Git、migration、Slack、GitHub は hooks / adapter 側
1. **idle stop は capacity 管理の主機能。** コスト削減の付属機能ではない

-----

## 11. ドメインモデル

内部の上位概念は **Workspace**。CLI / API では `branch` と呼ぶ(利用者の語彙に合わせる)。

```
Workspace (branch)
 ├─ Origin Baseline     どの baseline から生えたか
 ├─ Volume              storage backend 上の実体
 ├─ Engine Instance     mysqld プロセス(port, socket, pid)
 ├─ Init Snapshot       reset の戻り先
 ├─ Lifecycle Policy    profile / lease
 ├─ Metadata            provenance, purpose, owner, source
 └─ State               lifecycle state + engine state
```

### 11-1. Branch の状態

lifecycle state と engine state を分けて持つ余地を残す。v0.1 では 1 つの `state` でよいが、将来分離できるよう API の出力は両方のフィールドを持つ。

|state      |意味                                                      |
|-----------|--------------------------------------------------------|
|`creating` |clone / hook / @init 作成中                                |
|`running`  |mysqld 稼働中                                              |
|`sleeping` |volume はあるが mysqld 停止中(idle stop)                       |
|`resetting`|reset / recreate 中                                      |
|`deleting` |削除ジョブ実行中(fsx では数分続く)                                    |
|`error`    |失敗。`failed_operation` / `error_code` / `recoverable` を持つ|

error の情報:

```json
{
  "state": "error",
  "failed_operation": "create",
  "error_code": "hook_failed",
  "error_message": "on-create exited 1: migration 20260905_042 failed",
  "recoverable": true,
  "suggested_actions": ["sashiki retry pr-123", "sashiki delete pr-123"]
}
```

### 11-2. Branch の provenance

```json
{
  "name": "migration-user-index",
  "origin_baseline": "baseline-20260905-abc123",
  "profile": "sandbox",
  "owner": "team-shop",
  "purpose": "migration-test",
  "created_at": "2026-09-05T10:00:00Z",
  "expires_at": "2026-09-07T10:00:00Z",
  "source": { "type": "github_pr", "repository": "shop", "ref": "123" }
}
```

`source` は opaque な JSON。sashiki core は `github_pr` の意味を知らない。adapter が書き、adapter が読む。

### 11-3. Profile と lease

用途ごとに lifecycle が違う。`config.yaml` で定義し、`--profile` で選ぶ。

```yaml
branches:
  default_profile: preview
  profiles:
    preview: { idle_stop_after: 30m, delete_after_idle: 168h }
    ci:      { idle_stop_after: 5m,  delete_after_idle: 1h }
    sandbox: { idle_stop_after: 1h,  delete_after_idle: 720h }
```

branch は永久資源ではなく lease。`expires_at` を持ち、延長は明示操作。

```bash
sashiki lease renew alice --for 7d
```

PR close webhook による削除は eager cleanup、TTL / lease は correctness の担保、と役割を分ける。

-----

## 12. Baseline ライフサイクル

### 12-1. Baseline は main の immutable publication

```
main HEAD + main migrations + マスク済み開発データ + validation
        ↓
baseline-20260905-abc123     ← 不変
```

`current` は pointer にすぎない。新しい baseline を publish しても既存 branch の origin は変わらない。

```
baseline-A
 ├─ pr-100
 └─ pr-101
baseline-B  ← current
 ├─ pr-102
 └─ migration-test
```

### 12-2. Provenance

schema の鮮度と data の鮮度は一致しないので分けて持つ。

```json
{
  "name": "baseline-20260905-abc123",
  "snapshot": "dbpool/base@baseline-20260905-abc123",
  "source_revision": "abc123",
  "schema_revision": "20260905_042",
  "data_as_of": "2026-09-05T02:00:00Z",
  "masked": true,
  "mask_pipeline_version": "1.4.0",
  "created_at": "2026-09-05T03:10:00Z",
  "validated": true
}
```

利用者には `Schema: main@abc123 / Data: 2026-09-05 02:00 UTC` と見せる。

### 12-3. build → validate → publish

current をいきなり切り替えない。

```
build      : データ投入 → マスク → main の migration 適用 → graceful shutdown → auto.cnf 削除 → snapshot(candidate)
validate   : candidate から一時 branch を起動して検証 → 破棄
publish    : current pointer を candidate に向ける
```

> 実装ノート: 典型ケース(SQL を流すだけ)は refresh.sh を書かずに
> `baseline.source_dir` に SQL を置くだけでよい(組み込みローダー、#101)。

validation の最低項目:

- mysqld が起動し、クラッシュリカバリが走っていない
- migration version が期待値
- 主要テーブルが存在する
- mask validation(PII 列にマスク前パターンが残っていない)
- sanity query(件数の下限など)

壊れた migration が main に入っても、新規 branch が全滅しない。

### 12-4. 更新契機(ハイブリッド)

```
main merge
 ├─ migration 変更あり → 即 refresh(schema freshness)
 └─ 通常変更           → nightly refresh(data freshness)
```

大規模 DB で「merge のたびに全データ再投入」は重いので分ける。migration のみの refresh は「現 baseline から一時 branch を切って migration を当て、それを新 baseline として snapshot」で済ませられる(データ再投入なし)。

### 12-5. rollback / promotion / GC

```bash
sashiki baseline list
sashiki baseline set baseline-20260904-9f8e7d    # 明示的に切り替え。直前の正常版へ即座に戻せる
```

GC の残す条件: current / branch から参照中 / retention 内 / keep_last の対象。

```yaml
baseline:
  keep_last: 3
  retention: 720h   # 0 = 期間では残さず keep_last だけで決める
```

ZFS clone は origin snapshot に依存するので、sashiki が lineage を把握して GC する。参照中の baseline は消せない(`zfs promote` は使わない。lineage が追えなくなる)。

### 12-6. PII masking の位置

```
production → export/copy → mask/anonymize → validation → baseline → branches
```

マスクは baseline build パイプラインの必須ステップ。`masked: false` の baseline は `publish` できない(設定で強制)。

-----

## 13. Branch ライフサイクル

### 13-1. Engine インターフェース

```go
type Engine interface {
    Start(ctx, inst) error
    Stop(ctx, inst) error        // 正常終了。snapshot 前は必ずこちら
    Kill(ctx, inst) error        // rollback で破棄する dirty state に使う
    WaitReady(ctx, inst) error
    IsRunning(ctx, inst) (bool, error)
}
// オプショナル: ConnCounter(接続数。idle 判定・last_conn_at 更新に使用)
```

使い分け:

- baseline snapshot 前 → `Stop`(graceful)
- `@init` snapshot 前 → `Stop`(graceful)
- ebs-zfs の reset 前 → `Kill`(rollback で filesystem ごと捨てるので graceful は不要。PoC では reset 時間の大半が graceful shutdown だった)
- rollback 後 → `Start`

### 13-2. create

```
clone baseline → (hook がなければここで @init) → Start → Ready
  → on-create hook(migration / seed)
  → StopGracefully → @init snapshot → Start → Ready → running
```

**hook の結果が reset の戻り先になる。** hook がない場合は clone 直後・起動前に `@init` を取得してよい。稼働中の datadir を `@init` にはしない。

create 前に capacity check(14 章)。足りなければ `limit_reached` で拒否し、既存 mysqld を OOM に巻き込まない。

### 13-3. reset と recreate(分離する)

|       |reset                                                 |recreate                                   |
|-------|------------------------------------------------------|-------------------------------------------|
|意味     |同じ baseline + 同じ branch 固有 migration の**作成直後**に戻す     |**最新 current baseline** から作り直し、hook を再適用   |
|Git    |`git reset --hard`                                    |`git rebase main`                          |
|戻り先    |`@init`                                               |新 `@init`                                  |
|ebs-zfs|Kill → rollback → Start                               |新 clone → hook → 新 @init → 切替 → 旧 volume 削除|
|fsx-zfs|新 clone(@init 相当の snapshot から)→ 切替 → 旧 volume async 削除|同上                                         |

```bash
sashiki reset pr-123      # 壊したので戻す
sashiki recreate pr-123   # main が進んだので追従する
```

既存 branch を自動で recreate しない。利用者が明示的に選ぶ。

### 13-4. reset の抽象は「logical state replacement」

rollback ではなく「branch の中身を `@init` 相当の状態に置き換える」と定義する。ebs-zfs の rollback はその最適化。fsx-zfs は新 clone + engine 切替 + 旧 volume 非同期削除で同じ意味を実現する。切替(port / socket / state.db の更新)はコアに 1 回だけ書く。

### 13-5. idle stop / wake

- `idle_stop_after` 経過で `StopGracefully` → `sleeping`。volume と port 予約は保持
- wake: capacity check → `Start` → `Ready` → `running`
- `last_conn_at` は engine の接続数(`Threads_connected`)を定期取得して更新。proxy がなくても動く

### 13-6. delete と TTL

- delete は常に非同期ジョブ。ebs-zfs は即完了、fsx-zfs は数分 `deleting` で一覧に残る
- `delete_after_idle` または `expires_at` 超過で自動削除。削除前に `on-delete` hook

### 13-7. retry / error recovery

```bash
sashiki retry pr-123                 # failed_operation を再実行
sashiki hooks run pr-123 on-create   # hook だけ再実行
```

自動 retry はしない(migration の副作用が怖い)。`error` 状態の branch は volume を残すのでログと datadir を調べられる。

-----

## 14. Capacity 管理

### 14-1. Memory admission control

PoC の結果: メモリ不足時は新 branch が失敗するのではなく、**既存 mysqld が OOM killer に殺される**。`max_branches` では防げない。

```yaml
branches:
  max_branches: 100     # volume の数
engine:
  mysql:
    max_running: 10     # 同時稼働 mysqld の数(0 = 無制限)
    buffer_pool_size: 256M
    expected_rss: 600M  # buffer pool + 固定オーバーヘッドの見積もり
    memory_headroom: 1G # host に残す
```

create / wake / recreate 前に `MemAvailable > expected_rss + memory_headroom` を確認。足りなければ拒否(`limit_reached`)。将来は「一番古い sleeping 候補を止めて空ける」を選べるようにする。

### 14-2. Storage admission / quota

```yaml
storage:
  high_watermark: 0.8            # 0〜1 の比率。warning、メトリクスで通知
  critical_watermark: 0.9        # create / wake / recreate を拒否
  default_storage_quota: 20GiB   # zfs set refquota(ebs-zfs のみ)
```

migration の失敗や大量 UPDATE で 1 branch が pool を食い尽くすのを防ぐ。

### 14-3. Port allocator

`ss -ltn` は使わない(sleeping branch は listen していないので再割当される)。**state.db の `port UNIQUE` が source of truth。** `port_range` から未使用を選び、branch 削除まで予約を保持する。

### 14-4. サイズ表示

CoW なので「論理サイズ 482 GiB、固有差分 18 MiB」が普通に起きる。分けて出す。

```
Logical DB size    482 GiB     (zfs referenced)
Private delta       18 MiB     (zfs used)
Origin baseline    baseline-20260905-abc123
```

-----

## 15. Storage 抽象

### 15-1. 実測(参考: MonotaRO 記事、rikuka.dev 記事、PoC)

|操作        |ebs-zfs          |fsx-zfs                   |
|----------|-----------------|--------------------------|
|snapshot  |瞬時               |37〜51 秒                   |
|clone     |瞬時               |52〜71 秒                   |
|mount + 起動|1.3 秒            |11〜12 秒                   |
|reset     |数秒(Kill なら 1〜2 秒)|10 分超(restore)→ 使わず新 clone|
|delete    |1 秒              |6 分(非同期)                  |

### 15-2. インターフェース

```go
type Capabilities struct {
    TypicalCreate   time.Duration   // ebs: 2s   / fsx: 70s
    TypicalReset    time.Duration   // ebs: 2s   / fsx: 80s
    FastRollback    bool            // ebs: true / fsx: false
    AsyncDelete     bool            // ebs: false/ fsx: true
    SharedStorage   bool            // ebs: false/ fsx: true(複数 host からマウント可)
    HostIndependent bool            // ebs: false/ fsx: true(host を捨てても volume が残る)
}

type Storage interface {
    Capabilities() Capabilities
    Clone(ctx, from SnapshotRef, name string) (Volume, error)   // マウント済み・使用可能になるまで待って返す
    Snapshot(ctx, vol Volume, tag string) (SnapshotRef, error)
    Rollback(ctx, vol Volume, snap SnapshotRef) error           // FastRollback のときだけ呼ばれる
    DeleteAsync(ctx, vol Volume) (OperationID, error)
    SetQuota(ctx, vol Volume, bytes int64) error
    Usage(ctx, vol Volume) (logical, private int64, err error)
    ListSnapshots(ctx) ([]SnapshotRef, error)
}
```

方針: **実装詳細は隠すが、UX に影響する性能特性は隠さない。**

`Clone()` が返る条件は「マウント済みで filesystem が使える」まで。fsx の `AdministrativeActions` や `Lifecycle` を Branch Manager が知り始めたら abstraction が漏れている。

### 15-3. コアの挙動切り替え

|機能               |判定                    |ebs-zfs                |fsx-zfs                            |
|-----------------|----------------------|-----------------------|-----------------------------------|
|reset            |`FastRollback`        |Kill → Rollback → Start|新 clone → 切替 → 旧 volume DeleteAsync|
|delete           |常に非同期                 |即完了                    |`deleting` で数分残る                   |
|baseline refresh |常に非同期 job             |数秒                     |数分                                 |
|lazy create      |`TypicalCreate <= 20s`|可                      |不可。PR open トリガー必須                  |
|host 使い捨て / Spot |`HostIndependent`     |不可                     |可                                  |

### 15-4. backend の位置づけ

|  |ebs-zfs(**default**)                     |fsx-zfs                                                             |local-zfs(将来)        |
|--|-----------------------------------------|--------------------------------------------------------------------|---------------------|
|特徴|fast / simple / interactive / single-host|shared / host-independent / multi-host / control-plane latency heavy|fastest / laptop / VM|
|向く|sandbox、migration 検証、CI、PR Preview、秒 UX  |host 使い捨て、Spot、複数 host、1 台の RAM 限界超え、PR open に 1〜2 分待てる             |開発者の手元               |

判断軸は「小規模→EBS、大規模→FSx」ではない。**次のどれかが必要になったら FSx**: multi-host / compute replacement / Spot / host 障害と storage の分離 / 1 台の RAM 限界。100 branch あっても running が 5 個なら EBS 1 台で足りる。

**同じ sashikid で backend を混ぜない。** 別 deployment にする。

### 15-5. fsx-zfs 実装メモ

- 完了判定は `Lifecycle` ではなく `AdministrativeActions[].Status == COMPLETED`。間違えると復元中の volume に mysqld を起動して datadir を壊す
- NFS export は `no_root_squash`。マウントは `nfsvers=4.1,rsize=1048576,wsize=1048576,hard,noatime`
- SG: TCP/UDP `111`, `2049`, `20001-20003`
- `delete-volume` は `DELETE_CHILD_VOLUMES_AND_SNAPSHOTS`
- 最小構成(Single-AZ gen1、64GB、64MB/s)で東京約 6 円/時

-----

## 16. Hooks

### 16-1. 責務の境界

sashiki core がやること: hook を呼ぶ / timeout / stdout・stderr 保存 / exit code 保存 / metadata 保存。
hook 側がやること: Git clone、PR checkout、依存インストール、migration、seed、feature flag、外部 API、Slack、GitHub。

### 16-2. 種類とタイミング

|hook                  |タイミング                                                |
|----------------------|-----------------------------------------------------|
|`on-create`           |clone → Start → Ready 後、`@init` snapshot **前**       |
|`on-reset`            |rollback → Start → Ready 後                           |
|`on-recreate`         |新 clone に対して `on-create` と同じ(省略時は `on-create` を使う)   |
|`on-delete`           |StopGracefully 後、volume 削除前                          |
|(baseline build)      |hook ではなく `baseline.refresh_script` / `source_dir`(12-3)。環境変数の `SASHIKI_EVENT` は `baseline-build`|
|`on-baseline-validate`|candidate から起動した一時 branch に対して                       |

場所 `hooks.dir`(既定 `/etc/sashiki/hooks`)、実行ユーザー `sashiki`、timeout 既定 10 分、引数なし・環境変数渡し。

|環境変数                                           |例                                                                |
|-----------------------------------------------|-----------------------------------------------------------------|
|`SASHIKI_EVENT`                                |`on-create`                                                      |
|`SASHIKI_BRANCH`                               |`pr-123`                                                         |
|`SASHIKI_PORT` / `SASHIKI_SOCKET` / `SASHIKI_DATADIR`|`3401` / `/tmp/mysql-pr-123.sock` / `/dbpool/branches/pr-123/data`|
|`SASHIKI_ENGINE`                               |`mysql`                                                          |
|`SASHIKI_ADMIN_USER`                           |`root`(socket 経由)                                                |
|`SASHIKI_ORIGIN_SNAPSHOT`                      |`dbpool/base@baseline-20260905-abc123`                           |
|`SASHIKI_BASELINE_SCHEMA_REVISION`             |`20260905_042`(baselines に登録があるときのみ設定)                          |
|`SASHIKI_SOURCE_JSON`                          |`{"type":"github_pr","repository":"shop","ref":"123"}`(source があるときのみ設定)|
|`SASHIKI_OWNER` / `SASHIKI_PURPOSE` / `SASHIKI_PROFILE`|`alice` / `review` / `preview`(branch の provenance。core は解釈しない)|
|`SASHIKI_STATE_DIR`                            |`/var/lib/sashiki/branches/pr-123`(hook の作業領域)                   |

- 非 0 終了: `on-create` は branch を `error`(recoverable)で残す。`on-reset` / `on-delete` は記録して続行。`on-baseline-validate` の失敗は publish を阻止
- ログ: `/var/log/sashiki/hooks/<branch>-<event>-<ts>.log`

-----

## 17. REST API v1

- `http://<host>:8080/v1`、`Authorization: Bearer <token>`(localhost は不要)
- 時刻は RFC3339 UTC。metrics は `listen.metrics`(既定 `:9100`)**のみ**。API 側に `/metrics` は置かない

|Method  |Path                        |説明                                                         |成功             |エラー                              |
|--------|----------------------------|-----------------------------------------------------------|---------------|---------------------------------|
|`GET`   |`/branches`                 |一覧                                                         |200            |                                 |
|`POST`  |`/branches`                 |作成 `{name, profile?, baseline?, port?, owner?, purpose?, ttl?, source?}`|202 + operation|400 / 409 / 507                  |
|`GET`   |`/branches/{name}`          |詳細                                                         |200            |404                              |
|`POST`  |`/branches/{name}/reset`    |`@init` へ                                                  |202 + operation|404 / 409                        |
|`POST`  |`/branches/{name}/recreate` |current baseline から作り直し                                    |202 + operation|404                              |
|`POST`  |`/branches/{name}/wake`     |sleeping → running(同期)                                    |200            |404 / 507                        |
|`POST`  |`/branches/{name}/sleep`    |running → sleeping(同期)                                    |200            |404                              |
|`POST`  |`/branches/{name}/retry`    |failed_operation を再実行                                      |202 + operation|404 / 409                        |
|`POST`  |`/branches/{name}/lease`    |`{for: "7d"}`                                           |200            |404                              |
|`DELETE`|`/branches/{name}`          |削除                                                         |202 + operation|404                              |
|`GET`   |`/baselines`                |一覧                                                         |200            |                                 |
|`GET`   |`/baselines/current`        |current                                                    |200            |                                 |
|`POST`  |`/baselines/build`          |build → candidate                                          |202 + operation|409 実行中                          |
|`POST`  |`/baselines/{name}/validate`|                                                           |202 + operation|404                              |
|`POST`  |`/baselines/{name}/publish` |current pointer 更新                                         |200            |404 / 412 未 validate / 412 未 mask|
|`DELETE`|`/baselines/{name}`         |GC 対象外なら拒否(同期)                                            |200            |409 参照中                          |
|`GET`   |`/operations/{id}`          |非同期操作の状態                                                   |200            |404                              |
|`GET`   |`/capacity`                 |memory / storage / ports の空き                               |200            |                                 |
|`GET`   |`/healthz`                  |                                                           |200            |                                 |

> **実装ノート(現在)**: 表の baseline 系は実装では `GET /v1/baseline`(current)/ `GET /v1/baselines` / `POST /v1/baseline/set|gc|refresh`(build→validate→publish の一括)。段階 API(build/validate/publish/delete)は #84 で実装済み(`POST /v1/baseline/{build,validate,publish,delete}`、build/validate は 202 非同期)。refresh(一括)は互換維持。
> また表にない実装済みエンドポイントとして `GET /v1/doctor`、`POST /v1/gc/orphans`、`POST /v1/drain`、`GET /v1/branches/{name}/schema`、`POST /v1/branches/{name}/query`(データブラウザ)、`POST /v1/branches/{name}/hooks/{event}`、Web UI(`GET /`)がある。
> **202 + operation 非同期化は実装済み(#82)**。`create` / `reset` / `recreate` / `retry` / `delete` は **202 + `{operation_id}`**(+ `Sashiki-Operation-Id` ヘッダ)を返し、本体はバックグラウンド実行される(fsx-zfs で数分かかるため)。名前の妥当性・存在チェック・`exist_ok` 短絡は同期で先に評価して即 4xx/200 を返す。`wake` / `lease` は高速なので同期のまま(200)。**CLI は既定で `--wait`**(operation の完了までポーリングし、体感を同期に保つ。`--no-wait` で `operation_id` だけ返す。`--timeout` / `--interval` 可)。GitHub Action も 202 を検知して poll する。

すべての変更操作は **operation** を返す。ebs-zfs で 1 秒で終わっても同じ形にする(fsx で必要になるため)。

```json
{ "operation_id": "op_01J...", "type": "create", "target": "pr-123", "state": "running", "started_at": "...", "finished_at": null, "error": null }
```

`POST /branches` は既存なら 409。`?exist_ok=true` で 200 + 既存を返す。

### 接続情報(`host` / `port` / `user`)

branch の応答が返す `host` / `port` / `user` は、**そのまま繋がる 3 つ組**であることを保証する。
利用者(GitHub Action / IaC / アプリの env)はこれを解釈せずそのまま渡せる。

|`listen.proxy`|`host`|`port`|`user`|
|---|---|---|---|
|設定あり(既定 `:3306`)|`domain`|**proxy のポート**|`<app_user>@<branch>`|
|空(proxy 無効)|`domain`|ブランチの内部ポート|`<app_user>`|

proxy はユーザー名でルーティングするため、`user` と `port` は必ず同じ側を指していなければ
ならない。片方だけ proxy 形式にすると、**どう解釈しても接続できない値**になる(#260)。

`engine_port` はブランチ自身の listener を常に返す。**接続用ではなく**、ログや `ss` の出力と
突き合わせる調査用。

エラー形式と `code`: `invalid_name`, `branch_exists`, `branch_not_found`, `baseline_not_found`, `limit_reached`(memory / storage / max_running / max_branches を `detail` で区別), `hook_failed`, `storage_error`, `engine_error`, `operation_in_progress`, `precondition_failed`, `unauthorized`, `insufficient_scope`(403、トークンの scope 不足)。全一覧は docs/REFERENCE.md

-----

## 18. CLI

```
sashiki create <name> [--profile P] [--baseline B] [--owner O] [--purpose S] [--source JSON] [--wait] [--json]
sashiki reset <name>            sashiki recreate <name>
sashiki sleep <name>            sashiki wake <name>
sashiki delete <name>           sashiki retry <name>
sashiki lease renew <name> --for <dur>
sashiki list [--json]           sashiki show <name> [--json]
sashiki connect <name>          # engine に応じて mysql / psql を exec
sashiki hooks run <name> <event>
sashiki baseline list | build | validate <b> | publish <b> | set <b> | gc [--dry-run]
sashiki op list | show <id> | wait <id>
sashiki capacity
sashiki doctor
sashiki token create --name N [--scope branches|admin] | list | revoke N
sashiki init --pool P --device DEV [--engine mysql|postgres] [--app-pass PW] [--platform darwin] [--yes]
sashiki version
```

終了コード: 0 成功 / 1 一般 / 2 引数 / 3 見つからない / 4 既存(409) / 5 capacity 不足(507) / 6 待機タイムアウト。`--json` は API レスポンスそのまま。

`sashiki show` の error 表示例:

```
STATE       error
OPERATION   create
ERROR       hook_failed
RECOVERABLE yes
Suggested:  sashiki retry pr-123 / sashiki delete pr-123
```

-----

## 19. state.db(SQLite)

```sql
CREATE TABLE baselines (
  name              TEXT PRIMARY KEY,
  snapshot          TEXT NOT NULL,
  source_revision   TEXT, schema_revision TEXT, data_as_of TEXT,
  masked            INTEGER NOT NULL DEFAULT 0,
  mask_pipeline_version TEXT,
  validated         INTEGER NOT NULL DEFAULT 0,
  is_current        INTEGER NOT NULL DEFAULT 0,
  created_at        TEXT NOT NULL
);

CREATE TABLE branches (
  name              TEXT PRIMARY KEY,
  state             TEXT NOT NULL,
  engine_state      TEXT,                          -- 将来分離用
  port              INTEGER NOT NULL UNIQUE,       -- port allocator の source of truth
  origin_baseline   TEXT NOT NULL REFERENCES baselines(name),
  init_snapshot     TEXT,
  volume_ref        TEXT NOT NULL,                 -- backend 固有の識別子
  profile           TEXT NOT NULL,
  owner TEXT, purpose TEXT, source_json TEXT,
  created_at        TEXT NOT NULL,
  last_conn_at      TEXT,
  expires_at        TEXT,
  failed_operation TEXT, error_code TEXT, error_message TEXT, recoverable INTEGER
);

CREATE TABLE operations (
  id TEXT PRIMARY KEY, type TEXT NOT NULL, target TEXT, state TEXT NOT NULL,
  started_at TEXT NOT NULL, finished_at TEXT, error_json TEXT
);

CREATE TABLE hook_runs (
  id INTEGER PRIMARY KEY, branch TEXT, event TEXT NOT NULL, operation_id TEXT,
  started_at TEXT NOT NULL, finished_at TEXT, exit_code INTEGER, log_path TEXT
);

CREATE TABLE tokens (
  name TEXT PRIMARY KEY, hash TEXT NOT NULL, created_at TEXT NOT NULL, last_used_at TEXT,
  scope TEXT NOT NULL DEFAULT 'branches'   -- branches | admin(#294)
);
```

サイズ(logical / private)は保存せず `Storage.Usage()` を都度引く。

-----

## 20. 運用・安全性

### 20-1. 起動時 reconciliation

daemon 再起動後、state.db / ZFS dataset / mysqld プロセス / systemd unit は食い違い得る。起動時に必ず突き合わせる。

|state.db |実体        |結果                                                      |
|---------|----------|--------------------------------------------------------|
|running  |プロセス死亡    |`sleeping`(volume 健全なら)または `error`                      |
|branch あり|dataset なし|`error`(recoverable: false)                             |
|なし       |dataset あり|`orphan` として記録。`sashiki doctor` が報告、`sashiki gc --orphans` で削除|
|port 重複  |          |`error`、片方に再割当を提案                                       |

### 20-2. `sashiki doctor`

```
[OK]    zpool dbpool healthy (used 61%)
[OK]    current baseline baseline-20260905-abc123 exists, validated, masked
[OK]    state.db writable
[OK]    port allocation consistent
[WARN]  orphan dataset dbpool/branches/pr-91
[ERROR] pr-123 marked running but mysqld is dead
[OK]    memory headroom 3.2 GiB (max_running 10, running 4)
```

### 20-3. 権限

- **AppArmor**: PoC の complain モードは製品仕様にしない。`sashiki init` が `/dbpool/branches/**` と `/run/sashiki/**` だけを許可する mysqld プロファイルを生成する
- **sudoers**: `/usr/sbin/zfs` 全体は広すぎる。v0.1 は `zfs clone|snapshot|rollback|destroy|set|get dbpool/branches/*` にパス制限したラッパースクリプト経由。v0.3 で `sashiki-root-helper`(限定された zfs / mount / systemctl 操作だけを受ける小さな setuid-less デーモン)に閉じ込める
- **`CAP_NET_BIND_SERVICE` は不要**(3306 / 8080 は非特権ポート)
- sashikid は `sashiki` ユーザーで動く。mysqld は `mysql` ユーザー

### 20-4. API トークン

認証は「**loopback は無認証・ホスト外は Bearer トークン**」(ADR-005)。同一ホストの CLI はトークン不要(sashikid が loopback からのリクエストを通す)。ホスト外から API を叩く主体(GitHub Action・リモート CLI)にだけ Bearer トークンが要る。

**発行主体は sashiki 運用者(sashikid ホストに root で入れる人)**。トークンは 2 系統ある:

1. **state.db トークン(`sashiki token`)** — root がホスト上で `sashiki token create --name <n> [--scope branches|admin]` で発行(既定 `branches` = ブランチ操作と読み取り。`admin` は baseline publish / promote、drain、gc、データブラウザ、hook 手動実行も可)。**平文は 1 回だけ表示**し、DB には SHA-256 ハッシュのみ保存(`state.db` の tokens テーブル、last_used_at 記録)。`list` / `revoke` で管理。複数本・失効可。
2. **env トークン(`SASHIKI_API_TOKEN`)** — sashikid 起動時の環境変数で渡す静的 1 本(後方互換)。Terraform モジュールはこれを `random_password` で生成し、SSM SecureString(出力 `api_token_ssm_path`)に保存する。利用側は SSM から取得する。

`sashiki init` はトークンを発行しない(パッケージ/権限/zpool/config 生成まで)。手動運用では init 後に `token create` を 1 回叩く。

**利用側**は `SASHIKI_API_TOKEN`(優先)または `~/.config/sashiki/token` からトークンを読み、`Authorization: Bearer <token>` で送る。サーバーは env トークンか state.db のハッシュと照合し、DB 照合エラーは認証失敗と区別してログに残す(無言の 401 にしない)。

ローテーションは新規発行 → 配布先差し替え → 旧トークンを `revoke`。

**注意**: 認証免除は接続元が loopback かで判定するため、リバースプロキシ越しに公開すると接続元が 127.0.0.1 に見えて無認証で通る。外部公開時は sashikid を直接 listen させ、トークン必須で運用する。

### 20-5. Observability

- Prometheus(`:9100`): branches by state、running count、memory headroom、pool usage、create/reset 所要時間、hook 失敗数、operation 数
- 構造化ログ(JSON)に `operation_id` と `branch` を必ず含める

-----

## 21. 設定ファイル

実装(`internal/config`)と同じ形。**未知のキーは起動時にエラー**になるので、ここに無いキーは書かない
(下のサンプルはそのまま `config.Load` で読めることをテストで確かめている)。サイズは `256M` / `256MB` /
`20GiB` のいずれも可(2 進)。watermark は 0〜1 の比率。

```yaml
listen:
  api: "127.0.0.1:8080"           # Web UI もここ
  proxy: "0.0.0.0:3306"           # "" で proxy 無効
  metrics: "127.0.0.1:9100"       # 認証なし。loopback 以外で開くなら到達元を絞る

domain: sashiki.internal          # API の host に返る名前
state_db: /var/lib/sashiki/state.db
run_dir: /run/sashiki
log_dir: /var/log/sashiki
log_format: text                  # text | json

storage:
  backend: ebs-zfs                # ebs-zfs | fsx-zfs | apfs | reflink(旧名 zfs / fsx も可)
  high_watermark: 0.8             # 0〜1。0 で無効
  critical_watermark: 0.9         # 超えたら create / wake / recreate を拒否
  default_storage_quota: 20GiB    # branch ごとの refquota。空 = 無制限(ebs-zfs のみ)
  ebs-zfs:
    pool: dbpool
    base_dataset: dbpool/base
    branch_parent: dbpool/branches
    baseline_snapshot: baseline
    sudo: true
  fsx-zfs:
    region: ap-northeast-1
    filesystem_id: fs-xxxx
    base_volume_id: fsvol-xxxx
    dns_name: fs-xxxx.fsx.ap-northeast-1.amazonaws.com   # 必須
    parent_volume_id: ""          # 空なら自動発見
    baseline_snapshot: baseline
    mount_root: /mnt/sashiki
  local:                          # apfs / reflink
    root: ""
    baseline_snapshot: baseline

engine:
  type: mysql                     # mysql | postgres
  mysql:
    port_range: [3401, 3600]
    buffer_pool_size: 256M        # branch の mysqld に渡り、メモリ見積もりにも使う
    expected_rss: 600M
    memory_headroom: 1G
    max_running: 10               # 同時稼働 mysqld 数(0 = 無制限)
    app_user: dev                 # baseline に作る接続ユーザー(旧名 proxy_user)
    app_pass: dev                 # Linux の init はランダム生成(旧名 proxy_pass)
    env_dir: /run/sashiki
    sudo: true
    mode: systemd                 # systemd | process
    mysqld_bin: /usr/sbin/mysqld  # process モードの mysqld
    run_user: mysql
    extra_cnf: ""                 # プロジェクト固有の my.cnf
  postgres:                       # engine.type: postgres のときに読まれる
    port_range: [5433, 5632]
    bin_dir: /usr/lib/postgresql/16/bin
    env_dir: /run/sashiki
    listen_addresses: 127.0.0.1
    sudo: true
    app_user: dev
    app_pass: dev
    shared_buffers: 128M
    expected_rss: 600M
    memory_headroom: 1G
    max_running: 10
    mode: systemd
    run_user: postgres
    initdb_args: []

proxy:
  max_conn_per_branch: 50
  tls_cert: ""                    # tls_cert と tls_key は両方指定する
  tls_key: ""
  # allowed_user: dev             # 未設定 = app_user のみ、"" = 任意

branches:
  name_pattern: "^[a-z0-9-]{1,32}$"
  max_branches: 50
  lazy_create: true               # 未知のブランチ名で接続すると(認証後に)作る
  lazy_create_max_wait: 20s
  idle_stop_after: 30m
  delete_after_idle: 168h
  reaper_interval: 1m
  operation_retention: 168h
  error_retention: 72h            # error 状態になってからこの期間で削除(0 = 残す)
  default_profile: preview
  profiles:
    preview: { idle_stop_after: 30m, delete_after_idle: 168h }
    ci:      { idle_stop_after: 5m,  delete_after_idle: 1h }
    sandbox: { idle_stop_after: 1h,  delete_after_idle: 720h }

baseline:
  refresh_script: /etc/sashiki/refresh.sh
  refresh_timeout: 1h
  source_dir: ""                  # refresh_script が無ければ *.sql を組み込みローダーで適用
  source_db: ""
  require_masked: true
  require_validated: true
  validate_port: 3999
  masked_sentinel: /run/sashiki/baseline-masked
  keep_last: 3
  retention: 720h                 # Go の duration(d は使えない)。0 = 期間では残さない

hooks:
  dir: /etc/sashiki/hooks
  log_dir: /var/log/sashiki/hooks
  timeout: 10m

auth:
  api_token_env: SASHIKI_API_TOKEN
  api_token_ssm: ""               # 指定すると SSM SecureString から読む(env より優先)
  trust_loopback: true
```

シークレットは書かない。環境変数 or SSM。

-----

## 22. アダプター

### 22-1. GitHub Action

adapter の仕事は「PR #123 → branch `pr-123`」の変換と PR コメント投稿だけ。

```yaml
- uses: rikukadev/sashiki/action@v0.11.0 # x-release-please-version
  with:
    api_url: ${{ vars.SASHIKI_API_URL }}
    token: ${{ secrets.SASHIKI_API_TOKEN }}
    branch: pr-${{ github.event.pull_request.number }}
    profile: preview
    source: '{"type":"github_pr","repository":"${{ github.repository }}","ref":"${{ github.event.pull_request.number }}"}'
    on_close: delete
    comment: "true"   # action.yml は文字列比較なので引用符付き
```

PR open で create(fsx-zfs では常に必須。ebs-zfs は lazy create でも生えるが Action での明示 create を推奨)、synchronize(push)でも create を呼ぶ(既にあれば既存を返すだけ。migration の再適用は利用者が `recreate` を選ぶ)、close で delete。TTL は別途効く。

### 22-2. プレビュー環境への接続情報

proxy なし構成(fsx 直結など): branch ごとに port が違う。Action が API から `{host, port}` を取り、プレビュー環境の env に渡す。
(postgres も #222 で proxy に対応したため、`listen.proxy` を設定すれば MySQL と同じ固定エンドポイント運用ができる。)

```
DB_HOST=shop-sashiki.corp.example.com
DB_PORT=3417
DB_USER=dev
```

proxy あり構成(mysql): `DB_HOST` 固定、`DB_PORT=3306`、branch は `DB_USER=dev@pr-123` で指定(23 章)。

### 22-3. Terraform モジュール(社内 database module contract 互換)

「RDS 互換」ではなく **社内 database module contract 互換**。アプリ側 Terraform は次の出力だけを見る:

```
endpoint / port / username / password_secret_arn / security_group_id
```

sashiki モジュールはこれに加えて `api_url`, `api_token_ssm_path` を出す。入力は Aurora モジュールと揃える(`name`, `vpc_id`, `subnet_ids`, `allowed_sg_ids`, `instance_class`, `allocated_storage`)。

```hcl
module "db" {
  source = "github.com/yourorg/tf-modules//database"
  engine = "sashiki"     # "aurora" | "sashiki"
  name   = "shop"
  vpc_id = module.network.vpc_id
  subnet_ids = module.network.private_subnets
  allowed_sg_ids = [module.preview_env.sg_id]
  instance_class = "r6i.large"
  allocated_storage = 300
}
```

sashiki が作る AWS リソース: EC2、EBS(`prevent_destroy`)、SG、IAM ロール(ebs-zfs なら SSM 読み取りのみ)、Secrets Manager(app_user の PW)、SSM(API トークン)。`route53_zone_id` と `dns_name` を両方指定したときだけ Route53 A レコードも作る。**RDS / Aurora には触らない。** ebs-zfs の日常運用で sashikid は AWS API を呼ばない。

DNS は「VPC 内から解決できて sashiki ホストに向く」なら何でもよい。Route53 を指定しない場合、Terraform の `endpoint` は EC2 の private IP を返す。

-----

## 23. MySQL プロトコルプロキシ

> **改訂(2026-09-06、issue #31 / #51)**: 原文 v2 は「認証中継は成立しない」として proxy を v0.4 に後送りしていたが、
> 実装(ADR-006: バックエンド起点 AuthSwitch 転送)で中継は成立し稼働していた。そのうえで **方式 A(認証終端)へ移行済み**(#51, ADR-007)。
> 以下 23-2 が現行の実装、23-1 は経緯として残す。

### 23-1. 旧方式(ADR-006、経緯)

- 固定エンドポイント `:3306`、branch は `dev@pr-123` 形式の username でルーティング
- 認証はバックエンド起点の AuthSwitch 転送で中継(ADR-006)。正否は backend が判断
- lazy create: 存在しない branch への接続で create → ready まで TCP を保持(`TypicalCreate <= 20s` の backend のみ)
- 既知の課題: **認証前に create が走る**ため、ポートに到達できる相手が任意名で clone + mysqld 起動を積み上げられる(#7 の DoS)。→ 23-2 で解消

### 23-2. 方式 A(認証終端)【現行・実装済み #51 / ADR-007】

sashiki がクライアント認証(`dev@pr-123` のパスワード検証)を**終端**し、バックエンドへは sashiki が保持する credential で接続する。

- `app_user` / `app_pass`(`engine.mysql.app_pass`。Linux の init はランダム生成、Terraform は Secrets Manager 由来)でクライアントを**サーバー側検証**する。合成ハンドシェイクは `caching_sha2_password` を名乗り fast-auth スクランブルを検証、`mysql_native_password` のクライアントは AuthSwitch でフォールバック(ADR-008)
- **認証に成功してから route / lazy create**: 認証前の無償リソース確保(#7 の DoS)を構造的に解消
- backend へは sashiki がクライアントとして接続し直す(caching_sha2 の full-auth は localhost 上で RSA 公開鍵手順。認証はクライアントから不可視)
- TLS 終端は sashiki に一元化。`proxy.tls_cert` / `proxy.tls_key` 指定時に有効(クライアント↔sashiki=TLS、sashiki↔backend=localhost 平文)。SSLRequest を検出して TLS へ切替
- クライアントは普通の MySQL 接続で済む(SNI 方式のような全クライアント TLS+SNI 対応は不要)

### 23-3. proxy が提供する UX

- 固定エンドポイント、branch 指定は `user@branch`
- lazy create(認証後)
- `last_conn_at` を接続で直接更新(engine ポーリングと併用)
- GitHub Action の create ステップが省略可能(ebs-zfs のみ)

-----

## 24. コスト(要約)

- ストレージ費用の差は月 $50 程度(EBS gp3 $60〜80 vs FSx $75〜135、600GB)。全体の中では小さい
- **支配的なのは mysqld のメモリ ＝ EC2 代。** 同時稼働数で決まり、backend に依存しない。30 同時で r6i.2xlarge 月 $480
- だから **idle stop + max_running が最大のコスト対策**。100 branch でも running 5 なら r6i.large で済む
- FSx が効くのは Spot(EC2 代 60〜70% 減)と複数 host。単一 host で収まるうちは EBS

詳細は [COSTS.md](COSTS.md)。

-----

## 25. リポジトリ構成

```
sashiki/
├── cmd/sashikid, cmd/sashiki
├── internal/
│   ├── workspace/     # Branch Manager(lifecycle, reset/recreate, lease)
│   ├── baseline/      # Baseline Manager(build/validate/publish/GC, provenance)
│   ├── storage/       # ebszfs/, fsxzfs/(interface + Capabilities)
│   ├── engine/        # mysql/(Start/StopGracefully/Kill/Ready), postgres/
│   ├── hooks/
│   ├── state/         # SQLite, reconciler
│   ├── ops/           # operations(非同期ジョブ)
│   ├── api/
│   └── proxy/
├── hooks/             # サンプル(on-create.sh.example)。baseline build は examples/baseline-refresh
├── deploy/terraform, deploy/systemd, deploy/orbstack(AppArmor / sudoers は init が生成)
├── action/
├── docs/SPEC.md, DECISIONS.md, COSTS.md
└── README.md
```

アーキテクチャ像:

```
                    Baseline Manager
                           │
Storage ──────── Workspace / Branch Manager ──────── Engine
                           │
             ┌─────────────┼──────────────┐
           Hooks       Lease / TTL     Capacity
             └──────── State / Reconciler ─┘
                           │
                          API
        ┌──────────────────┼──────────────────┐
  GitHub Action          QA UI               CI
```

-----

## 26. ロードマップ

|版       |範囲                                                                                                                                                                                                                                |受け入れ条件                                                                                                                                                      |
|--------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------|
|**v0.1**|ebs-zfs、branch lifecycle(create / reset / recreate / delete / retry)、REST API + operations、SQLite、CLI、hooks、baseline lifecycle(build / validate / publish / set / GC、手動起動)、memory / storage / port admission、reconciliation、doctor|`sashiki init` → `baseline build/validate/publish` → `create pr-1` → 接続 → `reset` → `recreate` → `delete`。max_running 超過で `limit_reached`。sashikid 再起動後に state が一致|
|**v0.2**|idle stop / wake、TTL / lease、profile、GitHub Action、プレビュー環境連携(port 渡し)                                                                                                                                                             |30 分放置で sleeping、`wake` で復帰。PR open/close で create/delete。7 日で自動削除                                                                                          |
|**v0.3**|baseline automation(merge / nightly トリガー、mask validation)、Terraform モジュール、observability、sashiki-root-helper、AppArmor プロファイル生成                                                                                                     |`terraform apply` だけで sashikid が動く。夜間 refresh が回る。sudoers が zfs 全体を許可していない                                                                                  |
|**v0.4**|proxy 方式 A(認証終端)への移行、username routing、認証後 lazy create、Web UI 拡充                                                                                                                                                                   |`mysql -udev@pr-2 -h <host>` で存在しない branch が(認証後に)生えて繋がる                                                                                                    |
|**v1.x**|fsx-zfs、multi-host、replaceable compute、local-zfs profile、Postgres、team quota                                                                                                                                                      |backend を切り替えてもコアと CLI が変わらない                                                                                                                               |

> 注: 実装は歴史的経緯により一部を前倒し済み(proxy 中継・lazy create・fsx・postgres は搭載済み)。
> 本表は「機能の完成度をどの順で仕様水準に引き上げるか」の指針として読む。進捗は issue #48 参照。

FSx を「完成版」とは扱わない。EBS-ZFS 版を中心に据えて実運用し、multi-host / Spot / host 使い捨て / RAM 限界のどれかが出た時点で fsx-zfs を本命に格上げする。

-----

## 27. テスト戦略

- ユニット: `storage` / `engine` をモック。状態遷移、admission、port allocator、reconciler、reset/recreate の分岐を検証。通常の GitHub ランナー
- engine/mysql: `testcontainers-go` で `mysql:8.0` を起動し Start/StopGracefully/Kill/Ready を実接続で確認
- E2E: ランナー上の実 ZFS(`truncate -s 8G /tmp/zpool.img && zpool create tpool /tmp/zpool.img`)。主要シナリオ + OOM シナリオ(`max_running` を 2 にして 3 個目が拒否される)+ reconciliation シナリオ(sashikid を kill して再起動)をコード化
- baseline validate の hook 失敗で publish が阻止されることのテスト

-----

## 28. README の説明方針

冒頭(入口は PR):

> **PR を作ると、本番に近いサイズの独立した MySQL DB が数秒で生える。壊しても `sashiki reset` で戻せる。main が進んだら `sashiki recreate` で追従できる。**

その後に広げる:

> sashiki provides disposable database workspaces for previews, CI, migration testing, debugging and development sandboxes. Branch from an immutable, masked baseline in seconds; pay only while a branch is running.

Git との対応表を README に置く(main HEAD = current baseline / `git branch` = create / `git reset --hard` = reset / `git rebase main` = recreate / `git branch -D` = delete)。

FAQ に「向かない用途(10-3)」「本番では使えない(開発専用)」を置く。

-----

## 29. 実装前の決定事項チェックリスト

設計レビューの Must / Strongly Recommended をこの版でどう扱ったか。

|# |項目                                    |反映箇所                      |
|--|--------------------------------------|--------------------------|
|1 |proxy 認証方式の再設計                        |23 章(#31 で方式 A 採用、#51 で実装)|
|2 |`@init` lifecycle の統一                 |13-2                      |
|3 |port allocator を SQLite 基準に           |14-3、19 章                 |
|4 |auto.cnf / server_uuid                |12-3 build                |
|5 |memory admission                      |14-1                      |
|6 |baseline immutable                    |12 章                      |
|7 |reset / recreate 分離                   |13-3                      |
|8 |branch provenance                     |11-2、19 章                 |
|9 |PII masking を invariant に             |10-4、12-6、`require_masked`|
|10|EBS-ZFS を default に                   |10-1、15-4、26 章            |
|11|storage quota / watermark             |14-2                      |
|12|build / validate / publish            |12-3                      |
|13|baseline GC                           |12-5                      |
|14|hook retry                            |13-7                      |
|15|reconciliation                        |20-1                      |
|16|doctor                                |20-2                      |
|17|profile / lease                       |11-3                      |
|18|async operation API                   |17 章                      |
|19|schema / data revision 分離             |12-2                      |
|20|lifecycle / engine state 分離の余地        |11-1、19 章 `engine_state`  |
|— |CAP_NET_BIND_SERVICE 削除、metrics 二重定義解消|20-3、17 章                 |
|— |「RDS 互換」→「database module contract 互換」|22-3                      |
|— |向かない用途の明記                             |10-3                      |

ここに書いていないことは実装者が決めてよいが、決めた内容は [DECISIONS.md](DECISIONS.md) に ADR として残す。
