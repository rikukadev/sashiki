# Changelog

本プロジェクトのバージョニングは [Semantic Versioning](https://semver.org/lang/ja/) に従う。
v0.x の間は API / config が安定しておらず、マイナー版で破壊的変更があり得る。

> v0.6.0 以降のエントリは [release-please](https://github.com/googleapis/release-please) が
> [Conventional Commits](https://www.conventionalcommits.org/) から自動生成する(手動編集不要)。
> リリース手順は [docs/RELEASING.md](docs/RELEASING.md) を参照。v0.5.0 までは手書き。

## [0.12.0](https://github.com/rikukadev/sashiki/compare/v0.11.2...v0.12.0) (2026-09-24)


### Features

* **fsx:** baseline GC / 使用量 / quota / watermark を FSx バックエンドでも動かす ([#345](https://github.com/rikukadev/sashiki/issues/345)) ([4f04809](https://github.com/rikukadev/sashiki/commit/4f0480907f4fb5bf5680d6e66105e8d47d387921)), closes [#278](https://github.com/rikukadev/sashiki/issues/278)
* **logs:** operation 途中のログにも operation_id / operation_type / branch を構造化して付ける ([#348](https://github.com/rikukadev/sashiki/issues/348)) ([d1b53df](https://github.com/rikukadev/sashiki/commit/d1b53df2ee7ba36221885f7c9c6d51f134c27316)), closes [#284](https://github.com/rikukadev/sashiki/issues/284)
* **pgproxy:** SCRAM-SHA-256 のパスワードを SASLprep(RFC 4013)で正規化する ([#350](https://github.com/rikukadev/sashiki/issues/350)) ([390f064](https://github.com/rikukadev/sashiki/commit/390f064bd40889f4f7af54342099f39d654befda))
* **security:** sashiki-root-helper で root 操作を allowlist に閉じ込め、sudoers を helper 1 行にする ([#346](https://github.com/rikukadev/sashiki/issues/346)) ([23e2ba1](https://github.com/rikukadev/sashiki/commit/23e2ba1517f2dea1a2d5266d32bbddf5c2f5e80c))


### Documentation

* **examples:** baseline の nightly / merge トリガー自動更新テンプレートと runbook を追加する ([#349](https://github.com/rikukadev/sashiki/issues/349)) ([2241d18](https://github.com/rikukadev/sashiki/commit/2241d18aa8520ae050aaad54d08c64a5340fbecc)), closes [#277](https://github.com/rikukadev/sashiki/issues/277)
* **proxy:** 合成ハンドシェイクの版を固定する理由を ADR-009 に書き、26.7 の版判定テストと init のパッケージ方針を足す ([#344](https://github.com/rikukadev/sashiki/issues/344)) ([26496bf](https://github.com/rikukadev/sashiki/commit/26496bf286b07edf18767d5478c48208d142c3bc)), closes [#285](https://github.com/rikukadev/sashiki/issues/285)
* テスト戦略(SPEC 27)を実態に合わせ、max_running の実 ZFS E2E を足し、v1.x roadmap の未着手項目を整理する ([#351](https://github.com/rikukadev/sashiki/issues/351)) ([ffd8873](https://github.com/rikukadev/sashiki/commit/ffd8873c5107cb933fd905610145f3bd84630235))

## [0.11.2](https://github.com/rikukadev/sashiki/compare/v0.11.1...v0.11.2) (2026-09-24)


### Bug Fixes

* **terraform:** compute置換でstate.dbを保持する ([#329](https://github.com/rikukadev/sashiki/issues/329)) ([4a08dc1](https://github.com/rikukadev/sashiki/commit/4a08dc1892e78fd88beee76c5087ecb0dbcbf9fe))
* **workspace:** wake で last_conn_at を進め、起動中のブランチを reaper が止めないようにする ([#338](https://github.com/rikukadev/sashiki/issues/338)) ([ce3ac97](https://github.com/rikukadev/sashiki/commit/ce3ac973d8eee52c36cc4a42ca27e10d99c2f80e))


### Documentation

* **config:** postgres でも idle_stop_after / delete_after_idle が効くことを config テンプレートに書く ([#337](https://github.com/rikukadev/sashiki/issues/337)) ([8ef4809](https://github.com/rikukadev/sashiki/commit/8ef48091107ff68103b83321f41c8a55f419264e))

## [0.11.1](https://github.com/rikukadev/sashiki/compare/v0.11.0...v0.11.1) (2026-09-23)


### Bug Fixes

* **init:** --pool 省略時は既存 config の pool を使い、zpool の確認を AppArmor / sudoers より先にする ([#320](https://github.com/rikukadev/sashiki/issues/320)) ([#332](https://github.com/rikukadev/sashiki/issues/332)) ([07302d6](https://github.com/rikukadev/sashiki/commit/07302d622e47c593bad6b857d3375077a2a6e981))
* **proxy:** 認証試行のスロットを検証の間だけ握り、同一 IP の同時セッション上限を無くす ([#318](https://github.com/rikukadev/sashiki/issues/318)) ([#330](https://github.com/rikukadev/sashiki/issues/330)) ([d99c674](https://github.com/rikukadev/sashiki/commit/d99c6740ba11e0403adb8f399149fdb95e3d50a0))
* README 再監査(audit 2)の Medium を修正する ([#335](https://github.com/rikukadev/sashiki/issues/335)) ([b3868b9](https://github.com/rikukadev/sashiki/commit/b3868b92aad531c3622dc3f27612453e0a7a5f51))
* **workspace:** retry の create 経路を lock / promote-guard に通し、中断された recreate と wake の失敗を復旧できるようにする ([#321](https://github.com/rikukadev/sashiki/issues/321), [#322](https://github.com/rikukadev/sashiki/issues/322)) ([#333](https://github.com/rikukadev/sashiki/issues/333)) ([733565d](https://github.com/rikukadev/sashiki/commit/733565db9051a6eb71a563068908b20c37a583de))
* **workspace:** volume が無い error ブランチを API と reaper で削除できるようにする ([#319](https://github.com/rikukadev/sashiki/issues/319)) ([#331](https://github.com/rikukadev/sashiki/issues/331)) ([6849c1e](https://github.com/rikukadev/sashiki/commit/6849c1e0e83143d406676f518c3c7f4e644b5bff))


### Documentation

* ドキュメント間の食い違いを実装に合わせる(REFERENCE / SPEC / terraform README / LOCAL-DEV / 古いコメント) ([#336](https://github.com/rikukadev/sashiki/issues/336)) ([0b5fa45](https://github.com/rikukadev/sashiki/commit/0b5fa45962d0572202e36df24958216865f60e1f)), closes [#328](https://github.com/rikukadev/sashiki/issues/328)

## [0.11.0](https://github.com/rikukadev/sashiki/compare/v0.10.0...v0.11.0) (2026-09-22)


### Bug Fixes

* **cli:** README監査で判明したLow不具合を修正する ([45e6f3f](https://github.com/rikukadev/sashiki/commit/45e6f3f506f373c9b1d0be0224e72dab986443bc))
* README監査で判明したCritical不具合を修正する ([#309](https://github.com/rikukadev/sashiki/issues/309)) ([e563948](https://github.com/rikukadev/sashiki/commit/e563948a45ae8d2ee723210085a3d4149612fa4d))
* README監査で判明したHigh不具合を修正する ([#310](https://github.com/rikukadev/sashiki/issues/310)) ([ae6ddbd](https://github.com/rikukadev/sashiki/commit/ae6ddbd343f7a977036b0a787b555a6f32d836a5))
* README監査で判明したMedium不具合を修正する ([#312](https://github.com/rikukadev/sashiki/issues/312)) ([a87ec20](https://github.com/rikukadev/sashiki/commit/a87ec20fb8e11ea33998bb1a7e539322cfef626a))
* **terraform:** README監査の残件を修正する ([#317](https://github.com/rikukadev/sashiki/issues/317)) ([3de32ff](https://github.com/rikukadev/sashiki/commit/3de32ff2d362c57de8d0b0f064d843f57104c60c))


### Chores

* release 0.11.0 ([b4f8bd2](https://github.com/rikukadev/sashiki/commit/b4f8bd283060c233d33b81adcad6d9aa270b2c46))

## [0.10.0](https://github.com/rikukadev/sashiki/compare/v0.9.2...v0.10.0) (2026-09-13)


### Features

* **cli:** create --exist-ok と env サブコマンドを足す ([#266](https://github.com/rikukadev/sashiki/issues/266)) ([a9ec9a6](https://github.com/rikukadev/sashiki/commit/a9ec9a6ed286ec3c20806029a7c901f259139996))


### Bug Fixes

* **e2e:** 直接接続の検証が、張る前にブランチが寝て落ちるのを直す ([#269](https://github.com/rikukadev/sashiki/issues/269)) ([ec9aeee](https://github.com/rikukadev/sashiki/commit/ec9aeeec8b24a443786beb3f3ede61eb6b21374f))


### Documentation

* **releasing:** Actions の権限を絞り戻さないよう明記する ([#268](https://github.com/rikukadev/sashiki/issues/268)) ([ed86a80](https://github.com/rikukadev/sashiki/commit/ed86a8039d248495ab108812fa1ec5e39f433835))

## [0.9.2](https://github.com/rikukadev/sashiki/compare/v0.9.1...v0.9.2) (2026-09-12)


### Bug Fixes

* **action:** transport=ssm で複数行スクリプトが尻切れで届くのを直す ([#263](https://github.com/rikukadev/sashiki/issues/263)) ([15ebcb3](https://github.com/rikukadev/sashiki/commit/15ebcb3601d80c03ce59b550ff98c6e986e839ec))

## [0.9.1](https://github.com/rikukadev/sashiki/compare/v0.9.0...v0.9.1) (2026-09-12)


### Bug Fixes

* **api:** 接続情報の port を proxy のものに揃える ([#261](https://github.com/rikukadev/sashiki/issues/261)) ([8a7e6b0](https://github.com/rikukadev/sashiki/commit/8a7e6b0aa9aec856ce7b5f747ce5e1eac8556119))

## [0.9.0](https://github.com/rikukadev/sashiki/compare/v0.8.0...v0.9.0) (2026-09-11)


### Documentation

* correct the proxy auth note and record MySQL 26.7 verification ([#254](https://github.com/rikukadev/sashiki/issues/254)) ([4107ce3](https://github.com/rikukadev/sashiki/commit/4107ce34e88373561a3d646690209ec1647aaaa3))


### Chores

* release 0.9.0 for the module path change ([#258](https://github.com/rikukadev/sashiki/issues/258)) ([c652a78](https://github.com/rikukadev/sashiki/commit/c652a78754d7ad4d89cf146bc9a407582de74022))

## [0.8.0](https://github.com/rikukadev/sashiki/compare/v0.7.0...v0.8.0) (2026-09-11)


### Features

* **action:** add transport: ssm and an explicit action input ([#244](https://github.com/rikukadev/sashiki/issues/244)) ([#250](https://github.com/rikukadev/sashiki/issues/250)) ([f03a484](https://github.com/rikukadev/sashiki/commit/f03a484c3cf8e430893c0ce658bd419dfabc2c90))
* **api:** PostgreSQL support for the data browser ([#228](https://github.com/rikukadev/sashiki/issues/228)) ([#252](https://github.com/rikukadev/sashiki/issues/252)) ([ba0de0a](https://github.com/rikukadev/sashiki/commit/ba0de0a6dc4601e5a194b75afeb9777c0d326974))
* **baseline:** built-in refresh loader for Postgres ([#226](https://github.com/rikukadev/sashiki/issues/226)) ([#241](https://github.com/rikukadev/sashiki/issues/241)) ([21f0440](https://github.com/rikukadev/sashiki/commit/21f044088c3f18f61049ab2264d4f71a87e61c00))
* **baseline:** stream export/import via zfs send-recv, and S3/stdin dumps ([#243](https://github.com/rikukadev/sashiki/issues/243), [#242](https://github.com/rikukadev/sashiki/issues/242)) ([#249](https://github.com/rikukadev/sashiki/issues/249)) ([0ef573f](https://github.com/rikukadev/sashiki/commit/0ef573f3d4c4e6ad8071119541b93288372e7ad5))
* **init:** sashiki init --platform darwin --engine postgres ([#238](https://github.com/rikukadev/sashiki/issues/238)) ([#253](https://github.com/rikukadev/sashiki/issues/253)) ([36ff11c](https://github.com/rikukadev/sashiki/commit/36ff11c8c9c752ba85e54bb2598f7d8262ad0a73))
* **pgproxy:** relay CancelRequest to the right backend ([#234](https://github.com/rikukadev/sashiki/issues/234)) ([#251](https://github.com/rikukadev/sashiki/issues/251)) ([c164c2c](https://github.com/rikukadev/sashiki/commit/c164c2c1bf533faaaaa433029fb6f5d6bd97f1b1))


### Bug Fixes

* **init:** reuse an existing zpool on instance replacement; add mysqld TimeoutStopSec ([#248](https://github.com/rikukadev/sashiki/issues/248)) ([8badeb3](https://github.com/rikukadev/sashiki/commit/8badeb36a6e8ce7a30f5ed7dd38f50d29cb0315e)), closes [#245](https://github.com/rikukadev/sashiki/issues/245) [#246](https://github.com/rikukadev/sashiki/issues/246)

## [0.7.0](https://github.com/rikukadev/sashiki/compare/v0.6.0...v0.7.0) (2026-09-09)


### Features

* **engine/postgres:** process mode and CoW-backend baseline import ([#227](https://github.com/rikukadev/sashiki/issues/227)) ([#239](https://github.com/rikukadev/sashiki/issues/239)) ([012fb04](https://github.com/rikukadev/sashiki/commit/012fb04c1b2d467b0c8e278f6a2b8b09dc73c1a4))

## [0.6.0](https://github.com/rikukadev/sashiki/compare/v0.5.2...v0.6.0) (2026-09-08)


### Features

* **baseline:** Postgres baseline import via initdb/psql/pg_restore ([#223](https://github.com/rikukadev/sashiki/issues/223)) ([#236](https://github.com/rikukadev/sashiki/issues/236)) ([67298ac](https://github.com/rikukadev/sashiki/commit/67298ac1b1155f890527f5646583388aea7f5403))
* **config:** expand PostgresEngine and make engine settings accessor-based ([#225](https://github.com/rikukadev/sashiki/issues/225)) ([#232](https://github.com/rikukadev/sashiki/issues/232)) ([863c6cd](https://github.com/rikukadev/sashiki/commit/863c6cdb262e8f7a05b140b8463e3e58bb6c2518))
* **init:** sashiki init --engine postgres ([#224](https://github.com/rikukadev/sashiki/issues/224)) ([#237](https://github.com/rikukadev/sashiki/issues/237)) ([4b345d4](https://github.com/rikukadev/sashiki/commit/4b345d4425dda4f585f490dd7d6be6896f9b74d8))
* **pgproxy:** Postgres wire proxy with SCRAM auth termination and lazy create ([#222](https://github.com/rikukadev/sashiki/issues/222)) ([#235](https://github.com/rikukadev/sashiki/issues/235)) ([a0829cc](https://github.com/rikukadev/sashiki/commit/a0829ccac86f0cd9e801bf78b0611931894644e4))

## [0.5.2](https://github.com/rikukadev/sashiki/compare/v0.5.1...v0.5.2) (2026-09-08)


### Bug Fixes

* **engine:** don't pick caching_sha2 for MariaDB backends ([#219](https://github.com/rikukadev/sashiki/issues/219)) ([ece7b38](https://github.com/rikukadev/sashiki/commit/ece7b38a55588f6fc93b5db6032890805c2a0bb4))

## [0.5.1](https://github.com/rikukadev/sashiki/compare/v0.5.0...v0.5.1) (2026-09-08)


### Bug Fixes

* **engine/proxy:** support MySQL 8.4/9.x via caching_sha2 for proxy-&gt;backend auth ([#197](https://github.com/rikukadev/sashiki/issues/197) follow-up) ([#209](https://github.com/rikukadev/sashiki/issues/209)) ([c94de34](https://github.com/rikukadev/sashiki/commit/c94de3498dbbf726e0010a7e22b0e9fa9548fa58))
* **engine:** pick app_user auth plugin by backend version (MySQL 5.7..9.x) ([#211](https://github.com/rikukadev/sashiki/issues/211)) ([23bb406](https://github.com/rikukadev/sashiki/commit/23bb4069c9222636dd8ba1904b7ed38918597341))
* **security:** bump Go toolchain to 1.26.6 to clear stdlib TLS/x509 advisories ([#212](https://github.com/rikukadev/sashiki/issues/212)) ([02550ad](https://github.com/rikukadev/sashiki/commit/02550adcdba23ad0d1bbc59f1044c92e9972f8c2))

## v0.5.0 — (2026-09-08)

### Added
- **caching_sha2_password 対応(#197)** — これまで proxy(認証終端方式)が `mysql_native_password` しか話せず「MySQL 8.0 で native を有効にした構成限定」だったのを解消。ハンドシェイクで `caching_sha2_password` を advertise し、クライアントの応答長(32B=caching_sha2 / 20B=native)で判定して検証、caching_sha2 成功時は `AuthMoreData(fast_auth_success)` + `OK` を返す。これで **MySQL 8.0 のデフォルト認証や 9.x(native 廃止)** のクライアント/baseline でそのまま繋がる。go-sql-driver / PyMySQL / mysql CLI(8.0)で実機検証。
- **`auth.trust_loopback`(#198)** — 既定では loopback(127.0.0.1)からの API はトークン無しで通す(CLI 用)が、これを `false` にすると **loopback でも Bearer トークンを必須**にできる。同一ホストに信頼できない同居プロセスがいる環境向け。

### Changed
- **`baseline import` の高速化(#194)**: バルク投入セッションで `unique_checks` / `foreign_key_checks` / `sql_log_bin` を自動的に無効化する(セッション限定なので、import 後の実行時は FK/一意制約は通常どおり有効)。あわせて `--import-cnf <my.cnf>` を追加し、投入中だけ buffer pool や `innodb_flush_log_at_trx_commit` を緩めた mysqld で流し込める。実測(3M 行 / UNIQUE 索引 + FK、Apple Silicon): 約 25s → セッション変数のみ約 14.5s(-42%)→ `--import-cnf`(2G pool / trx_commit=0 / doublewrite off)併用で約 10s(-60%)。データ件数・FK 整合は不変。
- **`baseline import` の並列投入(#194)**: `--from` にディレクトリを渡すと `*.sql` を並列投入する(mydumper 出力やテーブル単位の分割ダンプ向け)。名前に `schema` を含むファイルを先に順次投入して全テーブルを作り、残りのデータファイルを `--threads N`(既定 = CPU 数)の接続で並列に流す。実測(8 テーブル / 280 万行、Apple Silicon): 単一ファイル逐次 約 14s → ディレクトリ 8 並列 約 7s(約 2 倍)。件数・テーブル数は不変。
- **baseline build の publish 経路も高速化(#194)**: `baseline build` が migration/seed を流し込む経路(`ApplyFile`)にも import と同じバルクロード用フラグを適用。大きな migration の投入が速くなる(build 用の使い捨て mysqld なのでセッション限定で安全)。
- **Terraform: `sashiki_ref` を VERSION から導出(#199)**: モジュールに `deploy/terraform/VERSION` を同梱し、`sashiki_ref` 未指定時はそこから解決する。利用側は `source` の `?ref=vX.Y.Z` を固定するだけでよく、バイナリ版がモジュールと自動一致(ref の二重指定 #193 が不要に)。

### Fixed
- **reconcile: 再起動で中断された遷移中ブランチを回収する(#206)**。sashikid が create/reset/delete の途中でクラッシュすると、ブランチが `creating` / `resetting` / `deleting` 状態のまま取り残され、reaper(running/sleeping しか触らない)にも起動時 reconcile(running→sleeping と dataset 欠損のみ扱う)にも回収されず永久に使えなくなっていた。reconcile が `creating`→error(op=create, recoverable)/ `resetting`→error(op=reset, recoverable)/ `deleting`→削除を完了、として回収するようにした(error 状態は `sashiki retry` で再駆動できる)。`ReconcileReport.Interrupted` と起動ログに件数を追加。
- **Terraform / install.sh の実戦修正(#192, #193)**: `apt-get` の dpkg ロック競合を待つ(`DPkg::Lock::Timeout`)/ `sashiki_ref` のピン留めを強調(module の `?ref=` と VERSION 由来値を一致させる)。

### Docs
- **apfs / reflink の per-branch quota ギャップ(#200)**: ZFS の `refquota` 相当が無いため `storage.default_storage_quota` が効かず、暴走ブランチへの storage admission は pool 使用率 watermark のみになることを `docs/LOCAL-DEV.md` に明記。
- README にコンテナ利用手順を追記し、フルコンテナ対応を「完了」に(#191)。

## v0.4.2 — (2026-09-07)

> v0.3.0〜v0.4.1 の詳細な差分は各 [GitHub Release](https://github.com/rikukadev/sashiki/releases)(自動生成ノート)を参照。ここでは主な追加・修正をまとめる。

### Added
- **`baseline promote <branch>`**: 検証済みブランチの現在の datadir をそのまま次の current baseline に昇格する(git の branch→main 相当)。snapshot 不変条件のため対象ブランチを graceful stop してから snapshot し、昇格後に再起動する。既存の他ブランチの origin は変えない。
- **VM レスのコンテナ実行(macOS / OrbStack)**: XFS reflink(CoW)+ process モード mysqld で、フル VM 無しに `create → reset → delete` が動く(`deploy/orbstack/`)。
- **VM レスの macOS ネイティブ実行が正式サポート(#113)**: `sashiki init --platform darwin` が Homebrew mysql 検出→APFS clonefile で baseline 構築→config 生成→launchd 常駐まで一括。`create → reset → delete` と proxy lazy create を実機 E2E で検証(`make e2e-darwin`、使い捨て temp root)。`docs/LOCAL-DEV.md` を実験的→正式サポートに更新(#139/#141/#142)。
- **macOS バイナリ配布**: goreleaser で darwin(arm64/amd64)を配布、`install.sh` が macOS 対応(`curl | bash`)。
- **`engine.mysql.extra_cnf`**: プロジェクト固有 my.cnf を `--defaults-file` で渡す(import / process / systemd の全経路に反映)。
- **`proxy.allowed_user`**: 接続許可ユーザーを設定可能に(未設定=app_user のみ / `""`=任意 / 名前=限定)。管理ユーザー接続向け。
- **`baseline import --db <name>`**: USE を含まない単体 DB ダンプの投入先を指定。
- **stale 表示**: origin が current baseline より古いブランチを `list` / `show` で示し、`reset` 時に警告(最新化は recreate)。

### Fixed
- **proxy DEPRECATE_EOF**: クライアントの capability に追従するよう修正。DEPRECATE_EOF を要求しないドライバ(PHP mysqlnd / Node / PyMySQL 等)で結果セットが空/エラーになる不具合を解消。
- **apfs/reflink の USED / capacity**: CoW 差分を正確に取れない値は 0 でなく「-」(不明)で表示。
- **extra_cnf の PERSIST 乗っ取り**: `--defaults-extra-file` → `--defaults-file` にし、起動前に `mysqld-auto.cnf`(SET PERSIST 残骸)を除去。
- **Terraform デプロイ実戦修正**: data device を EBS volume id から by-id で解決(Nitro nvme)/ awscli v2 zip 導入(Ubuntu 24.04)/ SSM・Secrets の平文ログ残留を `set +x` で防止 / `install.sh` のタグ直リンク取得 / instance profile のタグ除去(`iam:TagInstanceProfile` 不要)/ Route53 A レコード / `root_volume_size` / deb postinstall で `sashiki` ユーザー作成 + `/var/log` 権限。
- **systemd モードの extra_cnf**: ブランチの mysqld にも `--defaults-file` を反映(env の `MYSQLD_DEFAULTS` → ExecStart)。AppArmor が `/etc/sashiki/*.cnf` を読めるように。
- `baseline promote` が snapshot 後の baseline 登録に失敗すると、対象ブランチの mysqld を停止したまま抜けていた。成否に関わらず再起動するよう修正。
- `baseline import` が `mysql` / `mysqladmin` を PATH から引いており、`mysqld_bin` が PATH 外(Homebrew 等)だと失敗し得た。mysqld と同じディレクトリから解決するよう統一。
- `baseline import` 成功後の baseline 台帳登録エラーを握りつぶしていた(`baseline list` / GC から漏れる)。失敗時に警告を出すよう修正。
- systemd デプロイ整合性(authense 実戦 #134): `sashiki init` が生成する config を `sudo: true` に修正(sashikid は `User=sashiki` で動き zfs/systemctl を sudoers 経由で叩くため。`sudo: false` だとブランチ作成が permission denied で全滅していた、#176)。
- per-branch の `<branch>.env` を `/etc/sashiki`(root 所有で書けない)から `/run/sashiki` に移動。`sashikid.service` に `RuntimeDirectory=sashiki` を追加し、mysqld@/postgres-sashiki@ の `EnvironmentFile` も追随(#177)。
- sudoers に `baseline promote` の snapshot(`branches/*@baseline-*`)と、promote 済み baseline からの clone を追加(promote 後の運用が sudo で止まらないように、#178)。
- promote 元ブランチの `delete` が baseline snapshot ごと破棄し current baseline を宙吊りにしていたのを、dataset 上に baseline がある間は delete を拒否するよう修正(#179)。
- `baseline import` が常に root を要求し macOS ネイティブ(ログインユーザー)で使えなかったのを、apfs/reflink では非 root 実行を許可(root 時のみ mysql ユーザーへ降格)(#138)。
- macOS の `sashiki init --platform darwin` が baseline snapshot 取得前に `auto.cnf` を削除するよう修正(全ブランチが同一 server_uuid になるのを防ぐ、#80 と同趣旨)。

## v0.2.0 — public preview (2026-09-07)

初の公開リリース。設計仕様 v2 の機能を実装し、MySQL + GitHub PR プレビューの経路を実機で検証した。

### Highlights
- **branch lifecycle**: create / reset / recreate / delete / retry
- **proxy(:3306 固定)**: `dev@<branch>` ルーティング + 認証終端(方式A)+ 認証後 lazy create + TLS 終端
- **baseline**: build → validate → publish、`baseline set` ロールバック、GC(keep_last / retention)、
  組み込みローダー(`source_dir` に SQL を置くだけで refresh)
- **profile / lease**: preview / ci / sandbox + `--ttl` / `lease renew`
- **capacity**: メモリ admission、storage watermark、`sashiki capacity`
- **運用**: 起動時 reconciliation、`sashiki doctor`、orphan GC、`sashiki drain`、
  構造化ログ + Prometheus メトリクス、Web UI(データブラウザ)
- **engine**: MySQL / PostgreSQL(Postgres は直接ポート接続)
- **storage**: EBS-ZFS(既定)/ FSx-ZFS
- **配布**: GitHub Action、Terraform モジュール(RDS 互換 I/O)、deb / install.sh

### Security / correctness(実運用で検出・修正)
- proxy: 認証終端時にクライアント指定の DB を backend へ引き継ぐ(No database selected の修正)
- proxy: データフェーズの half-close + TCP keepalive
- sudoers のパス制限、AppArmor enforce プロファイル、auto.cnf 削除(server_uuid 重複防止)

### Known limitations
- API / config は未固定(v0.x)
- PostgreSQL は proxy / lazy create 非対応(直接ポート)
- FSx / multi-host / Spot は実装済みだが本番運用実績なし
- 本番 DB 用途は対象外(単一ノード、HA/レプリカなし)
