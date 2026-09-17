#!/usr/bin/env bash
# ベースライン更新テンプレート(仕様 12 章 / baseline.refresh_script)。自社向けに書き換えて
# /etc/sashiki/refresh.sh に置く。sashikid の POST /v1/baseline/refresh から呼ばれる。
#
# 責務: base の mysqld を起動 → 最新データ投入(PII はここでマスク) →
#       main の最新マイグレーション適用 → mysqld を正常終了。
# snapshot の取得と current の切り替えは sashikid 側が行う(スクリプトはやらない)。
# sashikid は snapshot 取得前に base の datadir を掴むプロセスが残っていないことを
# 検証し、残っていれば SIGTERM で回収してから進む(exit 0 は信用しない)。
#
# 環境変数: SASHIKI_BASELINE_TAG (例: baseline-20260905T120000Z)
set -euo pipefail

POOL=dbpool
DATADIR=/$POOL/base/data
SOCK=/tmp/sashiki-refresh.sock

sudo -u mysql mysqld --datadir="$DATADIR" --skip-networking \
  --socket="$SOCK" --pid-file=/tmp/sashiki-refresh.pid \
  --log-error=/var/log/sashiki/refresh.err --daemonize
for _ in $(seq 1 60); do
  mysqladmin -uroot -S "$SOCK" ping > /dev/null 2>&1 && break
  sleep 1
done

# --- ここを自社向けに書き換える ---------------------------------------
# 例1: 開発データパイプラインからの投入(PII マスク込み)
#   ./export-masked-dump.sh | mysql -uroot -S "$SOCK" app
# 例2: main の最新マイグレーション適用
#   DATABASE_URL="mysql://root@localhost/app?socket=$SOCK" ./bin/migrate up
mysql -uroot -S "$SOCK" -e "SELECT 1" > /dev/null
# ----------------------------------------------------------------------

# PII マスクを実施したら sentinel を touch(require_masked のとき publish 条件)
# mkdir -p /run/sashiki && touch /run/sashiki/baseline-masked

# 正常終了(必須): スナップショットは必ず正常終了状態でのみ取得する
mysqladmin -uroot -S "$SOCK" shutdown
sleep 2
