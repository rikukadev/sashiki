# アップグレードの注意

版を上げるときに**設定やコマンドの挙動が変わる**ものだけを書く。バグ修正の一覧は
[CHANGELOG.md](../CHANGELOG.md)、機能の説明は [README](../README.md) と
[docs/SPEC.md](SPEC.md) を見る。

v0.x の間は API / config が固定されていないので、マイナー版で挙動が変わることがある。

## 0.12.0 → 0.12.x

### Terraform: データ EBS が既定で暗号化される(#341)

新規に作る data EBS は `encrypted = true`(`kms_key_id` で利用者管理 KMS も可)。**v0.12.0 以前に作った
環境**は volume が非暗号化のまま module を更新すると、plan が「`encrypted` の変更 → 作り直し」になり
`prevent_destroy` で止まる(データは消えない)。どちらかを選ぶ:

- 当面そのまま使う: `data_volume_encrypted = false` を明示する(plan の差分が消える)
- 暗号化 volume へ移行する(ダウンタイムあり):
  1. `sashiki drain` → `sudo systemctl stop sashikid` → `sudo zpool export tank`
  2. `aws ec2 create-snapshot --volume-id <旧 volume>` → 完了を待つ
  3. `aws ec2 create-volume --snapshot-id <snapshot> --availability-zone <AZ> --encrypted [--kms-key-id <key>] --volume-type gp3`
  4. `terraform state rm module.<name>.aws_ebs_volume.data` → `terraform import module.<name>.aws_ebs_volume.data <新 volume>`
     (`aws_volume_attachment.data` も `state rm` してから import する。id は `vai-` + `<device>:<volume>:<instance>` の形)
  5. `terraform apply`(差分が attach の付け替えだけになることを確認)→ 旧 volume を detach し、新 volume を
     同じ device 名で attach → `sudo zpool import -f tank` → `sudo systemctl start sashikid`
  6. 動作確認後に旧 volume を削除する

### Terraform: `api_token_ssm_path` が revoke 可能な `branches` トークンになる(#340)

これまで `api_token_ssm_path` の値は環境変数 `SASHIKI_API_TOKEN` に渡す **admin** トークン
(revoke 不能)だった。0.12.x からは bootstrap でインスタンスが `sashiki token create --name ci
--scope branches` で発行した state.db トークンを置き、Terraform は値を管理しない。

- **新規インスタンス / 置換後**: 自動でこの形になる。CI(GitHub Action、`transport: api`)は
  そのまま動くが、admin 操作(`baseline publish` / `drain` / `gc` / データブラウザ)はこの
  トークンでは 403 になる。ホスト上の loopback から行う(`aws ssm start-session --target <instance_id>`
  → `sashiki ...`)。リモートから admin が要るなら `admin_token = true`
- **既存インスタンス**(置換しない場合): `api_token_ssm_path` の値は `ignore_changes` なので旧 admin
  トークンのまま動き続ける(無停止)。絞るなら、ホスト上で
  `sudo sashiki token create --name ci --scope branches` の平文を
  `aws ssm put-parameter --name <api_token_ssm_path> --type SecureString --value ... --overwrite` で入れ替え、
  `/etc/sashiki/sashikid.env` を空にして `sudo systemctl restart sashikid`(admin_token = false なら
  次の置換で自動的にこの状態になる)
- 呼び出し側で `random_password.api_token` を参照していた場合は `admin_token = true` +
  `admin_token_ssm_path` に置き換える

### Terraform: `github_token` 入力を削除(#342)

private リポジトリ向けの `github_token` は user-data(EC2 属性)と cloud-init ログに平文で残るため削除した。
呼び出し側に `github_token = ...` が残っていると `Unsupported argument` になるので消す。値を渡していた
場合はその token を GitHub 側で失効させる(user-data に残っている)。

## 0.11.x → 0.12.0

### root 操作が `sashiki-root-helper` 経由になる(#276)

