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

**決定**: sashikid が直接 mysqld プロセスを孵化させず、`systemctl start mysqld@<branch>` を経由する。datadir とポートは `/etc/sashiki/<branch>.env` の EnvironmentFile で渡す。

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
