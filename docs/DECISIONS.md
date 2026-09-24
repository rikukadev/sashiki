# 設計判断の記録 (ADR)

実装中に決めたことを追記していく。フォーマット: 背景 → 決定 → 理由。

## ADR-001: @init スナップショットとフックの順序

**背景**: 仕様 14-2 は「on-create の結果(マイグレーション適用済み)が reset の戻り先になる」ため @init をフック後に取得すると定めるが、仕様 15-6 は「@init は clone 直後・mysqld 起動前(クリーン状態)でのみ取得する」と定める。稼働中スナップショットは reset のたびにクラッシュリカバリを引き起こす(FSx 検証で実証済み)。

**決定**: フックの有無で経路を分ける。
- フックなし: clone → @init → start(最速。@init はベースと同一のクリーン状態)
- フックあり: clone → start → ready → on-create → **正常終了** → @init → start

**理由**: 両方の要求(フック結果を reset に含める / @init は常にクリーン)を満たす唯一の順序。フックあり時の追加コストは stop/start 1 回(数秒)で、マイグレーション適用時間に比べ誤差。

## ADR-002: 遅いバックエンドの reset は v0.1 では未実装

**背景**: 仕様 15-3 は reset の共通経路を「新クローン + 付け替え」とし、zfs rollback をその最適化と位置づける。

**決定**: v0.1 は FastRollback=true(zfs)の rollback 経路のみ実装。FastRollback=false のバックエンドでは明示的なエラーを返す。「新クローン + 付け替え」は fsx バックエンド実装(v1.0)と同時に入れる。

**理由**: v0.1 は zfs 専用であり、付け替え(ポート・接続先の切り替え)は実バックエンドなしにテスト不能。インターフェース(Capabilities.FastRollback)だけ v0.1 で切っておく。

**更新**: fsx-zfs の実装で「同じ origin から新クローン + 付け替え」の reset を実装済み(`Manager.recreateFrom`)。zfs の rollback と違い on-create hook を再実行する。

## ADR-003: SQLite ドライバは modernc.org/sqlite

**決定**: cgo 不要の pure Go 実装を使う。

**理由**: クロスコンパイル(linux/amd64・arm64 の release ビルド)が単純になる。性能は sashikid の書き込み頻度(ブランチ操作時のみ)では問題にならない。MaxOpenConns=1 で直列化。

## ADR-004: mysqld は systemd テンプレートユニットで管理

