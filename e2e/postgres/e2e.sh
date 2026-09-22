#!/usr/bin/env bash
# PostgreSQL エンジンの E2E。素の Ubuntu (VM/EC2) 上で root 実行。
# loopback zpool + postgres 16 で create/reset/delete を検証する。
#   usage: sudo ./e2e.sh <sashikid> <sashiki>
set -euo pipefail

SASHIKID_BIN=${1:?usage: e2e.sh <sashikid> <sashiki>}
SASHIKI_BIN=${2:?usage: e2e.sh <sashikid> <sashiki>}
POOL=tpgpool
POOL_IMG=/var/tmp/sashiki-pg-zpool.img

log() { echo -e "\n=== $* ==="; }
fail() { echo "PG E2E FAILED: $*" >&2; tail -20 /var/log/sashiki-pg/sashikid.log 2>/dev/null; exit 1; }

log "packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -q > /dev/null
apt-get install -y -q zfsutils-linux postgresql postgresql-client > /dev/null 2>&1
systemctl stop postgresql 2>/dev/null || true
systemctl disable postgresql 2>/dev/null || true
PGBIN=$(ls -d /usr/lib/postgresql/*/bin | sort -V | tail -1)

log "cleanup previous run"
systemctl stop 'postgres-sashiki@*' 2>/dev/null || true
pkill -f "sashikid --config /etc/sashiki-pg" 2>/dev/null || true
zpool destroy $POOL 2>/dev/null || true
rm -f "$POOL_IMG"
rm -rf /etc/sashiki-pg /var/lib/sashiki-pg /var/log/sashiki-pg
mkdir -p /etc/sashiki-pg/hooks /var/lib/sashiki-pg/branches /var/log/sashiki-pg/hooks

log "zpool"
truncate -s 3G "$POOL_IMG"
zpool create -o ashift=12 $POOL "$POOL_IMG"
zfs set compression=lz4 atime=off $POOL

log "install binaries"
install -m 755 "$SASHIKID_BIN" /usr/local/bin/sashikid-pg
install -m 755 "$SASHIKI_BIN" /usr/local/bin/sashiki-pg

log "sashiki init --engine postgres (#224)"
# データセット作成・unit 配置・config 生成を init に任せて実機で検証する
# (パッケージはこのスクリプトが先に入れているので --skip-packages)。
rm -f /etc/sashiki/config.yaml
sashiki-pg init --engine postgres --pool $POOL --app-pass dev --skip-packages --yes \
  || fail "sashiki init --engine postgres が失敗した"
# postgres は 8KB ページ。base だけでなく branches 側にも要る(クローンは
# origin ではなく名前空間上の親からプロパティを継承するため)。
for ds in $POOL/base $POOL/branches; do
  rs=$(zfs get -H -o value recordsize $ds)
  [ "$rs" = "8K" ] || fail "$ds の recordsize が 8K でない (got: $rs)"
done
echo "  base / branches とも recordsize=8K"
grep -q "type: postgres" /etc/sashiki/config.yaml || fail "生成 config が postgres になっていない"
[ -f /etc/systemd/system/postgres-sashiki@.service ] || fail "postgres unit が配置されていない"
echo "  config(engine: postgres)と unit を生成済み"
# 冪等性: 2 回目は全ステップがスキップされて成功する
sashiki-pg init --engine postgres --pool $POOL --skip-packages --yes > /tmp/init2.log 2>&1 \
  || { cat /tmp/init2.log; fail "init の 2 回目(冪等)が失敗した"; }
grep -q "済み・スキップ" /tmp/init2.log || fail "2 回目にスキップされたステップが無い(冪等でない)"
echo "  2 回目は既存ステップをスキップ(冪等)"

log "install unit override + config"
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
UNIT_SRC="$SCRIPT_DIR/../../deploy/systemd/postgres-sashiki@.service"
[ -f "$UNIT_SRC" ] || UNIT_SRC="$SCRIPT_DIR/postgres-sashiki@.service"
# EnvironmentFile を e2e 専用ディレクトリ(config の env_dir)へ向ける。unit 側の
# 既定パスは変わり得る(#177 で /etc/sashiki → /run/sashiki へ移動した)ので、
# 特定パスではなく行ごと置換し、置換できたことを検証する。パターン不一致で
# 無言の no-op になると env が読まれず systemd の起動が謎に失敗するため。
sed -E "s|^EnvironmentFile=.*|EnvironmentFile=/etc/sashiki-pg/%i.env|" "$UNIT_SRC" \
  > /etc/systemd/system/postgres-sashiki@.service
grep -q '^EnvironmentFile=/etc/sashiki-pg/%i.env$' /etc/systemd/system/postgres-sashiki@.service \
  || fail "unit の EnvironmentFile を書き換えられなかった ($UNIT_SRC)"
systemctl daemon-reload

cat > /etc/sashiki-pg/config.yaml <<YAML
listen:
  api: "127.0.0.1:8090"
  proxy: "127.0.0.1:15432"
state_db: /var/lib/sashiki-pg/state.db
storage:
  backend: ebs-zfs
  ebs-zfs:
    pool: $POOL
    base_dataset: $POOL/base
    branch_parent: $POOL/branches
    baseline_snapshot: baseline
    sudo: false
engine:
  type: postgres
  postgres:
    bin_dir: $PGBIN
    env_dir: /etc/sashiki-pg
    sudo: false
    app_user: dev
    app_pass: dev
branches:
  name_pattern: "^[a-z0-9-]{1,32}$"
  max_branches: 5
hooks:
  dir: /etc/sashiki-pg/hooks
  log_dir: /var/log/sashiki-pg/hooks
YAML

log "baseline import (#223)"
# 手で initdb する代わりに sashiki baseline import を使う。initdb → ダンプ投入 →
# ロール作成 → 正常終了 → snapshot までを実コマンドで通し、これ自体を検証する。
cat > /var/tmp/sashiki-pg-dump.sql <<'SQL'
CREATE TABLE items (id SERIAL PRIMARY KEY, name TEXT);
INSERT INTO items (name) VALUES ('alpha'), ('beta'), ('gamma');
SQL
mkdir -p /$POOL/base
chown postgres:postgres /$POOL/base
# timeout を噛ませる: 万一ぶら下がっても CI を待たせず、サーバログを添えて落とす。
timeout 300 sashiki-pg baseline import --config /etc/sashiki-pg/config.yaml \
  --from /var/tmp/sashiki-pg-dump.sql --db app \
  || { echo "--- postgres baseline log ---"; tail -40 /tmp/sashiki-baseline-pg.log 2>/dev/null; \
       fail "baseline import が失敗した"; }
zfs list -t snapshot $POOL/base@baseline > /dev/null || fail "@baseline が取得されていない"
echo "  @baseline を取得済み"

/usr/local/bin/sashikid-pg --config /etc/sashiki-pg/config.yaml > /var/log/sashiki-pg/sashikid.log 2>&1 &
SASHIKID_PID=$!
trap 'kill $SASHIKID_PID 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do curl -sf http://127.0.0.1:8090/v1/healthz > /dev/null 2>&1 && break; sleep 0.5; done
curl -sf http://127.0.0.1:8090/v1/healthz > /dev/null || fail "sashikid did not start"

export SASHIKI_API_URL=http://127.0.0.1:8090
# import が host 認証を scram-sha-256 にするので、直ポート接続もパスワードが要る
# (以前の手動 initdb は trust だった)。dev ロールのパスワードは app_pass。
q() { PGPASSWORD=dev psql -h 127.0.0.1 -p "$1" -U dev -d app -t -A -c "$2" 2>/dev/null; }

log "create pg-1"
time sashiki-pg create pg-1
# q() は proxy を通さずブランチへ直結するので engine_port を使う
# (port は proxy 宛 + user は dev@<branch>、#260)。
PORT=$(sashiki-pg show pg-1 --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["engine_port"])')
[ "$(q $PORT 'SELECT COUNT(*) FROM items')" = "3" ] || fail "pg-1 should have 3 items"

log "破壊 → reset"
q $PORT "DELETE FROM items" > /dev/null
time sashiki-pg reset pg-1
[ "$(q $PORT 'SELECT COUNT(*) FROM items')" = "3" ] || fail "reset should restore"
# initdb 既定では logging_collector が無効でサーバーログは journald に行く。
# data/log を grep しても常にパスしてしまうので journal 側を確認する。
# パイプで grep -q に流すと pipefail × SIGPIPE で判定が化けるため変数に受ける(#49)。
pg_journal=$(journalctl -u 'postgres-sashiki@pg-1' --no-pager 2>/dev/null || true)
grep -qi "database system was not properly shut down" <<<"$pg_journal" \
  && fail "crash recovery ran (dirty @init)"
# チェック自体が生きていることの確認: 正常起動ログは journal に必ず出る
grep -qi "database system is ready to accept connections" <<<"$pg_journal" \
  || fail "journal に postgres のログが見つからない(crash recovery チェックが機能していない)"

log "データブラウザ API (#228)"
# schema / query が postgres でも動くこと(pgx 経由)。
schema=$(curl -sf http://127.0.0.1:8090/v1/branches/pg-1/schema) \
  || fail "schema API に失敗"
grep -q '"items"' <<<"$schema" || fail "schema に items テーブルが出るはず (got: $schema)"
grep -q '"databases"' <<<"$schema" || fail "schema の形式が想定と違う"
echo "  schema API が items を返す"
qres=$(curl -sf -X POST -H 'Content-Type: application/json' \
  -d '{"sql":"SELECT name FROM items ORDER BY id LIMIT 1"}' \
  http://127.0.0.1:8090/v1/branches/pg-1/query) || fail "query API に失敗"
grep -q "alpha" <<<"$qres" || fail "query API が結果を返さない (got: $qres)"
echo "  query API が結果を返す"

log "pgproxy: 固定エンドポイント経由 + lazy create (#222)"
# 未作成の pg-2 へ dev@pg-2 で接続すると、認証(SCRAM-SHA-256)が通ってから
# lazy create されてそのブランチに繋がる。psql は既定で SSL を試すので、
# proxy が 'N' を返して平文へ落ちる経路も同時に確認できる。
pq() { PGPASSWORD=dev psql -h 127.0.0.1 -p 15432 -U "$1" -d app -t -A -c "$2" 2>&1; }
# 「本当に lazy create されたか」を言えるように、接続前に存在しないことを確かめる。
sashiki-pg show pg-2 > /dev/null 2>&1 && fail "pg-2 は接続前には存在しないはず"
echo "  接続前: pg-2 は存在しない"

proxy_count=$(pq 'dev@pg-2' 'SELECT COUNT(*) FROM items') \
  || fail "proxy 経由の接続に失敗: $proxy_count"
echo "  proxy 経由 SELECT COUNT(*) FROM items => $proxy_count"
[ "$proxy_count" = "3" ] || fail "proxy 経由で 3 件見えるはず (got: $proxy_count)"
sashiki-pg show pg-2 > /dev/null || fail "lazy create で pg-2 が作られるはず"
echo "  接続後: pg-2 が lazy create されている"

# 認証終端: パスワードが違えば失敗し、かつブランチは作られない(#7/#51)
bad=$(PGPASSWORD=wrong psql -h 127.0.0.1 -p 15432 -U 'dev@pg-3' -d app -t -A -c 'SELECT 1' 2>&1 || true)
grep -qi "authentication failed" <<<"$bad" || fail "誤パスワードは弾かれるはず (got: $bad)"
sashiki-pg show pg-3 > /dev/null 2>&1 && fail "認証前に lazy create してはいけない (#7/#51)"
echo "  誤パスワードは拒否され、pg-3 は作られていない"

sashiki-pg delete pg-2 > /dev/null

log "delete"
time sashiki-pg delete pg-1
grep -q pg- <<<"$(zfs list -r $POOL/branches)" && fail "dataset should be destroyed"

log "組み込み refresh ローダー: source_dir/*.sql を baseline に適用 (#226)"
# refresh.sh を書かずに SQL を置くだけで baseline を更新できること。
# 適用記録は sashiki_meta スキーマ(postgres は DB を跨げないため)。
mkdir -p /etc/sashiki-pg/baseline-src
cat > /etc/sashiki-pg/baseline-src/001_add_color.sql <<'SQL'
ALTER TABLE items ADD COLUMN color TEXT;
UPDATE items SET color = 'red';
SQL
python3 - <<'PY'
import re, pathlib
p = pathlib.Path("/etc/sashiki-pg/config.yaml")
s = p.read_text()
s += """
baseline:
  source_dir: /etc/sashiki-pg/baseline-src
  source_db: app
"""
p.write_text(s)
PY
# sashikid に config を読み直させる
kill $SASHIKID_PID 2>/dev/null || true
for _ in $(seq 1 20); do curl -sf http://127.0.0.1:8090/v1/healthz >/dev/null 2>&1 || break; sleep 0.3; done
/usr/local/bin/sashikid-pg --config /etc/sashiki-pg/config.yaml > /var/log/sashiki-pg/sashikid-refresh.log 2>&1 &
SASHIKID_PID=$!
for _ in $(seq 1 30); do curl -sf http://127.0.0.1:8090/v1/healthz > /dev/null 2>&1 && break; sleep 0.5; done
curl -sf http://127.0.0.1:8090/v1/healthz > /dev/null || fail "refresh 用 sashikid が起動しない"

# refresh は非同期(202 を返して裏で走る)。完了は refreshing が false に戻るまで待つ。
timeout 180 sashiki-pg baseline refresh \
  || { tail -30 /var/log/sashiki-pg/sashikid-refresh.log; fail "baseline refresh の起動が失敗した"; }
for _ in $(seq 1 120); do
  curl -s http://127.0.0.1:8090/v1/baseline | grep -q '"refreshing":false' && break
  sleep 1
done
curl -s http://127.0.0.1:8090/v1/baseline | grep -q '"refreshing":false' \
  || { tail -40 /var/log/sashiki-pg/sashikid-refresh.log; fail "refresh が終わらない"; }
# current が新しい baseline(タグ付き)に切り替わっていること
curl -s http://127.0.0.1:8090/v1/baseline | grep -q "baseline-" \
  || { curl -s http://127.0.0.1:8090/v1/baseline; tail -40 /var/log/sashiki-pg/sashikid-refresh.log; \
       fail "current が新しい baseline に切り替わっていない"; }
echo "  refresh 完了・current 切り替え済み"
# 新しい baseline から作ったブランチに列が入っていること
sashiki-pg create pg-ref > /dev/null || fail "refresh 後の create に失敗"
RPORT=$(sashiki-pg show pg-ref --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["engine_port"])')
[ "$(q $RPORT "SELECT color FROM items LIMIT 1")" = "red" ] \
  || fail "refresh で追加した列が新ブランチに反映されていない"
echo "  新ブランチに追加列が反映されている"
# 冪等: もう一度 refresh しても二重適用にならない
timeout 180 sashiki-pg baseline refresh > /dev/null || fail "2 回目の refresh の起動が失敗した"
for _ in $(seq 1 120); do
  curl -s http://127.0.0.1:8090/v1/baseline | grep -q '"refreshing":false' && break
  sleep 1
done
curl -s http://127.0.0.1:8090/v1/baseline | grep -q '"refreshing":false' \
  || { tail -40 /var/log/sashiki-pg/sashikid-refresh.log; fail "2 回目の refresh が終わらない"; }
echo "  2 回目の refresh も成功(適用記録で冪等)"
sashiki-pg delete pg-ref > /dev/null

log "process モード: systemd 無しで起動する (#227)"
# 起動方式は storage backend と直交するので、ここでは zfs のまま mode だけ
# 差し替えて pg_ctl 直起動を検証する(macOS/コンテナで使う経路の本質は同じ)。
kill $SASHIKID_PID 2>/dev/null || true
for _ in $(seq 1 20); do curl -sf http://127.0.0.1:8090/v1/healthz >/dev/null 2>&1 || break; sleep 0.3; done
# 注入の目印は engine.postgres 節にしか無い行を使う。`sudo: false` は
# storage.zfs にも居るので、それを目印にすると両方に入ってしまい、
# zfs 側の `mode` は未知キーとして起動時に弾かれる(#300 の strict 化以降)。
sed -i 's/^    app_pass: dev$/    app_pass: dev\n    mode: process\n    run_user: postgres/' /etc/sashiki-pg/config.yaml
[ "$(grep -c 'mode: process' /etc/sashiki-pg/config.yaml)" = "1" ] \
  || fail "config への mode: process の注入が 1 箇所になっていない"
/usr/local/bin/sashikid-pg --config /etc/sashiki-pg/config.yaml > /var/log/sashiki-pg/sashikid-process.log 2>&1 &
SASHIKID_PID=$!
for _ in $(seq 1 30); do curl -sf http://127.0.0.1:8090/v1/healthz > /dev/null 2>&1 && break; sleep 0.5; done
curl -sf http://127.0.0.1:8090/v1/healthz > /dev/null \
  || { tail -40 /var/log/sashiki-pg/sashikid-process.log; fail "process モードで sashikid が起動しない"; }

time sashiki-pg create pg-proc \
  || { echo "--- postgres server log ---"; tail -30 /var/log/sashiki/postgres-pg-proc.log 2>/dev/null; \
       ls -ld /var/log/sashiki /tpgpool/branches/pg-proc/data 2>/dev/null; \
       fail "process モードで create できない"; }
PPORT=$(sashiki-pg show pg-proc --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["engine_port"])')
[ "$(q $PPORT 'SELECT COUNT(*) FROM items')" = "3" ] || fail "process モードのブランチに接続できない"
# systemd ユニットを使っていないこと(= 本当に直起動している)
systemctl is-active --quiet postgres-sashiki@pg-proc && fail "process モードなのに systemd ユニットが動いている"
echo "  systemd ユニット非使用で起動・接続 OK"
sashiki-pg reset pg-proc > /dev/null || fail "process モードで reset できない"
[ "$(q $PPORT 'SELECT COUNT(*) FROM items')" = "3" ] || fail "process モードの reset 後に接続できない"
sashiki-pg delete pg-proc > /dev/null || fail "process モードで delete できない"
echo "  create / reset / delete OK"

# #291: ConnCount が app ロールで pg_stat_activity を読めず「判定不能=使用中」の
# 保護に倒れ続けると、Postgres のブランチは一度も sleeping にならない。ここで
# idle 停止が実際に発火することを見る(MySQL 側の e2e と同じ形)。
log "reaper: idle 停止が Postgres でも発火する (#291)"
kill $SASHIKID_PID 2>/dev/null || true
for _ in $(seq 1 20); do curl -sf http://127.0.0.1:8090/v1/healthz >/dev/null 2>&1 || break; sleep 0.3; done
# 既定 profile(preview)の閾値が効くので、global と preview の両方を短くする。
sed -i "s/^  max_branches: 5$/  max_branches: 5\n  idle_stop_after: 3s\n  delete_after_idle: 15s\n  reaper_interval: 1s\n  profiles:\n    preview: { idle_stop_after: 3s, delete_after_idle: 15s }/" /etc/sashiki-pg/config.yaml
grep -q "reaper_interval: 1s" /etc/sashiki-pg/config.yaml || fail "config に reaper 設定を入れられなかった"
/usr/local/bin/sashikid-pg --config /etc/sashiki-pg/config.yaml > /var/log/sashiki-pg/sashikid-idle.log 2>&1 &
SASHIKID_PID=$!
for _ in $(seq 1 30); do curl -sf http://127.0.0.1:8090/v1/healthz > /dev/null 2>&1 && break; sleep 0.5; done
curl -sf http://127.0.0.1:8090/v1/healthz > /dev/null || fail "idle 設定で sashikid が起動しない"

sashiki-pg create pg-idle > /dev/null || fail "pg-idle を作れない"
sleep 6   # idle_stop_after(3s) + reaper 数周期
if ! grep -q "pg-idle.*sleeping" <<<"$(sashiki-pg list)"; then
  sashiki-pg list
  echo "--- sashikid log (connpoll) ---"; grep -i "connpoll" /var/log/sashiki-pg/sashikid-idle.log | tail -5
  fail "reaper: pg-idle should be sleeping (ConnCount が失敗して保護に倒れていないか)"
fi
grep -q "接続数の取得に .* 回連続で失敗" /var/log/sashiki-pg/sashikid-idle.log \
  && fail "connpoll が失敗し続けている(ConnCount の認証が通っていない)"
echo "  idle 停止 OK"
# 再接続(proxy 経由)で起きる
[ "$(pq 'dev@pg-idle' 'SELECT COUNT(*) FROM items')" = "3" ] || fail "reaper: reconnect should wake sleeping branch"
grep -q "pg-idle.*running" <<<"$(sashiki-pg list)" || fail "reaper: pg-idle should be running after reconnect"
echo "  再接続で起床 OK"
# TTL: 15 秒放置で自動削除
sleep 18
grep -q pg-idle <<<"$(sashiki-pg list)" && fail "reaper: pg-idle should be TTL-deleted"
echo "  TTL 削除 OK"

log "cleanup"
kill $SASHIKID_PID 2>/dev/null || true
zpool destroy $POOL
rm -f "$POOL_IMG"
echo "PG E2E PASSED"
