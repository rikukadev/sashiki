# ローカル開発(macOS + Lima)

「git worktree でブランチごとに並行開発、DB も worktree ごとに独立させたい」——
sashiki はこれに向いている。worktree 1 本 = DB ブランチ 1 本(同じ baseline から CoW クローン、
完全分離、`reset` で即戻せる)。ここでは **macOS で常駐させて開発に使う**手順を書く。

## 構成

> **まず [VM を使わない経路](#vm-を使わない経路正式サポート113) を検討する。** macOS ネイティブ
> (APFS clonefile + mysqld 直起動)とコンテナ(XFS reflink)が正式サポートで、多くの用途はそれで足りる。
> この節の Lima 構成は「本番と同じ ZFS + systemd を手元で再現したい」ときのもの。

Lima 構成は ZFS + systemd(Linux カーネル)を使うので、macOS では **Lima のフル VM** の中で
動かす。アプリ側(OrbStack のコンテナや Mac ホストのプロセス)は VM の MySQL に TCP で繋ぐ。

```
[macOS]
 ├─ OrbStack ─► アプリのコンテナ                    ─┐
 ├─ Mac host のプロセス(go run など)              ─┼─► :3306 ─► [Lima VM] sashikid
 └─ Lima VM(Ubuntu + ZFS)◄── port-forward 3306/8080 ┘        (proxy が dev@branch で振り分け)
```

> **なぜ VM?** sashiki の CoW は ZFS の機能で、ZFS は out-of-tree のカーネルモジュール。
> **OrbStack / Docker Desktop の軽量共有カーネルでは ZFS を load できない見込み**なので、
> フル VM(Lima / Colima / UTM)の中で動かす。アプリは今までどおり OrbStack でよい。

## セットアップ

### 1. VM を起動(要 `brew install lima`)

```bash
limactl start --name=sashiki-dev ./docs/lima-dev.yaml --yes
limactl shell sashiki-dev   # 以降このシェルの中で作業
```

`docs/lima-dev.yaml` は :3306 / :8080 を Mac host へ port-forward してある(memory/disk は
baseline サイズに合わせて調整)。

### 2. インストール & 初期化(VM 内)

```bash
# sashiki 導入
curl -fsSL https://raw.githubusercontent.com/rikukadev/sashiki/main/install.sh | sudo bash

# ループバックファイルの zpool で初期化(物理ディスク不要)
truncate -s 40G /var/tmp/sashiki.img
sudo sashiki init --pool tank --device /var/tmp/sashiki.img
```

### 3. baseline(元データ)を入れる(VM 内)

```bash
# 手元の本番相当ダンプを VM に渡してから
sashiki baseline import --from /path/to/dump.sql
sudo systemctl enable --now sashikid
```

これで `sashiki-dev` VM に、いつでもブランチを払い出せる MySQL が常駐する。

## worktree ごとに DB を生やす

proxy の **lazy create** を使うと、`dev@<branch>` で**初回接続した瞬間にそのブランチが生える**。
CLI で create する必要すらない。各 worktree の `.envrc`(direnv)にこう書く:

```bash
# <repo>/.envrc  (git worktree ごとに同じ内容でよい)
export DB_HOST=host.docker.internal   # OrbStack のコンテナから。Mac host 直なら 127.0.0.1
export DB_PORT=3306
export DB_USER="dev@$(git branch --show-current | tr '/' '-' | cut -c1-32)"
export DB_PASSWORD=dev                # ローカル既定。config の app_pass を変えたら合わせる
```

- branch 名は sashiki の `name_pattern`(`^[a-z0-9-]{1,32}$`)に合わせて整形している(`/`→`-`、32 文字まで)。
- worktree でアプリを起動 → その branch の DB に繋がる。**別の worktree は完全に別 DB**。
- 使っていない worktree の DB は **idle で自動停止**(mysqld が落ちてメモリ 0、再接続で起床)。
  worktree を 10 個持っていても、実際に走らせている数ぶんしかメモリを食わない。

### 管理コマンド(VM 内 or `limactl shell` 越し)

```bash
limactl shell sashiki-dev sashiki list          # ブランチ一覧
limactl shell sashiki-dev sashiki reset  my-feat # 作成時点へ戻す
limactl shell sashiki-dev sashiki recreate my-feat # 最新 baseline から作り直す
limactl shell sashiki-dev sashiki delete my-feat
```

CLI は VM 内の API(loopback)を叩くのでトークン不要。Mac から直接叩きたい場合は
[トークン](SPEC.md#20-4-api-トークン)を発行して `SASHIKI_API_URL` / `SASHIKI_API_TOKEN` を設定する。

## OrbStack のアプリから繋ぐ

- **OrbStack のコンテナ**から: `host.docker.internal:3306`(OrbStack は host.docker.internal を Mac host に解決する)。
- **Mac host のプロセス**(`go run` など)から: `127.0.0.1:3306`(Lima の port-forward 先)。
- ユーザーは `dev@<branch>`、パスワードは config の `app_pass`(Lima 内で `sashiki init` した場合はランダム生成。`--app-pass dev` で固定できる)。

## 後片付け

```bash
limactl stop sashiki-dev     # 一時停止(データは残る)
limactl delete -f sashiki-dev # VM ごと破棄
```

## これが向く / 向かない

- **向く**: baseline が大きい(数十 GB)、worktree を何本も並行、migration を実データで試す、reset を多用する。
- **向かない(Docker で十分)**: 軽い MySQL が 1 個欲しいだけ。その場合は素の container の方が VM 不要で楽。

判断の目安は [docs/COSTS.md](COSTS.md) を参照。

## VM を使わない経路(正式サポート、#113)

フル VM(Lima)を避けたい場合、ZFS の代わりに **CoW クローン + mysqld 直起動**で
動かせる。ZFS カーネル拡張は不要。macOS ネイティブ / OrbStack コンテナのどちらも
`create → reset → delete` と proxy lazy create を E2E で検証済み(macOS は
`make e2e-darwin`、コンテナは `deploy/orbstack/`)。

- **macOS ネイティブ**: `storage.backend: apfs`(APFS `clonefile`)+ `engine.mysql.mode: process`
  (mysqld を systemd 無しで直接 spawn)。Homebrew の mysql を使う。
- **OrbStack コンテナ**: `storage.backend: reflink`(XFS reflink `cp --reflink`)+ 同 `mode: process`。
  OrbStack カーネルには ZFS が無いため XFS reflink を CoW 基盤に使う(`deploy/orbstack/` 参照)。

config 例(macOS ネイティブ):

```yaml
storage:
  backend: apfs
  local:
    root: ~/Library/Application Support/sashiki   # APFS 上のルート
engine:
  type: mysql
  mysql:
    mode: process
    mysqld_bin: /opt/homebrew/opt/mysql/bin/mysqld
    app_user: dev
    app_pass: dev
```

一括セットアップは **`sashiki init --platform darwin`**(macOS では既定)が行う:
Homebrew mysql の検出 → `<root>/base/data` を mysqld で初期化しデータ投入 → 正常終了 →
`<root>/base/snap/baseline` を clonefile で取得 → config 生成 → launchd に sashikid を
常駐登録、まで自動。以降 `sashiki create` が clonefile で一瞬・省容量にブランチを生やす。

```bash
brew install mysql                            # 版は問わない(8.0 / 8.4 / 最新。実機検証は 8.0 / 8.4 / 26.7)
sashiki init --platform darwin --yes         # ~/Library/Application Support/sashiki に構築
sashiki create pr-1                           # clonefile で秒未満
mysql -udev@pr-1 -pdev -h 127.0.0.1 -P 3306   # proxy 経由。未知ブランチは lazy create
```

`baseline` を後から差し替えるなら **`sashiki baseline import --from dump.sql --config <root>/config.yaml`**
(macOS では root 不要。ログインユーザーで走る)、または稼働中に `sashiki baseline refresh`。

> 停止/削除: `launchctl unload ~/Library/LaunchAgents/dev.sashiki.sashikid.plist`。
> この VM レス経路の回帰テストは `make e2e-darwin`(使い捨て temp root で完結、launchd 非使用)。

> **per-branch quota の注意(#200)**: apfs / reflink には ZFS の refquota に相当する
> per-branch quota が無いため、`storage.default_storage_quota` は効かない。1 ブランチの
> 暴走を止める仕組みは無い。**pool 使用率の watermark も効かない**(apfs / reflink は使用量を
> 報告しないため)。Mac は個人用途なので通常は問題ないが、共有マシンで使うなら空き容量に余裕を持たせること。

### process モードと実行ユーザー(`run_user`)

process モードは `sashikid` を動かしているユーザーで `mysqld` を起動する。
`engine.mysql.run_user` は **root で sashikid を動かすときだけ**効く(#117)。

- **一般ユーザー実行(macOS の通常、rootless コンテナ)**: mysqld も同じユーザーで動くので
  `--user` は付かない。`run_user` は無視される。前提として **`storage.local.root` 配下
  (datadir / socket / pid-file / error.log)がそのユーザーで読み書きできる**こと。
  Homebrew の Mac ではログインユーザーで動かすのが基本で、`run_user` は設定不要。
- **root 実行(Linux コンテナで PID1=root 等)**: mysqld は root では起動を拒むため、
  `run_user`(既定 `mysql`)へ降格して `--user=<run_user>` を渡す。この場合は
  **datadir の所有権を `run_user` に合わせておく**(でないと mysqld が書けない)。

### PostgreSQL を VM レスで使う(#227)

MySQL と同じく、Postgres も **systemd 無し(process モード)** + ローカル CoW
(APFS `clonefile` / XFS reflink)で動く。

```yaml
storage:
  backend: apfs
  local:
    root: ~/Library/Application Support/sashiki-pg
engine:
  type: postgres
  postgres:
    mode: process                     # pg_ctl で直接起動(systemd 不要)
    bin_dir: /opt/homebrew/opt/postgresql@16/bin
    app_user: dev
    app_pass: dev
```

baseline は `sashiki baseline import --from dump.sql --db app`
で作れる(config は `~/Library/Application Support/sashiki-pg/config.yaml` を自動で使う。
MySQL 版の config も併存するなら `--config` か `SASHIKI_CONFIG` で指定する)(プレーン SQL / `pg_dump` のカスタム形式・ディレクトリ形式に対応)。
接続は `listen.proxy` を設定すれば `psql -U 'dev@pr-1'` の固定エンドポイント経由、
未設定ならブランチごとの直ポート(`sashiki show <name>`)。

一括セットアップは **`sashiki init --platform darwin --engine postgres`**(#238)。
Homebrew の postgresql を検出(版は数値順で最新)→ `initdb` → 接続ロール `dev` と
`app` データベース作成 → 正常終了 → clonefile で baseline 取得 → config 生成 →
launchd 常駐。ラベル `dev.sashiki.sashikid-pg`、API `:8081`、metrics `:9101` で、
MySQL 版(`:8080` / `:9100`、proxy `:3306`)と同時に常駐できる(#304)。CLI はどちらの
sashikid を操作するかを `SASHIKI_API_URL` で選ぶ(既定は MySQL 版の `:8080`)。

```bash
brew install postgresql@17
sashiki init --platform darwin --engine postgres --yes
export SASHIKI_API_URL=http://127.0.0.1:8081
PGPASSWORD=dev psql -h 127.0.0.1 -p 5432 -U 'dev@pr-1' -d app   # 未作成でも接続時に生える
```

> 実機(Apple Silicon / PostgreSQL 17 / APFS)で init → create → 直ポート接続 →
> proxy 経由の lazy create まで検証済み。

### 接続ユーザー(#131)

- クライアントは **`<user>@<branch>`** の形で `:3306` プロキシに接続し、パスワードは
  `engine.mysql.app_pass`(既定 `dev`)。例: `mysql -udev@pr-1 -pdev -h127.0.0.1 -P3306`。
- 既定では **app_user(既定 `dev`)のみ**が許可される。プロキシは `@<branch>` の
  branch 部でルーティングし、パスワードは `app_pass` で検証する。
- **任意のユーザー名を許可**したいときは `proxy.allowed_user: ""`(空文字)を設定する。
  特定名だけ許可したいなら `proxy.allowed_user: <name>`。未設定なら app_user のみ。
- **管理者権限が要る操作**(migration 等、authense の migrationdb は root 固定)は
  app_user では権限不足になりうる。`proxy.allowed_user: ""` にした上で baseline に
  管理ユーザーを用意し、`<admin>@<branch>` で接続する(パスワードは `app_pass` で検証)。

### macOS の警告と compose 定型(#132)

- **`lower_case_table_names=2` の警告**: APFS は大小非区別のため mysqld が起動時に
  自動で 2 を選び警告を出す。**無害**(datadir 初期化時に確定し一貫している)。気に
  なる場合は baseline を作る環境で明示しておく。
- **docker compose から sashiki の DB を使う**(worktree ごとに PR プレビュー):
  - compose の DB サービスは使わない(`depends_on` から外す)
  - アプリの接続先を `DB_HOST=host.docker.internal`(OrbStack/Docker Desktop が Mac host に解決)、`DB_PORT=3306`、`DB_USER=dev@pr-<n>`、`DB_PASSWORD=dev`
  - worktree ごとに `COMPOSE_PROJECT_NAME` を分けてコンテナ名の衝突を避ける(`.envrc` で
    worktree 名から設定すると楽)
