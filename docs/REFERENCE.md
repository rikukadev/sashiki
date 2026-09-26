# リファレンス

実装されているインターフェースの一覧(#307)。設計の背景は [SPEC.md](SPEC.md)、
判断の記録は [DECISIONS.md](DECISIONS.md)、使い方の導入は [README](../README.md) を見る。

- [CLI](#cli)
- [終了コード](#終了コード)
- [HTTP API](#http-api)
- [hooks に渡る環境変数](#hooks-に渡る環境変数)
- [GitHub Action](#github-action)
- [Terraform モジュール](#terraform-モジュール)
- [config](#config)

---

## CLI

`sashiki` は API(`SASHIKI_API_URL`、既定 `http://127.0.0.1:8080`)を叩く。
`baseline import` / `export` / `import-stream` と `token` と `init` だけはホスト上で
config と state.db を直接触る(sashikid 経由ではない)。

### ブランチ

| コマンド | 何をするか |
|---|---|
| `create <name>` | 作る。`--wait`(既定)/ `--no-wait` / `--exist-ok`(既にあれば既存を返す)/ `--profile P` / `--ttl D` / `--port N` / `--baseline <snapshot>` / `--owner O` / `--purpose P` / `--source <json>` |
| `list` / `show <name>` | 一覧 / 詳細。`show` は `stale`(origin が current baseline より古い)と `baseline:`(promote 元)も出す |
| `env <name>` | `DB_HOST=` / `DB_PORT=` / `DB_USER=` を出す(dotenv)。`--prefix P` で接頭辞を変える。パスワードと内部ポートは出さない |
| `connect <name>` | engine に合わせたクライアント(`mysql` / `psql`)を exec する。パスワードは `SASHIKI_DB_PASSWORD`(無ければクライアントが尋ねる)、接続先は API の host(`SASHIKI_DB_HOST` で上書き可)。Postgres は `SASHIKI_DB_NAME`(既定 `postgres`)に繋ぐ |
| `reset <name>` | 作成時点(`@init`)に戻す |
| `recreate <name>` | 現在の current baseline から作り直す |
| `retry <name>` | `error` のブランチで失敗した操作をやり直す |
| `delete <name>` | 消す |
| `sleep <name>` / `wake <name>` | 手動で停止 / 起床(同期) |
| `lease renew <name> --for <dur> [--json]` | 絶対期限を延ばす(例 `7d`) |
| `hooks run <name> <event>` | hook だけを手で流す |

変更操作は既定で完了まで待つ。`--no-wait` で operation id だけ返し、`--timeout <dur>` /
`--interval <dur>` で待ち方を変える。ほとんどのコマンドが `--json` を取る。

### baseline

| コマンド | 何をするか |
|---|---|
| `baseline import --from <src>` | 初回の baseline を作る。Linux(ZFS)は root で実行する(`sudo`)。macOS ネイティブは不要。`<src>` は SQL ファイル / ディレクトリ(`*.sql` を並列投入)/ `s3://…` / `-`(標準入力)。Postgres は `pg_dump` のカスタム形式・ディレクトリ形式も自動判定。`--db <name>` / `--import-cnf <my.cnf>` / `--threads N` / `--config <path>` |
| `baseline list [--json]` | 登録済み snapshot と current |
| `baseline refresh` | `refresh_script` か `source_dir` で作り直す(開始だけ返す) |
| `baseline build` / `validate <snap>` / `publish <snap>` / `delete <snap>` | 段階ごとに実行する |
| `baseline promote <branch> [--masked] [--skip-validate]` | ブランチの現在の datadir を次の baseline に昇格する。`require_masked` なら `--masked` の宣言が要る |
| `baseline set <snap>` | current を切り替える(ロールバック) |
| `baseline gc [--keep-last N] [--dry-run]` | 古い snapshot を掃除する |
| `baseline export --to <dst> [--snapshot <tag>]` | `zfs send` で書き出す(ファイル / `s3://…` / `-`) |
| `baseline import-stream --from <src> [--force]` | 書き出したものを `zfs recv` して current にする。**`--force` は既存の base を `zfs destroy -r` する**(baseline snapshot が全部消える) |

### 運用

| コマンド | 何をするか |
|---|---|
| `init` | ホストを構成する。`--pool <p>` / `--device <dev>` / `--engine mysql\|postgres` / `--app-pass <pw>`(省略時: Linux はランダム生成、darwin は `dev`。config が既にあると無視される)/ `--platform darwin`(既定は実行中の OS)/ `--root <dir>`(darwin)/ `--skip-packages`(apt を触らない。Linux の導入は `mysql-server-8.0` 固定で、mysqld が既にあれば自動で飛ばす。8.4 / 26.7 は先に入れておく)/ `--yes`(`-y`) |
| `token create --name <n> [--scope branches\|admin]` / `token list` / `token revoke <n>` | API トークン。既定 scope は `branches`。`--config <path>` で config を指定 |
| `op list` / `op show <id>` / `op wait <id>` | operation(直近 50 件) |
| `capacity` / `doctor` | 容量とヘルスチェック(読み取りのみ)。`doctor` は問題があると終了コード 1 |
| `gc --orphans` | state.db に無い dataset を消す |
| `drain [--json]` | 全ブランチを安全に停止する(メンテ前) |
| `version` | 版を出す |

環境変数: `SASHIKI_API_URL` / `SASHIKI_API_TOKEN`(または `~/.config/sashiki/token`)/
`SASHIKI_CONFIG`(config の場所を上書き)/ `SASHIKI_DB_HOST` / `SASHIKI_DB_PASSWORD` / `SASHIKI_DB_NAME`(`connect` 用)。

未知のフラグ・余分な引数は全コマンドでエラーにする(終了コード 2)。`--timeout` / `--interval` の値が
読めないときも黙って既定に戻さずエラーにする。create 専用のフラグ(`--port` / `--owner` / `--ttl` …)は
他のブランチ操作では受けない。`token create` の `--scope` は `branches` | `admin` 以外(値無し・タイポ)を
エラーにする。引数の不足・不正(`--for` / `--prefix` の値無し、`--threads` の不正、`export` の `--to` 無し、
`init --engine` の不正)も 2 で終了する。

`app_pass` は非 ASCII でもよい。MySQL / PostgreSQL の proxy はどちらも UTF-8 のバイト列で検証し、
PostgreSQL(SCRAM-SHA-256)は RFC 4013 の SASLprep で正規化してから鍵を導出する(PostgreSQL 本体と同じ。
正規化に失敗する入力は元の文字列のまま)。

`sashikid` は `--config <path>`(`-config` も可)と `--help` だけを取る。`log_format: json` のとき、
operation 内のログには `operation_id` / `operation_type` / `branch` が付く(reaper 等の operation 外は
`branch` のみ、baseline refresh は `operation_type=baseline-refresh` + `baseline_tag`。SPEC 20-5)。

`sashiki-root-helper`(Linux の deb / tar.gz に同梱)は sashikid が `sudo -n` 経由で呼ぶ root 操作の
allowlist ヘルパーで、手で使うものではない。config の `root_helper` がパスを指し、`sashiki init` が
`/etc/sudoers.d/sashiki`(helper 1 行)と `/etc/sashiki/root-helper.yaml` を生成する(ADR-010)。

## 終了コード

| コード | 意味 |
|---|---|
| 0 | 成功 |
| 1 | エラー(API エラー、通信失敗、operation の失敗) |
| 2 | 使い方が違う(未知のコマンド / フラグ、引数不足) |
| 3 | 対象が無い(404) |
| 4 | 既にある / 競合(409。`create` の重複、実行中の操作がある) |
| 5 | 容量不足(507)。`create` を受け付ける前の同期判定(port / 上限)で返る。operation の中で判定される admission(`max_running` / メモリ / storage watermark)は operation の失敗として **1** になり、メッセージに `limit reached: max_running …` が出る |
| 6 | `--wait` / `op wait` が**期限までに終わらなかった**(API エラーや通信断はそれぞれのコードになる) |

## HTTP API

`{name}` はブランチ名。変更操作は 202 + `operation_id`(ヘッダ `Sashiki-Operation-Id`)を返し、
`GET /v1/operations/{id}` で追う。認証は README の[認証](../README.md#認証トークン)を参照
(`GET /v1/healthz` と Web UI の HTML だけ認証の外)。

| メソッド / パス | 応答 | 備考 |
|---|---|---|
| `GET /v1/branches` | 200 | 一覧(ページングなし) |
| `POST /v1/branches` | 202 / 200 / 409 | body `{name, port?, profile?, owner?, purpose?, source?, ttl?, baseline?}`。`?exist_ok=true` なら既存を 200 で返す |
| `GET /v1/branches/{name}` | 200 / 404 | |
| `POST /v1/branches/{name}/reset` \| `/recreate` \| `/retry` | 202 | 404 / 412(promote 元)は同期で返す。409 = 実行中の操作あり |
| `DELETE /v1/branches/{name}` | 202 | 404 / 412 は同期で返す |
| `POST /v1/branches/{name}/sleep` \| `/wake` | 200 | 同期 |
| `POST /v1/branches/{name}/lease` | 200 | body `{for: "7d"}` |
| `POST /v1/branches/{name}/hooks/{event}` | 200 | **admin scope**。hooks dir の実行ファイルを手で流す |
| `GET /v1/branches/{name}/schema` | 200 | **admin scope**。データブラウザ |
| `POST /v1/branches/{name}/query` | 200 | **admin scope**。body `{sql}`。10 秒 / 200 行 / セル 64KB で打ち切り |
| `GET /v1/baseline` | 200 | current / snapshot 一覧 / refresh 状態 |
| `GET /v1/baselines` | 200 | 台帳(provenance 付き) |
| `POST /v1/baseline/refresh` | 202 | **admin**。`{status, tag}` のみで `operation_id` は返さない |
| `POST /v1/baseline/build` \| `/validate` | 202 | **admin** |
| `POST /v1/baseline/publish` \| `/set` \| `/delete` \| `/promote` \| `/gc` | 200 | **admin**。promote は body `{branch, masked?, skip_validate?}` |
| `GET /v1/capacity` \| `/v1/doctor` | 200 | |
| `POST /v1/gc/orphans` \| `/v1/drain` | 200 | **admin** |
| `GET /v1/operations` \| `/v1/operations/{id}` | 200 / 404 | 一覧は直近 50 件固定 |
| `GET /v1/healthz` | 200 | 認証不要 |
| `GET /` | 200 | Web UI(HTML。データは API を叩いて取る) |

エラーは `{"error": {"code": "...", "message": "..."}}`。主なコード:
`invalid_name` `invalid_request` `query_error`(400)/ `unauthorized`(401)/
`insufficient_scope` `cross_origin_denied` `host_mismatch`(403)/
`branch_not_found` `baseline_not_found`(404)/ `branch_exists` `operation_in_progress`(409。baseline refresh の重複実行も)/
`precondition_failed`(412)/ `invalid_content_type`(415)/ `unsupported_engine`(501)/
`limit_reached`(507)/ `storage_error`(500)。

## hooks に渡る環境変数

hooks dir(`hooks.dir`)に置いた実行ファイル名がイベント名になる。
イベントは `on-create` / `on-recreate` / `on-reset` / `on-delete` / `on-baseline-validate` の 5 つ。

| 変数 | 中身 |
|---|---|
| `SASHIKI_EVENT` | イベント名(baseline build スクリプトでは `baseline-build`) |
| `SASHIKI_BRANCH` | ブランチ名 |
| `SASHIKI_PORT` | ブランチの DB ポート |
| `SASHIKI_SOCKET` | UNIX socket のパス(`/tmp/mysql-<branch>.sock`。mysql の systemd モードだけ実在し、process モード / postgres では使えない) |
| `SASHIKI_DATADIR` | datadir |
| `SASHIKI_ENGINE` | `mysql` / `postgres` |
| `SASHIKI_ADMIN_USER` | 管理ユーザー名 |
| `SASHIKI_ORIGIN_SNAPSHOT` | 作成元の snapshot |
| `SASHIKI_STATE_DIR` | `/var/lib/sashiki/branches/<name>`(sashiki は作らない) |
| `SASHIKI_OWNER` / `SASHIKI_PURPOSE` / `SASHIKI_PROFILE` | create 時の provenance |
| `SASHIKI_SOURCE_JSON` | `--source` の JSON(指定時のみ) |
| `SASHIKI_BASELINE_SCHEMA_REVISION` | origin baseline の schema revision(あれば) |
| `SASHIKI_BASELINE_TAG` | baseline build 中のタグ |

hook は sashikid と同じユーザー・環境で走る(API トークンだけは渡さない)。
タイムアウトは `hooks.timeout`(既定 10 分)。ログは `<hooks.log_dir>/<branch>-<event>-<時刻>.log` に
`hooks.log_retention`(既定 30 日、負の値で削除しない)の間残る。`on-reset` / `on-delete` の失敗は記録して続行する。

## GitHub Action

`uses: rikukadev/sashiki/action@<tag>`。

| 入力 | 既定 | 中身 |
|---|---|---|
| `transport` | `api` | `api` \| `ssm`(SSM Run Command でインスタンス上の CLI を叩く) |
| `instance_id` | — | `transport: ssm` のときの EC2 instance id |
| `api_url` | — | `transport: api` のとき必須 |
| `token` | — | API トークン |
| `action` | — | `create` \| `delete` \| `reset`。省略時は PR イベントから決める(ラベル駆動で使う) |
| `branch` | `pr-<番号>` | ブランチ名 |
| `profile` | サーバー既定 | lifecycle profile |
| `source` | 自動生成 | provenance の JSON |
| `on_close` | `delete` | `delete` \| `keep`(TTL 回収に任せる) |
| `comment` | `false` | PR に接続先をコメントする(`pull-requests: write` が要る) |
| `github_token` | `github.token` | コメント投稿用 |

出力は `host` / `port` / `user`。PR の open / reopen / synchronize で create(冪等)、
close で delete(既に無ければ成功扱い)、それ以外のイベントは何もしない。

## Terraform モジュール

`source = "github.com/rikukadev/sashiki//deploy/terraform?ref=<tag>"`。

| 変数 | 既定 | 中身 |
|---|---|---|
| `name` | — | リソース名のプレフィックス |
| `vpc_id` / `subnet_ids` | — | 配置先。EC2 は `subnet_ids[0]` |
| `allowed_sg_ids` | `[]` | 3306 / 8080 を許可する SG(空なら何も開かない) |
| `instance_class` | `m6i.large` | 変更前に `sashiki drain` |
| `allocated_storage` | `100` | データ EBS(GiB)。ZFS プールになる |
| `root_volume_size` | `30` | root EBS(GiB) |
| `ebs_type` | `gp3` | データ EBS の種別 |
| `data_volume_encrypted` | `true` | データ EBS を暗号化する(#341)。既存の非暗号化 volume は移行まで `false` |
| `kms_key_id` | `""` | データ EBS の KMS key ARN(空なら `aws/ebs`) |
| `data_device_name` | `/dev/xvdf` | デバイス名(Nitro では nvme に見えるので user-data が解決する) |
| `ami_id` | 最新の Ubuntu 24.04 | 変更しても `ignore_changes` で作り直されない |
| `route53_zone_id` / `dns_name` | `""` | 両方指定したときだけ A レコードを作る |
| `key_name` | `""` | SSH 鍵(SSM があれば不要) |
| `proxy_user` | `dev` | アプリ用ユーザー名 |
| `sashiki_ref` | モジュール同梱の `VERSION` | install.sh の取得元 |
| `tags` | `{}` | 追加タグ |

出力は `endpoint` / `reader_endpoint`(同値)/ `port` / `username` / `branch_user` /
`password_secret_arn` / `api_url` / `api_token_ssm_path` / `security_group_id` / `instance_id`。

## config

キーの一覧と既定値は [SPEC.md の 21 章](SPEC.md#21-設定ファイル)にある(そのサンプルが
`config.Load` で読めることをテストしている)。運用上の注意:

- **未知のキーは起動時にエラー**になる(綴り間違いを黙って無視しない)
- サイズは `256M` / `256MB` / `20GiB` のいずれも可(2 進)。解釈できない値はエラー
- watermark は 0〜1 の比率。`proxy.tls_cert` と `proxy.tls_key` は両方指定する
- `SIGHUP` による再読み込みは無い。変更したら sashikid を再起動する
- `app_pass` を変えたら baseline 側のユーザー/ロールのパスワードも変える
