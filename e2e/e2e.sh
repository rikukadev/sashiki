#!/usr/bin/env bash
# sashiki E2E: 素の Ubuntu (VM / EC2) 上で root 実行する。
# ループバックファイルの zpool を使うので追加ディスク不要。
#   usage: sudo ./e2e.sh <sashikid-binary> <sashiki-binary>
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SASHIKID_BIN=${1:?usage: e2e.sh <sashikid> <sashiki>}
SASHIKI_BIN=${2:?usage: e2e.sh <sashikid> <sashiki>}
POOL=tpool
POOL_IMG=/var/tmp/sashiki-e2e-zpool.img

log() { echo -e "\n=== $* ==="; }
fail() { echo "E2E FAILED: $*" >&2; exit 1; }

# HTTP アサーション用(#49)。`curl | grep -q` は pipefail 下で壊れる:
# grep -q がマッチ即終了でパイプを閉じると、書き手が EPIPE / SIGPIPE で
# 失敗し、中身が正しくてもパイプライン全体が失敗する(発生はレース次第
# なので flake に見える)。パイプを使わず本文を変数に受けて判定する。
# 接続エラーとパターン不一致は 3 回まで再試行。
probe() { # probe <url> <grep-pattern> [curl-args...] : pattern 空なら 200 のみ確認
  local url=$1 pat=$2; shift 2
  local i body=""
  for i in 1 2 3; do
    if body=$(curl -sf "$@" "$url") && { [ -z "$pat" ] || grep -q -- "$pat" <<<"$body"; }; then
      return 0
    fi
    [ "$i" -lt 3 ] && sleep 1
  done
  echo "probe failed: $url pattern='${pat}' (HTTP $(curl -s -o /dev/null -w '%{http_code}' "$@" "$url" 2>/dev/null))" >&2
  echo "probe last body: $(head -c 200 <<<"$body")" >&2
  return 1
}

# --- 0. 前提パッケージ ---
log "packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -q > /dev/null
apt-get install -y -q zfsutils-linux mysql-server-8.0 mysql-client-8.0 apparmor-utils > /dev/null
systemctl stop mysql 2>/dev/null || true
systemctl disable mysql 2>/dev/null || true
# AppArmor は回避しない: sashiki init が生成する sashiki-mysqld プロファイル
# (enforce)の下でシナリオ全体を通す(#79)。旧 PoC の disable symlink が
# VM 使い回しで残っていても init が掃除する。

# --- 1. クリーンアップ(再実行安全) ---
log "cleanup previous run"
systemctl stop 'mysqld@*' 2>/dev/null || true
pkill -f "sashikid --config" 2>/dev/null || true
zpool destroy $POOL 2>/dev/null || true
rm -f "$POOL_IMG"
rm -rf /var/lib/sashiki /var/log/sashiki /etc/sashiki
rm -f /tmp/sashiki-hook-fixed /tmp/sashiki-action-out
mkdir -p /var/lib/sashiki/branches /var/log/sashiki/hooks /etc/sashiki/hooks

# --- 2. sashiki init (zpool/データセット/unit/config を作る) ---
log "sashiki install + init"
install -m 755 "$SASHIKID_BIN" /usr/local/bin/sashikid
install -m 755 "$SASHIKI_BIN" /usr/local/bin/sashiki
truncate -s 3G "$POOL_IMG"
sashiki init --pool $POOL --device "$POOL_IMG" --skip-packages --yes
zfs list $POOL/base $POOL/branches > /dev/null || fail "init should create datasets"
# 再実行安全であること(主要ステップがスキップされ成功する)
init2=$(sashiki init --pool $POOL --skip-packages --yes) || fail "init re-run should succeed"
grep -q "スキップ" <<<"$init2" || fail "init should be idempotent"
# AppArmor プロファイルと sudoers が生成されていること(詳細な enforce 検証は末尾 #79)
aa-status | grep -q 'sashiki-mysqld' || fail "apparmor: init should load sashiki-mysqld profile"
visudo -cf /etc/sudoers.d/sashiki || fail "sudoers: generated file should pass visudo (#78)"
grep -q "zfs destroy -r $POOL/branches/\*" /etc/sudoers.d/sashiki || fail "sudoers: destroy should be path-restricted (#78)"
# E2E 用にポートレンジと上限を絞る
sed -i 's/port_range: \[3401, 3600\]/port_range: [3401, 3410]/' /etc/sashiki/config.yaml
sed -i 's/max_branches: 50/max_branches: 5/' /etc/sashiki/config.yaml
# branch ごとに refquota 100M を課す(#85)
sed -i '/^  critical_watermark:/a\  default_storage_quota: 100M' /etc/sashiki/config.yaml

# --- 3. ベースライン: sashiki baseline import ---
log "sashiki baseline import"
cat > /tmp/sashiki-e2e-sample.sql <<'SQL'
CREATE DATABASE app;
CREATE TABLE app.items (id INT PRIMARY KEY AUTO_INCREMENT, name VARCHAR(64));
INSERT INTO app.items (name) VALUES ('alpha'), ('beta'), ('gamma');
SQL
sashiki baseline import --from /tmp/sashiki-e2e-sample.sql
zfs list $POOL/base@baseline > /dev/null || fail "baseline snapshot should exist"
# 二重 import は拒否されること
if sashiki baseline import --from /tmp/sashiki-e2e-sample.sql 2>/dev/null; then
  fail "second import should fail (baseline exists)"
