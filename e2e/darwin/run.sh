#!/usr/bin/env bash
# macOS ネイティブ(VM レス)E2E。APFS clonefile ストレージ + process モード
# mysqld で create→reset→delete と proxy lazy create を検証する(#113 / #139)。
#
# 前提: brew install mysql@8.0(このマシンの mysqld を使う。root 不要)。
# 使い方: ./e2e/darwin/run.sh <sashikid> <sashiki>
#
# launchd も既定の ~/Library/Application Support/sashiki も触らない。すべて
# 使い捨ての temp root(TMPDIR 配下)で完結し、終了時に片付ける。
set -euo pipefail

SASHIKID_BIN=${1:?usage: run.sh <sashikid> <sashiki>}
SASHIKI_BIN=${2:?usage: run.sh <sashikid> <sashiki>}

log() { echo -e "\n=== $* ==="; }
fail() { echo "DARWIN E2E FAILED: $*" >&2; [ -n "${ROOT:-}" ] && tail -30 "$ROOT/sashikid.out" 2>/dev/null; exit 1; }

# --- mysqld / client の解決(Homebrew mysql@8.0 優先) ---
BREW_PREFIX="$(brew --prefix mysql@8.0 2>/dev/null || true)"
# SASHIKI_MYSQLD で版を明示できる(クロス版チェック用: 8.0 / 8.4 / 9.x など)。
# app_user は caching_sha2_password で作るのでどの版でも通るはず(#version-compat)。
MYSQLD="${SASHIKI_MYSQLD:-}"
if [ -z "$MYSQLD" ]; then
  for c in "$BREW_PREFIX/bin/mysqld" /opt/homebrew/opt/mysql@8.0/bin/mysqld /opt/homebrew/opt/mysql@8.4/bin/mysqld /usr/local/opt/mysql@8.0/bin/mysqld; do
    [ -x "$c" ] && MYSQLD="$c" && break
  done
fi
[ -x "$MYSQLD" ] || fail "mysqld が見つかりません(SASHIKI_MYSQLD で指定 or brew install mysql@8.0)"
MYSQL="$(dirname "$MYSQLD")/mysql"
log "mysqld: $MYSQLD"

API_PORT=18080
PROXY_PORT=13306
export SASHIKI_API_URL="http://127.0.0.1:$API_PORT"