**決定**: sashikid が直接 mysqld プロセスを孵化させず、`systemctl start mysqld@<branch>` を経由する。datadir とポートは `/run/sashiki/<branch>.env`(sashikid の RuntimeDirectory、#177)の EnvironmentFile で渡す。

**理由**: sashikid の再起動・クラッシュとブランチ mysqld の生存を分離できる。プロセス監督(異常終了の記録)を systemd に任せられる。PoC で実証済みの構成。

## ADR-005: API 認証は「loopback 無認証 + 外部は Bearer」

**決定**: 127.0.0.1 からのリクエストは無認証、それ以外は Bearer トークン(SHA-256 定数時間比較)。トークンは環境変数から読む。

**理由**: 仕様 13-3。ホスト上の CLI 利用を摩擦なしにし、GitHub Action など外部からの呼び出しだけ守る。

**更新(v0.3)**: `sashiki token` サブコマンドを実装し、state.db の tokens テーブル(SHA-256 ハッシュ保存・last_used_at 記録)との照合を追加した。env トークンは後方互換として残る。DB 照合のエラーは認証失敗と区別してログに残す(無言の 401 にしない)。

## ADR-006: プロキシは「バックエンド起点の AuthSwitch 転送」方式、proxy_user は mysql_native_password

**背景**: プロキシはユーザー名 `<user>@<branch>` を見てからバックエンドを選ぶが、MySQL はサーバーが先にハンドシェイク(salt 含む)を送るため、クライアントの最初の認証応答は sashiki の salt に対するもので転送できない。当初は sashiki 自身が AuthSwitchRequest を送る設計だったが、バックエンドも(申告プラグインとユーザーの実プラグインが違うと)AuthSwitch を返すため、クライアントが「2 回目の AuthSwitch」をプロトコル違反として切断することが実機で判明した。

**決定**:
1. sashiki は合成ハンドシェイクでユーザー名だけ取得し、バックエンドには**わざとユーザーの実プラグインと異なる caching_sha2 を名乗って**接続する。バックエンドが必ず返す AuthSwitchRequest(新しい salt 付き)を**そのままクライアントへ転送**して認証させる。sashiki はパスワードを保存せず、正否はバックエンドが判断する(仕様 14-4)
2. この方式では両側の会話が handshake(0) → response(1) → switch(2) → reply(3) → result(4) と対称になり、**sequence 番号の書き換えが不要**
3. バックエンドへ申告する capability は「クライアント ∩ バックエンド ∩ **sashiki が合成ハンドシェイクで広告した集合(synthCaps)**」に絞る。広告していない機能(QUERY_ATTRIBUTES 等)を申告すると、クライアントの送るパケットとバックエンドの期待がずれて Malformed packet になる(実機で確認)
4. baseline import が作る proxy_user は **mysql_native_password**。AuthSwitch 1 回で認証が完結し決定的になる(caching_sha2 の full-auth は RSA 鍵交換が挟まる)。8.4 以降は native がデフォルト無効のため、caching_sha2 対応は TLS 終端(sni ルーティング)導入時に再検討

**理由**: 実機の挙動(2 回目 AuthSwitch 拒否・caps 不一致の Malformed packet)に合わせた最小の設計。認証フェーズは素直な双方向転送ループだけで済む。

## ADR-007: プロキシは「認証終端(方式A)」へ移行、proxy 自身がパスワードを検証する

**背景**: ADR-006(バックエンド起点の AuthSwitch 転送)は中継として成立し実機で稼働していたが、認証の正否をバックエンドに委ねるため、**認証前にブランチを route / lazy create してしまう**構造的な問題が残っていた(ポートに到達できる相手が任意名で clone + mysqld 起動を積み上げられる DoS、#7)。また TLS 終端・caching_sha2 対応・パスワード管理の一元化が中継方式では難しい。#31 で方式A(認証終端)を採用と決定し、#51 で実装した。

**決定**:
1. sashiki が app_user のパスワード(`engine.mysql.proxy_pass`、本番は Secrets Manager 由来)を保持し、クライアントの **mysql_native_password 認証を自身で検証**する(`SHA1(pass) XOR SHA1(salt||SHA1(SHA1(pass)))` を合成ハンドシェイクの salt に対して照合、定時間比較)。
2. **認証に成功してから** `RouteBranch` / lazy create する。認証前の無償リソース確保(#7 の DoS 構造)を解消する。
3. バックエンドへは sashiki がクライアントとして mysql_native_password で接続し直す(保持している credential を使用)。認証フェーズはクライアントから見えない。
4. TLS 終端は `proxy.tls_cert` / `proxy.tls_key` 指定時のみ有効(クライアント↔sashiki=TLS、sashiki↔backend=localhost 平文)。SSLRequest を検出して `tls.Server` に切り替えてから本 HandshakeResponse を読む。
5. クライアントには合成ハンドシェイクで `mysql_native_password` を名乗るため、8.0/8.4 のデフォルト(caching_sha2)クライアントも native 応答で接続できる。将来 caching_sha2 の full-auth を終端する場合は TLS 前提で追加する。

**理由**: 認証を終端することで #7 の DoS を構造的に閉じ、TLS 終端とパスワード管理を sashiki 側に一元化できる。ADR-006 は「中継でも成立する」ことを実証した経緯として本ファイルに残す(現行の実装は本 ADR)。

**関連**: #31(決定)、#51(実装)、#7(DoS)、ADR-006(前方式・経緯として保持)。

## ADR-008: 認証を caching_sha2_password に統一(client 検証 + backend 接続の両 leg)

**背景**: ADR-006-4 / ADR-007 は proxy_user を `mysql_native_password` で作り、client 検証・backend 接続とも native を前提にしていた(「caching_sha2 は TLS 終端導入時に再検討」)。しかし **MySQL 8.4 は native をデフォルト無効**にし、**9.x は native を廃止**したため、`CREATE USER ... IDENTIFIED WITH mysql_native_password` が `ERROR 1524`(8.4)/ 作成不能(9.x)になり、baseline import が動かない。native 前提のままでは 8.4/9.x を backend mysqld に使えない。

**決定**:
1. **app_user(dev)は backend の版に応じたプラグインで作成**する(baseline import / `init --platform darwin`)。`SELECT @@version` を見て **MySQL 8.0+ は `caching_sha2_password`、5.7 等(major<8)は `mysql_native_password`**(caching_sha2 は 8.0 追加で 5.7 に無い)。**MariaDB** は version 文字列に `MariaDB` を含み major も 10+ だが caching_sha2 を持たない(native / ed25519)ため、名前で先に判定して native にする。判定不能時は既定 caching_sha2。正式サポートは MySQL 8.0〜9.x(実機検証 8.0/8.4)、**5.7 は best-effort(EOL・未検証)、MariaDB は非対応**(native を選ぶが他挙動未検証)。
2. **client → proxy**: proxy は合成ハンドシェイクで caching_sha2 を名乗り、sashiki が app パスワードから fast-auth スクランブルを計算して検証(#197)。native クライアントは AuthSwitch でフォールバック。
3. **proxy → backend**: sashiki が caching_sha2 で接続し直す。branch mysqld は起動直後でキャッシュが空のため full-auth になるが、**sashiki↔backend は localhost 平文 TCP** なので、TLS の代わりに **RSA 公開鍵手順**で送る(pubkey 要求 `0x02` → `0x01`+PEM 受領 → `password\0` を nonce で XOR → RSA-OAEP/SHA-1 で暗号化)。cleartext を平文回線に出さない。
4. baseline の app_user が(旧版由来で)native の場合は、backend が返す AuthSwitchRequest の plugin にあわせて応答するため後方互換を保つ。

**理由**: native は 8.4/9.x で使えず backend の版を縛る。caching_sha2 は 8.0/8.4/9.x 共通で、方式A(ADR-007)は client 認証を sashiki が終端するので、backend leg を caching_sha2 化しても localhost で RSA full-auth を 1 回行うだけで完結する。実機 E2E(import→create→proxy 接続→reset→delete)を MySQL 8.0 と 8.4 で確認。

**これにより ADR-006-4 の「proxy_user は mysql_native_password / caching_sha2 は TLS 終端まで保留」は supersede される**。ADR-007 の native 記述(決定 1/3/5)も caching_sha2 に更新される(方式A の骨子=認証終端は不変)。

**関連**: #197(client 側 caching_sha2)、本 PR(backend 側 + app_user)、ADR-006-4 / ADR-007(前提を更新)。

## ADR-009: proxy の合成ハンドシェイクは backend の版を映さず `8.0.0-sashiki-proxy` を名乗る

**背景**: 方式A(ADR-007)では proxy がクライアントへハンドシェイクを返してから認証し、`user@branch` の branch 部を見て初めて接続先が決まる。つまり **ハンドシェイクを送る時点では backend mysqld の版が分からない**(lazy create ならまだ起動すらしていない)。一方 MySQL は 9.7 の次から calendar versioning(`26.7.0` など)になり、「backend の版をそのまま名乗る」案は将来も追従し続ける前提を置くことになる(#285)。

**決定**:
1. 合成ハンドシェイクの server version は **固定の `8.0.0-sashiki-proxy`** とする。backend の版(8.0 / 8.4 / 26.7)には依存しない。
2. version 文字列でクライアントに伝えたいのは「**8.0 世代のプロトコル・既定値**(caching_sha2_password を名乗る、utf8mb4、CLIENT_PLUGIN_AUTH 等)で話せる」ことだけで、機能検出はケイパビリティビットで行う。8.0 未満を名乗ると一部ドライバが native 認証・utf8 前提の古い経路に入るため、8.0 以上であることが要件。
3. backend 固有の版が要る用途(`SELECT @@version`、`sashiki show` の `engine` 情報)は接続後にクエリで取れるので、ハンドシェイクで運ばない。
4. 版の互換範囲は **backend: MySQL 8.0 / 8.4 / 26.7(実機検証)、5.7 は best-effort、MariaDB 非対応**(ADR-008 と同じ)。**client**: go-sql-driver / Node mysql2 / PyMySQL / mysql CLI で `8.0.0-sashiki-proxy` を見て問題が無いことを確認済み。

**理由**: 認証終端の構造上、ハンドシェイク時点で backend は未定。固定版なら「どの branch に繋いでも同じ挙動」が保てて、クライアントの版依存の挙動差をブランチごとに持ち込まない。`8.0.0` は caching_sha2 / utf8mb4 の既定が揃った最初の版で、以降の版でプロトコルは互換。

**やらないこと**: backend の版をハンドシェイクに反映する(認証後に接続先が決まるため不可能)。合成版を設定で変える(「8.0 以上」以外に意味のある選択肢が無い)。

**Linux `sashiki init` のパッケージ方針**: Ubuntu 24.04 の apt にある `mysql-server-8.0` を入れる(固定)。8.4 LTS / 26.7 を使う場合は MySQL APT リポジトリ等で先に入れてから init する(`/usr/sbin/mysqld` があれば apt ステップは飛ぶ。`--skip-packages` で明示も可)(sashiki 自体は 8.0 / 8.4 / 26.7 で動く。app_user のプラグインは `@@version` から選ぶ)。CI / E2E は 8.0 のみで、他版は macOS ネイティブの実機確認に留まる。

**関連**: #285、ADR-007(方式A)、ADR-008(caching_sha2 統一)。

## ADR-010: root 操作は sudoers のパターン行ではなく `sashiki-root-helper` に閉じ込める

**背景**: sashikid(User=sashiki)は zfs / zpool / systemctl に root が要る。ADR / #78 では sudoers に `zfs clone <base>@* <branches>/*` のような実呼び出し形のパターン行を並べていたが、sudoers の `*` は空白をまたいでマッチするため `zfs clone <base>@x -o mountpoint=/etc <branches>/y` のような追加引数を防げない。仕様 20-3 は v0.3 で root-helper に閉じ込めるとしていた(#276)。

**決定**:
1. `cmd/sashiki-root-helper`(setuid 無しの通常バイナリ)を deb / tar.gz に同梱し、sudoers は `sashiki ALL=(root) NOPASSWD: /usr/local/bin/sashiki-root-helper *` の 1 行だけにする。
2. helper は `/etc/sashiki/root-helper.yaml`(root 0600、`sashiki init` が生成。引数や環境変数で場所を変えられない)の pool / base_dataset / branch_parent / units を読み、argv を型付き allowlist(`internal/roothelper.Validate`、純関数)で検証してから絶対パスの実体(`/usr/sbin/zfs` 等)を固定環境で exec する。許可する形は sashikid が実際に発行する形と 1:1(clone / snapshot / rollback -r / destroy -r・destroy <snap> / rename / set refquota= / get -H [-p] -o value {mountpoint,used,referenced} / list、zpool list -Hp / status -x、systemctl start|stop|is-active|kill -s SIGKILL <unit>@<name>)。
3. sashikid は config の `root_helper`(init のテンプレートは `/usr/local/bin/sashiki-root-helper`)が設定されていれば `sudo -n <helper> <tool> args...` で呼ぶ。空なら旧方式(`sudo -n zfs ...`)。
4. 既存ホストの移行: `sashiki init` は既存 config に `root_helper` が無ければ旧方式の sudoers を維持する(config を書き換えないと sashikid が止まるため)。運用者が config に `root_helper:` を足して `sashiki init` を再実行すると helper 1 行に絞られる(docs/UPGRADING.md)。

**理由**: 検証をコード(テスト可能な純関数 + negative test)に置けば、許可外 dataset・追加フラグ・任意 property・任意 unit を確実に拒否できる。デーモン + IPC にしない(sudo が認証・監査ログを持っており、exec 1 回の helper で十分)。

**関連**: #276、#78、仕様 20-3。