sashiki ユーザーの sudoers が「zfs / systemctl のパターン行」から「`/usr/local/bin/sashiki-root-helper` 1 行」に
変わる(ADR-010)。**新規インスタンス**(deb で入れて `sashiki init`)は自動でこの方式になる。

**既存ホスト**は `sashiki init` を再実行しても旧方式の sudoers が維持される(config に `root_helper` が
無いため)。移行するには:

1. バイナリを更新する(deb なら `sashiki-root-helper` も `/usr/local/bin` に入る)
2. `/etc/sashiki/config.yaml` のトップレベルに `root_helper: /usr/local/bin/sashiki-root-helper` を足す
3. `sudo sashiki init --pool <pool>` を再実行する(sudoers を helper 1 行に絞り、`/etc/sashiki/root-helper.yaml` を書く)
4. `sudo systemctl restart sashikid` → `sashiki doctor` / `sashiki create` で動作を確認する

2 と 3 の順序を逆にすると、sudoers が先に絞られて旧方式の `sudo -n zfs` が拒否される(sashikid の
create / delete が全部失敗する)。その場合は config に `root_helper` を足して restart すれば戻る。

## 0.11.0 → 0.11.x

### CLI の引数チェックが全コマンドに及ぶ(#325)

0.11.0 の「未知のフラグは終了コード 2」は一部のコマンド(create / show / list / capacity /
doctor / drain / gc / lease / baseline build)だけだった。残りも同じ扱いになる。

- `token create` は未知のフラグと値無しの `--scope` をエラーにする(以前は黙って
  `branches` で発行していた)。
- `hooks run` / `op list` / `op show` / `baseline set` / `baseline gc` / `baseline refresh` は
  余分な引数・未知のフラグを受けない。`env` / `connect` は未知のフラグをブランチ名として
  API に投げない(404 → 3 ではなく 2)。
- `show` / `reset` / `recreate` / `retry` / `sleep` / `wake` / `list` / `baseline list` は
  create 専用のフラグ(`--port` / `--owner` / `--ttl` …)を受けない。
- `delete --json` が使える(usage と REFERENCE が掲げていたが拒否されていた)。
- 引数エラーの終了コードは **2** に統一(`--for` / `--prefix` の値無し、`--threads` の不正、
  `baseline export --to` 無し、`init --engine` の不正は以前 1 だった)。

## 0.10.x → 0.11.0