fi

# --- 4. sashikid 起動 ---
log "start sashikid"
/usr/local/bin/sashikid --config /etc/sashiki/config.yaml > /var/log/sashiki/sashikid.log 2>&1 &
SASHIKID_PID=$!
trap 'kill $SASHIKID_PID 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do
  curl -sf http://127.0.0.1:8080/v1/healthz > /dev/null 2>&1 && break
  sleep 0.5
done
curl -sf http://127.0.0.1:8080/v1/healthz > /dev/null || fail "sashikid did not start: $(tail -5 /var/log/sashiki/sashikid.log)"

# --- 5. シナリオ ---
q() { mysql -udev -pdev -h127.0.0.1 -P"$1" -N -e "$2" 2>/dev/null; }

log "create pr-1"
time sashiki create pr-1
[ "$(q 3401 'SELECT COUNT(*) FROM app.items')" = "3" ] || fail "pr-1 should have 3 items"
# refquota が clone 直後に適用されていること(#85)
[ "$(zfs get -H -o value refquota $POOL/branches/pr-1)" = "100M" ] || fail "refquota 100M should be applied (#85)"

log "create pr-2 (isolation)"
sashiki create pr-2
q 3401 "DELETE FROM app.items; DROP TABLE app.items" || fail "break pr-1"
[ "$(q 3402 'SELECT COUNT(*) FROM app.items')" = "3" ] || fail "pr-2 must be isolated"

log "server_uuid must differ (#80: auto.cnf は baseline に含めない)"
uuid1=$(q 3401 'SELECT @@server_uuid'); uuid2=$(q 3402 'SELECT @@server_uuid')
[ -n "$uuid1" ] && [ "$uuid1" != "$uuid2" ] || fail "server_uuid duplicated: pr-1=$uuid1 pr-2=$uuid2"

log "reset pr-1"
time sashiki reset pr-1
[ "$(q 3401 'SELECT COUNT(*) FROM app.items')" = "3" ] || fail "reset should restore 3 items"
if grep -qi "crash recovery" /var/log/sashiki/pr-1.err; then
  fail "crash recovery ran (dirty @init)"
fi

log "duplicate create must fail (exit 4)"
set +e
sashiki create pr-1 2>/dev/null
rc=$?
set -e
[ "$rc" -eq 4 ] || fail "duplicate create: exit=$rc, want 4"

log "invalid name must fail"
if sashiki create "BAD_NAME" 2>/dev/null; then
  fail "invalid name should fail"
fi

log "list"
sashiki list

log "create --exist-ok は冪等 / env は dotenv を出す"
# hook から毎回叩かれる経路。冪等でないと利用者は `|| true` で実エラーごと
# 握り潰す回避に追い込まれる(#259)。
sashiki create pr-1 --exist-ok > /dev/null || fail "--exist-ok で既存が失敗した"
# 付けない場合は従来どおり 409 で落ちること(握り潰していないことの裏)
if sashiki create pr-1 > /dev/null 2>&1; then
  fail "--exist-ok 無しの重複 create は失敗すべき"
fi
envout=$(sashiki env pr-1) || fail "sashiki env が失敗した"
grep -qx "DB_HOST=sashiki.internal" <<<"$envout" || fail "env: DB_HOST がおかしい: $envout"
grep -qx "DB_PORT=3306" <<<"$envout" || fail "env: proxy のポートを出すべき: $envout"
grep -qx "DB_USER=dev@pr-1" <<<"$envout" || fail "env: user がおかしい: $envout"
grep -q "PASSWORD" <<<"$envout" && fail "env: パスワードを出してはいけない: $envout"
echo "  exist-ok 冪等 / env は接続に使える 3 つ組だけ"

log "proxy: dev@<branch> ルーティング"
# API が返す host/port/user は「そのまま繋がる 3 つ組」であること(#260)。
# 過去に port だけブランチ内部のものを返していて、どう解釈しても接続できない
# 値になっていた。ここは値を解釈せずそのまま psql/mysql に渡して確かめる。
conn=$(sashiki show pr-1 --json | python3 -c 'import json,sys;b=json.load(sys.stdin);print(b["port"],b["user"],b["engine_port"])')
read -r cport cuser ceport <<<"$conn"
[ "$cport" = "3306" ] || fail "conn: port=$cport, proxy のポートを返すべき (#260)"
[ "$ceport" != "3306" ] || fail "conn: engine_port はブランチ自身の listener であるべき"
val=$(mysql -u"$cuser" -pdev -h127.0.0.1 -P"$cport" -N -e "SELECT COUNT(*) FROM app.items" 2>/dev/null) \
  || fail "conn: API が返した接続情報で繋がらない (port=$cport user=$cuser)"
[ "$val" = "3" ] || fail "conn: query result = $val, want 3"
echo "  API の接続情報をそのまま使って接続できた (port=$cport user=$cuser engine_port=$ceport)"

# 固定ポート 3306 経由で pr-1 に接続できること
val=$(mysql -udev@pr-1 -pdev -h127.0.0.1 -P3306 -N -e "SELECT COUNT(*) FROM app.items" 2>/dev/null) \
  || fail "proxy: connect via dev@pr-1 should work"