ROOT="$(mktemp -d "${TMPDIR:-/tmp}/sashiki-darwin-e2e.XXXXXX")"
SKPID=""
cleanup() {
  [ -n "$SKPID" ] && kill "$SKPID" 2>/dev/null || true
  # 起動しっぱなしの branch mysqld を掃除(datadir 配下の pid を SIGKILL)
  for p in "$ROOT"/branches/*/data/mysqld.pid; do
    [ -f "$p" ] && kill -9 "$(cat "$p")" 2>/dev/null || true
  done
  rm -rf "$ROOT"
}
trap cleanup EXIT

log "temp root: $ROOT"
mkdir -p "$ROOT/log" "$ROOT/run" "$ROOT/hooks"

# proxy を TLS 終端にして、クライアント TLS × backend 平文の経路(#394 の RSA 詰め替え)も通す。
# 平文接続は引き続き受けるので、既存の検査は --ssl-mode=DISABLED を明示して平文経路に固定する
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=localhost" \
  -keyout "$ROOT/proxy.key" -out "$ROOT/proxy.crt" >/dev/null 2>&1 || fail "openssl で自己署名証明書を作れない"

cat > "$ROOT/config.yaml" <<YAML
listen:
  api: "127.0.0.1:$API_PORT"
  proxy: "127.0.0.1:$PROXY_PORT"
  metrics: "127.0.0.1:19100"
state_db: $ROOT/state.db
run_dir: $ROOT/run
log_dir: $ROOT/log
domain: sashiki.local
storage:
  backend: apfs
  local:
    root: $ROOT
engine:
  type: mysql
  mysql:
    mode: process
    mysqld_bin: $MYSQLD
    sudo: false
    port_range: [13401, 13600]
    app_user: dev
    app_pass: dev
proxy:
  passthrough_users: [photo_app]   # #394: ブランチ内ユーザーの認証中継
  tls_cert: $ROOT/proxy.crt
  tls_key: $ROOT/proxy.key
branches:
  name_pattern: "^[a-z0-9-]{1,32}$"
  max_branches: 20
  lazy_create: true
hooks:
  dir: $ROOT/hooks
YAML

log "seed dump"
cat > "$ROOT/seed.sql" <<'SQL'
CREATE DATABASE IF NOT EXISTS app;
USE app;
CREATE TABLE items (id INT PRIMARY KEY AUTO_INCREMENT, name VARCHAR(64));
INSERT INTO items (name) VALUES ('alpha'), ('beta'), ('gamma');
CREATE DATABASE IF NOT EXISTS other;
CREATE TABLE other.secrets (id INT PRIMARY KEY);
-- #394: サービス別ユーザー(GRANT は app.* の SELECT だけ)。proxy は認証を backend に委ねる
CREATE USER 'photo_app'@'%' IDENTIFIED BY 'photo-secret';
GRANT SELECT ON app.* TO 'photo_app'@'%';
SQL

log "baseline import (non-root, apfs, clonefile)"
"$SASHIKI_BIN" baseline import --config "$ROOT/config.yaml" --from "$ROOT/seed.sql" || fail "baseline import"

log "start sashikid (background, no launchd)"
"$SASHIKID_BIN" --config "$ROOT/config.yaml" >"$ROOT/sashikid.out" 2>&1 &
SKPID=$!
for i in $(seq 1 50); do "$SASHIKI_BIN" list >/dev/null 2>&1 && break; sleep 0.3; done
"$SASHIKI_BIN" list >/dev/null 2>&1 || fail "sashikid が ready になりません"

q() { "$MYSQL" -udev@"$1" -pdev -h127.0.0.1 -P"$PROXY_PORT" -N -e "$2" 2>/dev/null; }

log "create pr-1"
"$SASHIKI_BIN" create pr-1 || fail "create"
[ "$(q pr-1 'SELECT COUNT(*) FROM app.items;')" = "3" ] || fail "初期データ 3 件のはず"

log "break -> reset -> データ復元"
"$MYSQL" -udev@pr-1 -pdev -h127.0.0.1 -P"$PROXY_PORT" -e "DELETE FROM app.items;" 2>/dev/null
[ "$(q pr-1 'SELECT COUNT(*) FROM app.items;')" = "0" ] || fail "破壊後は 0 件のはず"
"$SASHIKI_BIN" reset pr-1 || fail "reset"
[ "$(q pr-1 'SELECT COUNT(*) FROM app.items;')" = "3" ] || fail "reset で 3 件に戻るはず"

log "proxy lazy create (未知ブランチへ接続だけで自動作成)"
[ "$(q pr-lazy 'SELECT COUNT(*) FROM app.items;')" = "3" ] || fail "lazy create が効かない"

log "proxy passthrough: ブランチ内ユーザー photo_app で接続 (#394)"
# 期待どおりのエラー(1142/1045/1049)でも mysql は非ゼロで終わる。set -e / pipefail に
# 拾わせず出力だけを返す(判定は呼び出し側の grep で行う)
pt() { "$MYSQL" -uphoto_app@"$1" -p"$2" -h127.0.0.1 -P"$PROXY_PORT" --ssl-mode=DISABLED --get-server-public-key -N -e "$3" 2>&1 | grep -v "Using a password"; return 0; }
# TLS 終端あり: クライアントは平文パスワードを送り、proxy が backend(平文)向けに RSA へ詰め替える
ptls() { "$MYSQL" -uphoto_app@"$1" -p"$2" -h127.0.0.1 -P"$PROXY_PORT" --ssl-mode=REQUIRED -N -e "$3" 2>&1 | grep -v "Using a password"; return 0; }
# 初回はキャッシュが冷えていて full auth → RSA 公開鍵の中継(--get-server-public-key)
[ "$(pt pr-1 photo-secret 'SELECT COUNT(*) FROM app.items;')" = "3" ] || fail "passthrough: photo_app@pr-1 で読めるはず"
# 2 回目はキャッシュが温まっていて fast auth → フラグなしで通る
[ "$("$MYSQL" -uphoto_app@pr-1 -pphoto-secret -h127.0.0.1 -P"$PROXY_PORT" --ssl-mode=DISABLED -N -e 'SELECT COUNT(*) FROM app.items;' 2>/dev/null)" = "3" ] || fail "passthrough: 2 回目はフラグなしで通るはず"
# GRANT はそのユーザーのまま(app_user に化けていない)
out=$(pt pr-1 photo-secret 'SELECT COUNT(*) FROM other.secrets;'); grep -q "1142" <<<"$out" || fail "passthrough: GRANT 分離が保たれるはず(other.* は 1142): $out"
# 誤パスワードは backend が拒否する
out=$(pt pr-1 WRONG 'SELECT 1;'); grep -q "1045" <<<"$out" || fail "passthrough: 誤パスワードは 1045 のはず: $out"
# 未知ブランチは lazy create しない(lazy_create: true でも)
out=$(pt no-such photo-secret 'SELECT 1;'); grep -q "1049" <<<"$out" || fail "passthrough: 未知ブランチは Unknown branch のはず: $out"
"$SASHIKI_BIN" list 2>/dev/null | grep -q "no-such" && fail "passthrough: 未知ブランチを作ってはいけない"
# 列挙外のユーザーは入口で拒否
out=$("$MYSQL" -ugear_app@pr-1 -px -h127.0.0.1 -P"$PROXY_PORT" -e 'SELECT 1' 2>&1 || true); grep -q "1045" <<<"$out" || fail "passthrough: 列挙外ユーザーは拒否のはず: $out"
# sleeping からの起床が proxy 経由で効く(直結では効かなかった、issue の動機)
"$SASHIKI_BIN" sleep pr-1 || fail "sleep pr-1"
[ "$(pt pr-1 photo-secret 'SELECT COUNT(*) FROM app.items;')" = "3" ] || fail "passthrough: sleeping からの起床が効くはず"

log "proxy passthrough: TLS 終端 × full auth (#394 の RSA 詰め替え経路)"
# pr-tls は新しい mysqld なので photo_app のキャッシュは冷えている → backend は full auth を要求する。
# クライアントは TLS なので --get-server-public-key なしで平文パスワードを送り、proxy が RSA に詰め替える
"$SASHIKI_BIN" create pr-tls || fail "create pr-tls"
[ "$(ptls pr-tls photo-secret 'SELECT COUNT(*) FROM app.items;')" = "3" ] || fail "passthrough(TLS): 冷えたキャッシュでフラグなしで通るはず"
# 本当に TLS だったか(平文なら Ssl_cipher は空)
out=$("$MYSQL" -uphoto_app@pr-tls -pphoto-secret -h127.0.0.1 -P"$PROXY_PORT" --ssl-mode=REQUIRED -e '\s' 2>/dev/null | grep -i '^SSL:' || true)
grep -q "Cipher in use" <<<"$out" || fail "passthrough(TLS): クライアント側が TLS で接続しているはず: $out"
# 誤パスワードは TLS 経由でも backend の 1045(詰め替え後に backend が判定する)
"$SASHIKI_BIN" create pr-tls2 || fail "create pr-tls2"
out=$(ptls pr-tls2 WRONG 'SELECT 1;'); grep -q "1045" <<<"$out" || fail "passthrough(TLS): 誤パスワードは 1045 のはず: $out"
# 方式A(dev)も TLS で従来どおり
[ "$("$MYSQL" -udev@pr-tls -pdev -h127.0.0.1 -P"$PROXY_PORT" --ssl-mode=REQUIRED -N -e 'SELECT COUNT(*) FROM app.items;' 2>/dev/null)" = "3" ] || fail "方式A が TLS で通るはず"

log "delete"
"$SASHIKI_BIN" delete pr-tls || fail "delete pr-tls"
"$SASHIKI_BIN" delete pr-tls2 || fail "delete pr-tls2"
"$SASHIKI_BIN" delete pr-1 || fail "delete pr-1"
"$SASHIKI_BIN" delete pr-lazy || fail "delete pr-lazy"

log "DARWIN E2E PASSED"