README 監査(#286〜#308)の修正がまとまって入る。**上げる前に config を確認する**。

### 1. config の誤りが起動エラーになる(#300)

これまで黙って無視されていた設定が、sashikid の起動時にエラーになる。

- **未知のキー**(綴り間違い、SPEC の古い書き方の `baselines:` / トップレベル `profiles:` /
  `branches.max_running` / `engine.mysql.binary` など)
- **解釈できないサイズ**(`256MB` / `20GiB` は読めるようになった。`256XB` のような値はエラー)
- **範囲外の watermark**(`high_watermark` / `critical_watermark` は 0〜1 の比率。`80` は不可)
- **`proxy.tls_cert` / `proxy.tls_key` の片方だけ**の指定(以前は黙って平文で listen していた)

設定だけを検証するサブコマンドはまだない。稼働中のサービスと並行して
`sashikid --config /etc/sashiki/config.yaml` を手動起動してはいけない。設定を読んだ後に
`state.db` を開いて起動時の reconcile まで実行するため、既存デーモンと競合する。
上げる前に [SPEC.md の 21 章](SPEC.md#21-設定ファイル)と照合し、バイナリ更新後は
systemd のサービスを 1 つだけ再起動して `systemctl status sashikid` と
`journalctl -u sashikid` で設定エラーがないことを確認する。

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
最大 10 分待つようになったので、`sashikid.service` の `TimeoutStopSec` は 660 秒にしてある。
このユニットは deb が置くもので、**`sashiki init` では更新されない**(init が書くのは
`mysqld@.service` / `postgres-sashiki@.service` だけ)。deb を上げ直せば `/lib/systemd/system/`
に入る。tarball から入れている場合は `deploy/systemd/sashikid.service` を手で置き直す。

### 6. macOS の Postgres 版の既定ポートが変わる(#304)

`init --platform darwin --engine postgres` が書く config が **API `:8081` / metrics `:9101`** になり、
MySQL 版(`:8080` / `:9100`)と同時に常駐できる。CLI は `SASHIKI_API_URL` で向き先を選ぶ。
**既存の config は書き換えない**。

### 7. ブランチの MySQL が loopback だけで listen する(#288)

systemd 構成でも branch の mysqld が `127.0.0.1` に閉じる(次回の起動から)。
直結ポート(3401-3600)に外から繋いでいた構成は、proxy(3306)経由に変える。

ユニット自体(`mysqld@.service`)の更新は `init` の再実行で入る。**pool 名を必ず合わせる**:

```bash
zpool list                                             # pool 名を確認(Terraform 構築は tank)
sudo sashiki init --pool <pool 名> --skip-packages --yes
```

0.11.0 の `init` は `--pool` 省略時に既存 config の pool を使うが、それより前の版で
`--pool` を省略すると既定の `dbpool` で AppArmor と sudoers を書き換え、動いている
ブランチの mysqld が拒否される。

#299(`buffer_pool_size` / `shared_buffers` が実際の mysqld / postgres に届く)も、
旧ユニットが残っていると unit 側の `256M` 固定が勝つ。同じ再実行で新しいユニットが入り、
ブランチを再起動(`sashiki sleep` → 接続)すると効く。

### 8. Terraform: API が VPC 内から到達できるようになる(#286)

user-data が `listen.api` を `0.0.0.0:8080` に書き換えるようになり、出力の `api_url` が
`allowed_sg_ids` の SG から実際に使える(以前は loopback にしか bind しておらず届かなかった)。
新規インスタンスには自動で反映されるが、既存インスタンスでは user-data が再実行されない。
`/etc/sashiki/config.yaml` の `listen.api` を `0.0.0.0:8080` に変更して sashikid を再起動する。

v0.11.0以前から、state.db永続化対応を含む版へ上げるときは次の2 applyに分ける。

1. moduleのrefだけを更新し、**EC2を置換せず**`terraform apply`する。SSM Associationが
   root volumeの`/var/lib/sashiki/state.db`をdata EBSの`tank/sashiki-state`へ移し、
   health checkまで待つ。
2. 1が成功したあとで、必要なら`terraform apply -replace=module.<name>.aws_instance.this`
   またはinstance classの変更をapplyする。

module更新とEC2置換を同じplanに入れてはいけない。旧root volumeを破棄した後では台帳を
救出できない。移行後はbranch / baselineに加え、port / origin / operation / tokenを持つ
`state.db`もdata EBSから新インスタンスへ引き継がれる(#275)。

### 9. Terraform: `engine_version` 入力を削除(#306)

未使用だった Terraform module の `engine_version` 入力を削除した。module の `source` を
0.11.0 に上げる前に、呼び出し側の `engine_version = ...` を削除する。残したままだと
`terraform validate` / `terraform plan` が `Unsupported argument` で失敗する。

### 10. CLI の引数・終了コード・`connect` を厳格化(#307 / #308)

CLI を呼ぶスクリプトは次を確認する。

- 未知のフラグと余分な位置引数は、無視せず終了コード **2** で失敗する。
- `--timeout` / `--interval` は `30s` / `200ms` のように単位まで指定する。不正な値は
  既定値へ戻さず終了コード **2** で失敗する。
- operation の待機タイムアウトは終了コード **6** になった。終了コード **5** は容量不足(507)だけに使う。
- `sashiki connect` は固定パスワード `dev` をコマンドラインへ埋め込まない。
  `SASHIKI_DB_PASSWORD` を渡すか、mysql / psql の対話入力を使う。Postgres の接続先DBは
  `SASHIKI_DB_NAME` で指定し、未指定なら `postgres` を使う。