[ "$val" = "3" ] || fail "proxy: query result = $val, want 3"
# name_pattern 違反のブランチ名は拒否(lazy create の対象にもならない)
if mysql -udev@Bad_Name -pdev -h127.0.0.1 -P3306 -e "SELECT 1" 2>/dev/null; then
  fail "proxy: invalid branch name should be rejected"
fi
# パスワード誤りは proxy が終端で拒否(方式A #51)
if mysql -udev@pr-1 -pWRONG -h127.0.0.1 -P3306 -e "SELECT 1" 2>/dev/null; then
  fail "proxy: wrong password should be rejected"
fi
# ブランチ名なしユーザーは拒否
if mysql -udev -pdev -h127.0.0.1 -P3306 -e "SELECT 1" 2>/dev/null; then
  fail "proxy: user without @branch should be rejected"
fi
# 方式A の肝(#51 / #7): 認証前に branch を作らない。誤パスワードで未知の
# branch 名へ接続しても、認証終端で弾かれて lazy create は走らない(DoS 構造の解消)。
if mysql -udev@pr-dos -pWRONG -h127.0.0.1 -P3306 -e "SELECT 1" 2>/dev/null; then
  fail "proxy: auth-before-create — wrong password must be rejected"
fi
sleep 1
grep -q "pr-dos" <<<"$(sashiki list)" && fail "proxy: 認証失敗した branch(pr-dos)は作られてはならない (#7 DoS)"

log "metrics & Web UI"
probe http://127.0.0.1:9100/metrics 'sashiki_branches{state="running"} 2' \
  || fail "metrics should report 2 running"
probe http://127.0.0.1:8080/ "sashiki" || fail "web ui should serve"

log "proxy: lazy create (未知ブランチ名で接続すると生える)"
val=$(mysql -udev@pr-lazy -pdev -h127.0.0.1 -P3306 -N -e "SELECT COUNT(*) FROM app.items" 2>/dev/null) \
  || fail "lazy create: connect should auto-create branch"
[ "$val" = "3" ] || fail "lazy create: query result = $val"
grep -q "pr-lazy" <<<"$(sashiki list)" || fail "lazy create: branch should appear in list"
sashiki delete pr-lazy

log "wake API"
sashiki create pr-wake > /dev/null
systemctl stop mysqld@pr-wake
# wake は冪等(running でも 200)なので再試行してよい
probe http://127.0.0.1:8080/v1/branches/pr-wake/wake "" -X POST || fail "wake should succeed"
val=$(mysql -udev@pr-wake -pdev -h127.0.0.1 -P3306 -N -e "SELECT 1" 2>/dev/null) || fail "wake: connect after wake"
[ "$val" = "1" ] || fail "wake: query"
# CLI の sleep / wake 露出(#87)
sashiki sleep pr-wake > /dev/null || fail "sashiki sleep should succeed"
sashiki show pr-wake --json | grep -q '"state":"sleeping"' || fail "sleep should set state=sleeping"
systemctl is-active --quiet mysqld@pr-wake && fail "sleep should stop mysqld"
sashiki wake pr-wake > /dev/null || fail "sashiki wake should succeed"
sashiki show pr-wake --json | grep -q '"state":"running"' || fail "wake should set state=running"
sashiki delete pr-wake

log "baseline refresh (current 切り替え)"
cat > /etc/sashiki/refresh.sh <<REFRESH
#!/usr/bin/env bash
set -euo pipefail
DATADIR=/$POOL/base/data
SOCK=/tmp/sashiki-refresh.sock
mysqld --user=mysql --datadir="\$DATADIR" --skip-networking --socket="\$SOCK" \
  --pid-file=/tmp/sashiki-refresh.pid --log-error=/var/log/sashiki/refresh.err --daemonize
