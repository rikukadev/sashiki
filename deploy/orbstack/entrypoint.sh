#!/usr/bin/env bash
# コンテナ内で XFS reflink 領域を用意し、baseline を構築して sashikid を常駐させる。
# privileged コンテナで動かす前提(loopback XFS のマウントに必要)。
set -euo pipefail

ROOT=/var/lib/sashiki-data     # reflink 対応 FS(XFS)をここにマウントする
# 永続化する物は全部 STORE 配下に置く(#290)。compose はここに named volume を
# 当てる。XFS イメージ・state.db・config が writable layer にあると
# `docker compose down` で全部消えるので、消えて困る物はここ以外に置かない。
STORE=${SASHIKI_STORE:-/xfs-store}
IMG=$STORE/xfs.img
STATE_DB=$STORE/state.db
CONFIG=$STORE/config.yaml

mkdir -p "$STORE"
if ! mountpoint -q "$STORE"; then
  echo "WARN: $STORE が volume ではありません。コンテナを消すとブランチ・baseline・state.db も消えます" >&2
  echo "      (compose は sashiki-xfs volume を当てる。docker run なら -v sashiki-xfs:$STORE)" >&2
fi
# #290 より前のイメージは writable layer(/xfs.img, /var/lib/sashiki/state.db,
# /etc/sashiki/config.yaml)に置いていた。同じコンテナを再起動した場合だけ
# 拾えるので、あれば STORE へ移す(down していれば既に無い)。
for pair in "/xfs.img:$IMG" "/var/lib/sashiki/state.db:$STATE_DB"; do
  src=${pair%%:*}; dst=${pair#*:}
  if [ -f "$src" ] && [ ! -e "$dst" ]; then
    echo "==> migrating $src -> $dst"
    mv "$src" "$dst"
  fi
done
if [ -f /etc/sashiki/config.yaml ] && [ ! -L /etc/sashiki/config.yaml ] && [ ! -e "$CONFIG" ]; then
  echo "==> migrating /etc/sashiki/config.yaml -> $CONFIG"
  mv /etc/sashiki/config.yaml "$CONFIG"
fi
# 旧既定パスの state.db を STORE へ移した場合、config の参照も同時に直す。
# 利用者が明示した別パスは上書きせず、旧 entrypoint の既定値だけを移行する。
if [ -f "$CONFIG" ]; then
  sed -i "s#^state_db: /var/lib/sashiki/state.db$#state_db: $STATE_DB#" "$CONFIG"
fi

# --- 1. XFS reflink 領域を用意(既存なら再利用)---
# XFS_SIZE_MB は初回の mkfs にだけ効く。後から広げるには、コンテナを止めて
# `truncate -s +<size> xfs.img` → 起動後に `xfs_growfs $ROOT`。
if ! mountpoint -q "$ROOT"; then
  mkdir -p "$ROOT"
  if [ ! -f "$IMG" ]; then
    dd if=/dev/zero of="$IMG" bs=1M count="${XFS_SIZE_MB:-2048}" status=none
    mkfs.xfs -q -m reflink=1 "$IMG"
  fi
  mount -o loop "$IMG" "$ROOT"
fi
# reflink が実際に効くか確認(効かない FS だと create が失敗するので早期に落とす)
touch "$ROOT/.probe.src"
if ! cp --reflink=always "$ROOT/.probe.src" "$ROOT/.probe.dst" 2>/dev/null; then
  echo "FATAL: $ROOT は reflink 非対応です(XFS reflink=1 / Btrfs が必要)" >&2
  exit 1
fi
rm -f "$ROOT/.probe.src" "$ROOT/.probe.dst"

mkdir -p /etc/sashiki /var/log/sashiki "$ROOT/base" "$ROOT/branches"

# --- 2. config を生成(reflink backend + process engine)---
# 実体は STORE に置き、CLI の既定パス /etc/sashiki/config.yaml からは symlink で辿る。
if [ ! -f "$CONFIG" ]; then
  cat > "$CONFIG" <<YAML
listen:
  api: "0.0.0.0:8080"
  proxy: "0.0.0.0:3306"
  metrics: "127.0.0.1:9100"
state_db: $STATE_DB
domain: sashiki.local
storage:
  backend: reflink
  local:
    root: $ROOT
engine:
  type: mysql
  mysql:
    mode: process
    mysqld_bin: /usr/sbin/mysqld
    run_user: mysql
    sudo: false
    app_user: dev
    app_pass: dev
branches:
  name_pattern: "^[a-z0-9-]{1,32}$"
  max_branches: 20
YAML
fi
ln -sfn "$CONFIG" /etc/sashiki/config.yaml
chown -R mysql:mysql "$ROOT" /var/log/sashiki

# --- 3. baseline を構築(初回のみ)---
if [ ! -d "$ROOT/base/snap/baseline" ]; then
  echo "==> building baseline from /opt/sashiki/schema.sql"
  sashiki baseline import --from /opt/sashiki/schema.sql
fi

# --- 4. sashikid 常駐 ---
echo "==> starting sashikid (backend=reflink, engine=process)"
exec sashikid --config /etc/sashiki/config.yaml
