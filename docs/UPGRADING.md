# アップグレードの注意

版を上げるときに**設定やコマンドの挙動が変わる**ものだけを書く。バグ修正の一覧は
[CHANGELOG.md](../CHANGELOG.md)、機能の説明は [README](../README.md) と
[docs/REFERENCE.md](REFERENCE.md) を見る。

v0.x の間は API / config が固定されていないので、マイナー版で挙動が変わることがある。

## 0.10.x → 0.11.0

README 監査(#286〜#308)の修正がまとまって入る。**上げる前に config を確認する**。

### 1. config の誤りが起動エラーになる(#300)

これまで黙って無視されていた設定が、sashikid の起動時にエラーになる。

- **未知のキー**(綴り間違い、SPEC の古い書き方の `baselines:` / トップレベル `profiles:` /
  `branches.max_running` / `engine.mysql.binary` など)
- **解釈できないサイズ**(`256MB` / `20GiB` は読めるようになった。`256XB` のような値はエラー)
- **範囲外の watermark**(`high_watermark` / `critical_watermark` は 0〜1 の比率。`80` は不可)
- **`proxy.tls_cert` / `proxy.tls_key` の片方だけ**の指定(以前は黙って平文で listen していた)

上げる前に `sashikid --config /etc/sashiki/config.yaml` を手で 1 回起動して確かめるのが速い。
キーの一覧は [SPEC.md の 21 章](SPEC.md#21-設定ファイル)。

### 2. API トークンに scope が付く(#294)

`sashiki token create` の**既定が `branches`** になった。ブランチの作成・削除・reset と
読み取りだけができる。次は **admin** が要る:

- baseline の publish / promote / set / delete / gc / refresh / build / validate
- `drain`、`gc --orphans`
- データブラウザ(`/v1/branches/{name}/query` と `/schema`)
- hook の手動実行

CI からこれらを叩いているなら `sashiki token create --name ci --scope admin` で発行し直す。
**既存のトークンと環境変数のトークン(Terraform 生成)は admin のまま**なので、
そのままでも動く。

### 3. Linux の `sashiki init` が `app_pass` をランダム生成する(#297)

新規構築では `dev` 固定ではなくなり、`init` の最後に 1 回だけ表示する。固定したいなら
`sashiki init --app-pass <値>`。**既存の config は書き換えない**ので、アップグレードだけなら
影響しない。proxy は認証失敗が続く接続元の次の試行を遅らせる(5 回まで即時、以降 1s → 8s)。

### 4. `error` 状態のブランチが自動削除される(#298)

`branches.error_retention`(既定 **72h**)を過ぎた `error` のブランチを reaper が削除する。
調査のために残したいなら `error_retention: 0`。lease(`--ttl`)は `error` でも効くようになった。

### 5. 同じブランチへの操作の連打が 409 になる(#303)

実行中の変更操作(create / reset / recreate / retry / delete)があるあいだ、同じブランチへの
次の変更は **`409 operation_in_progress`**(CLI は終了コード 4)で断る。
`--wait`(既定)で完了を待ってから次を叩けば当たらない。sashikid は停止時に実行中の操作を
最大 10 分待つようになったので、`systemd` の `TimeoutStopSec` は 660 秒にしてある
(`sashiki init` を再実行するとユニットが更新される)。

### 6. macOS の Postgres 版の既定ポートが変わる(#304)

`init --platform darwin --engine postgres` が書く config が **API `:8081` / metrics `:9101`** になり、
MySQL 版(`:8080` / `:9100`)と同時に常駐できる。CLI は `SASHIKI_API_URL` で向き先を選ぶ。
**既存の config は書き換えない**。

### 7. ブランチの MySQL が loopback だけで listen する(#288)

systemd 構成でも branch の mysqld が `127.0.0.1` に閉じる(次回の起動から)。
直結ポート(3401-3600)に外から繋いでいた構成は、proxy(3306)経由に変える。
ユニット自体の更新は `sudo sashiki init --skip-packages --yes` の再実行で入る。

### 8. Terraform: API が VPC 内から到達できるようになる(#286)

user-data が `listen.api` を `0.0.0.0:8080` に書き換えるようになり、出力の `api_url` が
`allowed_sg_ids` の SG から実際に使える(以前は loopback にしか bind しておらず届かなかった)。
`terraform apply` でインスタンスを入れ替えたときに反映される。