for _ in \$(seq 1 60); do mysqladmin -uroot -S "\$SOCK" ping >/dev/null 2>&1 && break; sleep 1; done
mysql -uroot -S "\$SOCK" -e "INSERT INTO app.items (name) VALUES ('from-refresh')"
mysqladmin -uroot -S "\$SOCK" shutdown
sleep 2
REFRESH
chmod +x /etc/sashiki/refresh.sh
code=$(curl -s -o /tmp/refresh-resp -w '%{http_code}' -X POST http://127.0.0.1:8080/v1/baseline/refresh)
[ "$code" = "202" ] || { cat /tmp/refresh-resp; fail "refresh should return 202 (got $code)"; }
for _ in $(seq 1 60); do
  curl -s http://127.0.0.1:8080/v1/baseline | grep -q '"refreshing":false' && break
  sleep 1
done
probe http://127.0.0.1:8080/v1/baseline "baseline-" || fail "current should be rotated baseline"
# 新ブランチは新 baseline(4行)、既存 pr-1 は旧 baseline(3行)のまま
sashiki create pr-new > /dev/null
val=$(mysql -udev@pr-new -pdev -h127.0.0.1 -P3306 -N -e "SELECT COUNT(*) FROM app.items" 2>/dev/null)
[ "$val" = "4" ] || fail "new branch should see refreshed baseline (got $val)"
val=$(mysql -udev@pr-1 -pdev -h127.0.0.1 -P3306 -N -e "SELECT COUNT(*) FROM app.items" 2>/dev/null)
[ "$val" = "3" ] || fail "existing branch should keep old baseline (got $val)"
sashiki delete pr-new

log "baseline immutable / set / gc (#37)"
# refresh 済みなので baseline が2つ以上ある
curl -sf http://127.0.0.1:8080/v1/baselines | grep -q '"is_current":true' || fail "should have a current baseline"
NB=$(curl -sf http://127.0.0.1:8080/v1/baselines | grep -o '"snapshot"' | wc -l)
[ "$NB" -ge 2 ] || fail "should have >=2 baselines after refresh (got $NB)"
# gc --dry-run(#86): 実際には消さず、応答に dry_run:true(current/参照中は消えない前提)
sashiki baseline gc --dry-run | grep -q '"dry_run":true' || fail "baseline gc --dry-run should report dry_run"
NB2=$(curl -sf http://127.0.0.1:8080/v1/baselines | grep -o '"snapshot"' | wc -l)
[ "$NB2" -eq "$NB" ] || fail "dry-run must not delete baselines ($NB2 != $NB)"
# 負の keep_last は 400(#86)
sashiki baseline gc --keep-last -1 2>/dev/null && fail "negative keep_last should be rejected"
# gc: current と参照中は残る(pr-1 が旧baseline参照中)
sashiki baseline gc --keep-last 5 > /dev/null || fail "baseline gc"
# current baseline はまだ存在
sashiki baseline list | grep -q '\*' || fail "current baseline should remain after gc"

log "baseline 段階 API: build → validate → publish → delete (#84)"
# 検証後に元の current へ戻す(後続テストは current baseline の行数に依存するため)
prev=$(curl -sf http://127.0.0.1:8080/v1/baseline | python3 -c 'import json,sys;print(json.load(sys.stdin)["current"])')
snap=$(sashiki baseline build | sed -n 's/^baseline built: //p')
[ -n "$snap" ] || { sashiki baseline build; fail "baseline build should return a candidate snapshot"; }
sashiki baseline validate "$snap" > /dev/null || fail "baseline validate"
sashiki baseline publish "$snap" > /dev/null || fail "baseline publish"
# publish 後、その snapshot が current(*)であること
sashiki baseline list | grep -F "$snap" | grep -q '\*' || { sashiki baseline list; fail "published baseline should be current"; }
# current の delete は 412 で拒否
sashiki baseline delete "$snap" 2>/dev/null && fail "deleting the current baseline should be rejected"
# 元の current へ戻す
sashiki baseline publish "$prev" > /dev/null || fail "restore previous current baseline"
# 未 publish の candidate($snap)を delete できること
sashiki baseline delete "$snap" > /dev/null || fail "should delete an unpublished candidate"
grep -qF "$snap" <<<"$(sashiki baseline list)" && fail "deleted candidate should be gone"

log "recreate (最新baselineから作り直し)"
# refresh 済みなので current は新baseline(4行)。既存 pr-1 は旧(3行)
val=$(mysql -udev@pr-1 -pdev -h127.0.0.1 -P3306 -N -e "SELECT COUNT(*) FROM app.items" 2>/dev/null)
[ "$val" = "3" ] || fail "pr-1 should still be on old baseline (got $val)"
sashiki recreate pr-1 > /dev/null || fail "recreate should succeed"
val=$(mysql -udev@pr-1 -pdev -h127.0.0.1 -P3306 -N -e "SELECT COUNT(*) FROM app.items" 2>/dev/null)
[ "$val" = "4" ] || fail "recreate should move pr-1 to current baseline (got $val)"
# recreate 後に reset が成功すること(@init が存在する検証 = Fix 2)
sashiki reset pr-1 > /dev/null || fail "reset after recreate should work (valid @init)"

log "github action entrypoint (create/idempotent/delete)"
AE="$SCRIPT_DIR/../action/entrypoint.sh"
[ -f "$AE" ] || AE="$SCRIPT_DIR/action-entrypoint.sh"   # Lima はフラットコピー
[ -f "$AE" ] || fail "action entrypoint not found"
export SASHIKI_API_URL=http://127.0.0.1:8080 SASHIKI_BRANCH=pr-77
export SASHIKI_PR=77 GITHUB_REPOSITORY=example/app   # source 自動生成の材料 (#46)
: > /tmp/sashiki-action-out
SASHIKI_EVENT=opened SASHIKI_OUTPUT=/tmp/sashiki-action-out bash "$AE" || fail "action: opened should create"
grep -q "created=true" /tmp/sashiki-action-out || fail "action: created=true expected"
# source が自動生成され provenance として残る (#46)
grep -q '"type":"github_pr"' <<<"$(sashiki show pr-77 --json)" || fail "action: github_pr source should be recorded"
: > /tmp/sashiki-action-out
SASHIKI_EVENT=synchronize SASHIKI_OUTPUT=/tmp/sashiki-action-out bash "$AE" || fail "action: synchronize should succeed on existing branch"
grep -q "created=false" /tmp/sashiki-action-out || fail "action: created=false expected for existing"
# action: reset を明示指定(ラベル駆動想定、#244)。イベントではなく action が優先される。
# ここは proxy を通さずブランチへ直結するので engine_port を使う(port は proxy 宛、#260)。
mysql -udev -pdev -h127.0.0.1 -P"$(sashiki show pr-77 --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["engine_port"])')" \
  -e "DELETE FROM app.items" 2>/dev/null || fail "action: reset 検証の準備(削除)に失敗"
: > /tmp/sashiki-action-out
SASHIKI_ACTION=reset SASHIKI_OUTPUT=/tmp/sashiki-action-out bash "$AE" || fail "action: reset should succeed"
grep -q "^host=" /tmp/sashiki-action-out || fail "action: reset should emit connection info"
rp=$(sashiki show pr-77 --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["engine_port"])')
[ "$(mysql -udev -pdev -h127.0.0.1 -P"$rp" -N -B -e 'SELECT COUNT(*) FROM app.items' 2>/dev/null)" -ge 1 ] \
  || fail "action: reset should restore rows"
echo "  action=reset でブランチが作成時点に戻った"
# action 明示は on_close=keep より優先される(delete を明示したら消す)
SASHIKI_ACTION=delete SASHIKI_ON_CLOSE=keep bash "$AE" || fail "action: explicit delete should succeed"
sashiki show pr-77 > /dev/null 2>&1 && fail "action: explicit delete should remove the branch"
echo "  action=delete は on_close=keep より優先される"
# 消したので以降の keep/closed 検証のために作り直す
SASHIKI_EVENT=opened bash "$AE" > /dev/null || fail "action: recreate for the remaining checks"

# on_close=keep は削除しない (#46)
SASHIKI_EVENT=closed SASHIKI_ON_CLOSE=keep bash "$AE" || fail "action: on_close=keep should succeed"
sashiki show pr-77 --json > /dev/null || fail "action: on_close=keep should not delete the branch"
SASHIKI_EVENT=closed bash "$AE" || fail "action: closed should delete"
SASHIKI_EVENT=closed bash "$AE" || fail "action: closed should be idempotent (404 OK)"
unset SASHIKI_API_URL SASHIKI_BRANCH SASHIKI_PR GITHUB_REPOSITORY

log "capacity / logical size (#40)"
curl -sf http://127.0.0.1:8080/v1/capacity | grep -q '"pool_used_ratio"' || fail "capacity should report pool ratio"
# CLI は整形表示(#128)。--json は生レスポンス素通し。
sashiki capacity | grep -q '^storage:' || fail "capacity CLI (formatted)"
sashiki capacity --json | grep -q 'pool_total_bytes' || fail "capacity CLI --json"
# CoW: logical(referenced)は private(used)より大きい
sashiki show pr-1 --json | grep -q '"logical_bytes"' || fail "should report logical size"

log "provenance / metadata (#35)"
sashiki create prov-test --owner alice --purpose review --source '{"type":"github_pr","ref":"42"}' > /dev/null || fail "create with provenance"
sashiki show prov-test --json | grep -q '"owner":"alice"' || fail "owner should be stored"
sashiki show prov-test --json | grep -q '"purpose":"review"' || fail "purpose should be stored"
sashiki show prov-test --json | grep -q 'github_pr' || fail "source (opaque) should round-trip"
sashiki delete prov-test > /dev/null

log "create --baseline (#82: 指定 baseline から作成)"
sashiki create bl-test --baseline "$POOL/base@baseline" > /dev/null || fail "create --baseline should work"
grep -q bl-test <<<"$(sashiki list)" || fail "bl-test should be created from the given baseline"
sashiki delete bl-test > /dev/null
# 未登録 baseline は失敗し、branch を残さない
sashiki create bl-bad --baseline "nope@nope" 2>/dev/null && fail "create --baseline unknown should fail"
grep -q bl-bad <<<"$(sashiki list)" && fail "failed create --baseline must not leave a branch"

log "profile / lease (#34)"
# 既定 profile(preview)が付く
sashiki create prof-def > /dev/null || fail "create with default profile"
sashiki show prof-def --json | grep -q '"profile":"preview"' || fail "default profile should be preview"
# 明示 profile + 初期 lease(--ttl)
sashiki create prof-ci --profile ci --ttl 1h > /dev/null || fail "create --profile ci --ttl"
sashiki show prof-ci --json | grep -q '"profile":"ci"' || fail "profile ci should be stored"
exp1=$(sashiki show prof-ci --json | grep -o '"expires_at":"[^"]*"')
[ -n "$exp1" ] || fail "--ttl should set expires_at"
# lease renew で期限を延長(expires_at が変わる)
sleep 1
sashiki lease renew prof-ci --for 48h > /dev/null || fail "lease renew"
exp2=$(sashiki show prof-ci --json | grep -o '"expires_at":"[^"]*"')
[ -n "$exp2" ] || fail "lease renew should keep expires_at set"
[ "$exp1" != "$exp2" ] || fail "lease renew should change expires_at"
# 未知 profile は拒否
sashiki create prof-bad --profile nope 2>/dev/null && fail "unknown profile should be rejected"
sashiki delete prof-def > /dev/null
sashiki delete prof-ci > /dev/null

log "error model + retry (hook失敗→修正→retry)"
rm -f /tmp/sashiki-hook-fixed   # VM 使い回しの残骸を排除
# 失敗する on-create フックを置く
cat > /etc/sashiki/hooks/on-create.sh <<'HOOK'
#!/bin/sh
[ -f /tmp/sashiki-hook-fixed ] && exit 0
exit 1
HOOK
chmod +x /etc/sashiki/hooks/on-create.sh
sashiki create err-test 2>/dev/null && fail "create should fail on hook error"
# error 状態で診断が付く
sashiki show err-test --json | grep -q '"error_code":"hook_failed"' || fail "should record hook_failed"
sashiki show err-test --json | grep -q '"recoverable":true' || fail "hook_failed should be recoverable"
# フックを直して retry
touch /tmp/sashiki-hook-fixed
sashiki retry err-test > /dev/null || fail "retry should succeed after fixing hook"
sashiki show err-test | grep -qE "state: +running" || fail "err-test should be running after retry"
sashiki delete err-test > /dev/null
rm -f /etc/sashiki/hooks/on-create.sh /tmp/sashiki-hook-fixed

log "operations 記録 (async ops API)"
# operation id で決定的に検証(op list の grep はタイミングに脆い)
opid=$(curl -sf -D - -o /dev/null -X POST http://127.0.0.1:8080/v1/branches \
  -H "Content-Type: application/json" -d '{"name":"op-test"}' \
  | tr -d '\r' | awk 'tolower($1)=="sashiki-operation-id:"{print $2}')
[ -n "$opid" ] || fail "create should return operation id header"
curl -sf "http://127.0.0.1:8080/v1/operations/$opid" | grep -q '"target":"op-test"' \
  || fail "operation should be queryable by id"
curl -sf "http://127.0.0.1:8080/v1/operations/$opid" | grep -q '"type":"create"' \
  || fail "operation type should be create"
# op wait の --timeout フラグ(#83)。create は完了済みなので即 0 で返る
sashiki op wait "$opid" --timeout 30s --interval 100ms > /dev/null || fail "op wait --timeout should succeed for a finished op"
# 不正な timeout 書式は usage エラー(exit 2)
sashiki op wait "$opid" --timeout nonsense 2>/dev/null && fail "op wait should reject a bad --timeout"
sashiki delete op-test > /dev/null

log "reconciliation + doctor (#42)"
# doctor が健全性を返す(#88: 新項目 + [OK]/[WARN]/[ERROR] 整形)
docjson=$(curl -sf http://127.0.0.1:8080/v1/doctor)
grep -q '"current_baseline"' <<<"$docjson" || fail "doctor should report current_baseline"
grep -q '"state_db_writable":true' <<<"$docjson" || fail "doctor: state.db should be writable"
grep -q '"pool_status_healthy":true' <<<"$docjson" || fail "doctor: pool status should be healthy"
grep -q '"checks"' <<<"$docjson" || fail "doctor should include checks array"
docout=$(sashiki doctor)  # healthy 環境なので終了コード 0
echo "$docout" | grep -q '\[OK\]' || fail "doctor CLI should render [OK] lines"
echo "$docout" | grep -q 'state.db writable' || fail "doctor CLI should show state.db check"
echo "$docout" | grep -q '^branches: ' || fail "doctor CLI should show branch count"
sashiki doctor --json | grep -q '"branch_count"' || fail "doctor --json should pass through raw JSON"
# sashikid を kill して再起動 → running だった branch は sleeping に整合(mysqld も残る場合あり)
sashiki create recon-test > /dev/null
kill $SASHIKID_PID 2>/dev/null; sleep 1
# recon-test の mysqld を止める(プロセス死亡を再現)
systemctl stop mysqld@recon-test 2>/dev/null || true
/usr/local/bin/sashikid --config /etc/sashiki/config.yaml > /var/log/sashiki/sashikid.log 2>&1 &
SASHIKID_PID=$!
for _ in $(seq 1 30); do curl -sf http://127.0.0.1:8080/v1/healthz > /dev/null 2>&1 && break; sleep 0.5; done
# reconcile で running → sleeping に降格しているはず
st=$(sashiki show recon-test --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["state"])')
[ "$st" = "sleeping" ] || fail "recon-test should be reconciled to sleeping (got $st)"
sashiki delete recon-test > /dev/null

log "drain (#28: instance_class 変更前に全 branch を sleeping)"
# pr-1/pr-2 は running。drain で両方 sleeping になる。
sashiki show pr-1 --json | grep -q '"state":"running"' || fail "pr-1 should be running before drain"
sashiki drain > /dev/null || fail "drain should succeed"
sashiki show pr-1 --json | grep -q '"state":"sleeping"' || fail "drain: pr-1 should be sleeping"
sashiki show pr-2 --json | grep -q '"state":"sleeping"' || fail "drain: pr-2 should be sleeping"
# 再接続で復帰する(データは残っている)
mysql -udev@pr-1 -pdev -h127.0.0.1 -P3306 -N -e "SELECT 1" >/dev/null 2>&1 || fail "drain: pr-1 should wake on reconnect"

log "delete"
sashiki delete pr-1
sashiki delete pr-2
grep -q pr- <<<"$(zfs list -r $POOL/branches)" && fail "datasets should be destroyed"

log "token 管理"
out=$(sashiki token create --name e2e-test) || fail "token create"
tok=$(echo "$out" | grep -o "sashiki_[0-9a-f]*")
[ -n "$tok" ] || fail "token: 平文が表示されるべき"
grep -q e2e-test <<<"$(sashiki token list)" || fail "token list"
# 非 loopback からの検証は環境上できないため、DB トークンの受理はユニットテストで担保
sashiki token revoke e2e-test || fail "token revoke"
grep -q e2e-test <<<"$(sashiki token list)" && fail "token should be revoked"

log "idle stop & TTL (リーパー)"
# 短い閾値で sashikid を再起動
kill $SASHIKID_PID 2>/dev/null || true
sleep 1
cp /etc/sashiki/config.yaml /tmp/sashiki-config.bak
# 行頭2スペースの global キーだけを書き換える(profiles ブロック内の
# 「    preview: { idle_stop_after: 30m, ... }」に誤マッチさせない)。
sed -i "s/^  idle_stop_after: 30m.*/  idle_stop_after: 3s/; s/^  delete_after_idle: 168h.*/  delete_after_idle: 15s/" /etc/sashiki/config.yaml
sed -i "/^  delete_after_idle: 15s/a\\  reaper_interval: 1s" /etc/sashiki/config.yaml
# pr-idle は profile 未指定 → 既定 profile(preview)が付くため、reaper は
# preview の閾値を使う。テスト用に preview も短縮する(profile 経路の検証を兼ねる)。
sed -i "s/^    preview: .*/    preview: { idle_stop_after: 3s, delete_after_idle: 15s }/" /etc/sashiki/config.yaml
grep -A1 "delete_after_idle" /etc/sashiki/config.yaml
/usr/local/bin/sashikid --config /etc/sashiki/config.yaml > /var/log/sashiki/sashikid2.log 2>&1 &
SASHIKID_PID=$!
sleep 1
sashiki create pr-idle > /dev/null
sleep 6   # idle_stop_after(3s) + リーパー数周期
grep -q "pr-idle.*sleeping" <<<"$(sashiki list)" || { sashiki list; fail "reaper: pr-idle should be sleeping" ; }
systemctl is-active --quiet mysqld@pr-idle && fail "reaper: mysqld should be stopped"
# 再接続で起床(sleeping → running)
val=$(mysql -udev@pr-idle -pdev -h127.0.0.1 -P3306 -N -e "SELECT COUNT(*) FROM app.items" 2>/dev/null) \
  || fail "reaper: reconnect should wake sleeping branch"
[ "$val" = "4" ] || fail "reaper: wake query = $val (refresh後のbaselineは4行)"
# TTL: 15 秒放置で自動削除
sleep 18
grep -q pr-idle <<<"$(sashiki list)" && fail "reaper: pr-idle should be TTL-deleted"

# #41: proxy を通らない直接接続でも idle stop を防げること(engine ポーリングで
# last_conn_at を更新する)。proxy(3306)ではなく branch の実ポートへ直接つなぐと
# proxy の activeConns には出ないため、これは connpoll でしか検出できない。
log "reaper: 直接接続は idle stop を防ぐ (#41)"
sashiki create pr-hold > /dev/null
holdport=$(sashiki show pr-hold --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["engine_port"])')
# proxy を通さず branch 実ポートへ直接、長い接続を張る(SLEEP は余裕をもって 30s)
mysql -udev -pdev -h127.0.0.1 -P"$holdport" -e "SELECT SLEEP(30)" >/dev/null 2>&1 &
holdpid=$!
# 接続が確立して SLEEP が走り始めるまで待つ(これを待たずに idle 判定へ入ると
# connpoll がまだ接続を観測できておらず reaper に寝かされて flaky になる)。
for _ in $(seq 1 30); do
  n=$(mysql -udev -pdev -h127.0.0.1 -P"$holdport" -N -e \
    "SELECT COUNT(*) FROM information_schema.processlist WHERE info LIKE 'SELECT SLEEP%'" 2>/dev/null)
  [ "${n:-0}" -ge 1 ] && break
  sleep 0.3
done
sleep 6   # idle_stop_after(3s)+ connpoll(1s周期)を十分に跨ぐ
grep -q "pr-hold.*running" <<<"$(sashiki list)" \
  || { sashiki list; fail "reaper: 直接接続中の pr-hold は running のままであるべき (#41)"; }
kill "$holdpid" 2>/dev/null || true
wait "$holdpid" 2>/dev/null || true
sashiki delete pr-hold > /dev/null

# 設定を戻して再起動
kill $SASHIKID_PID 2>/dev/null || true
sleep 1
cp /tmp/sashiki-config.bak /etc/sashiki/config.yaml
/usr/local/bin/sashikid --config /etc/sashiki/config.yaml >> /var/log/sashiki/sashikid.log 2>&1 &
SASHIKID_PID=$!
sleep 1

# --- AppArmor enforce の実効性 (#79) ---
log "apparmor enforce"
# (1) sashiki-mysqld プロファイルが enforce モードでロードされていること
aa-status | sed -n '/profiles are in enforce mode/,/profiles are in complain mode/p' \
  | grep -q 'sashiki-mysqld' || { aa-status; fail "apparmor: sashiki-mysqld should be in enforce mode"; }

# (2) branch の mysqld プロセスが実際に confinement 下にあること
sashiki create pr-aa > /dev/null
aapid=$(systemctl show -p MainPID --value mysqld@pr-aa)
{ [ -n "$aapid" ] && [ "$aapid" != "0" ]; } || fail "apparmor: mysqld@pr-aa should be running"
grep -q 'sashiki-mysqld (enforce)' "/proc/$aapid/attr/current" \
  || fail "apparmor: mysqld@pr-aa should be confined (got: $(cat "/proc/$aapid/attr/current"))"
sashiki delete pr-aa > /dev/null

# (3) 負のテスト: datadir 外(/var/lib/mysql)への書込が拒否されること。
# 通常の branch mysqld は deb 既定の secure_file_priv(/var/lib/mysql-files)で
# OUTFILE が MySQL 側で弾かれ AppArmor の検証にならないため、secure_file_priv を
# 無効化した一時 mysqld を base datadir で起動して確認する。
# この mysqld は必ず正常終了させる(以後 base の snapshot は取得しないため
# baseline 不変条件には影響しない)。
install -d -o mysql -g mysql /var/lib/mysql   # DAC では書ける状態を保証(拒否 = AppArmor 起因)
mysqld --user=mysql --datadir=/$POOL/base/data --skip-networking \
  --socket=/tmp/sashiki-aa.sock --pid-file=/tmp/sashiki-aa.pid \
  --log-error=/var/log/sashiki/aa-test.err --secure-file-priv= --daemonize \
  || { tail -30 /var/log/sashiki/aa-test.err; fail "apparmor: temp mysqld should start under enforce"; }
for _ in $(seq 1 60); do mysqladmin -uroot -S /tmp/sashiki-aa.sock ping >/dev/null 2>&1 && break; sleep 1; done
# 正の対照: 許可パス(base datadir)への OUTFILE は成功する
mysql -uroot -S /tmp/sashiki-aa.sock \
  -e "SELECT 1 INTO OUTFILE '/$POOL/base/data/sashiki-aa-allowed.txt'" \
  || fail "apparmor: write inside datadir should be allowed"
# 負: /var/lib/mysql(datadir 外)への OUTFILE は AppArmor が拒否する
if mysql -uroot -S /tmp/sashiki-aa.sock \
  -e "SELECT 1 INTO OUTFILE '/var/lib/mysql/sashiki-aa-denied.txt'" 2>/dev/null; then
  fail "apparmor: write outside datadir should be denied"
fi
[ ! -f /var/lib/mysql/sashiki-aa-denied.txt ] || fail "apparmor: denied file should not exist"
mysqladmin -uroot -S /tmp/sashiki-aa.sock shutdown || fail "apparmor: temp mysqld should shut down cleanly"
# datadir を掴んだまま zpool destroy(後片付け)に進まないよう終了を待つ
for _ in $(seq 1 30); do [ ! -f /tmp/sashiki-aa.pid ] && break; sleep 1; done
[ ! -f /tmp/sashiki-aa.pid ] || fail "apparmor: temp mysqld did not exit"
rm -f "/$POOL/base/data/sashiki-aa-allowed.txt"

log "baseline export / import-stream の往復 (#243)"
# zfs send で書き出し、いったん base を捨ててから recv で戻す。
# ブランチが残っていると clone があって base を置き換えられないので、
# 「先に消せ」と言われることも確認する。
sashiki baseline export --to /var/tmp/baseline.zfs || fail "baseline export に失敗"
[ -s /var/tmp/baseline.zfs ] || fail "export したストリームが空"
echo "  export したストリーム: $(du -h /var/tmp/baseline.zfs | cut -f1)"

# ブランチが居るうちは import-stream を拒否すること(安全側)。
# この節は e2e 末尾にあり既存ブランチが片付いているので、判定用に 1 本作る。
sashiki create pr-guard > /dev/null || fail "拒否テスト用のブランチを作れない"
if sashiki baseline import-stream --from /var/tmp/baseline.zfs --force > /tmp/is.log 2>&1; then
  cat /tmp/is.log; fail "ブランチが残っている間は import-stream を拒否すべき"
fi
grep -q "ブランチが" /tmp/is.log || { cat /tmp/is.log; fail "拒否理由がブランチ残存であるべき"; }
echo "  ブランチ残存時は拒否される"

# ブランチを片付けてから受け入れる
for b in $(sashiki list --json | python3 -c 'import json,sys;d=json.load(sys.stdin);print(" ".join(x["name"] for x in d.get("branches") or []))'); do
  sashiki delete "$b" > /dev/null
done
sashiki baseline import-stream --from /var/tmp/baseline.zfs --force \
  || fail "baseline import-stream に失敗"
# 受け取った baseline から作れて、中身も戻っていること
sashiki create pr-recv > /dev/null || fail "import-stream 後に create できない"
RP=$(sashiki show pr-recv --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["engine_port"])')
[ "$(mysql -udev -pdev -h127.0.0.1 -P$RP -N -B -e 'SELECT COUNT(*) FROM app.items' 2>/dev/null)" -ge 1 ] \
  || fail "import-stream したデータが読めない"
echo "  recv したベースラインからブランチを作れる"
sashiki delete pr-recv > /dev/null
rm -f /var/tmp/baseline.zfs

# --- 6. 後片付け ---
log "cleanup"
kill $SASHIKID_PID 2>/dev/null || true
zpool destroy $POOL
rm -f "$POOL_IMG"

echo -e "\nE2E PASSED"
