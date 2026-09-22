#!/usr/bin/env bash
# Terraform 構成の state.db を、EC2 の root volume ではなくデータ EBS 上の
# ZFS dataset に置く。user-data と SSM Association の両方から呼ばれるため冪等。
set -euo pipefail

POOL=${SASHIKI_POOL:-tank}
DATASET="$POOL/sashiki-state"
MOUNTPOINT=/var/lib/sashiki-state
OLD_STATE=/var/lib/sashiki/state.db
STATE="$MOUNTPOINT/state.db"
CONFIG=/etc/sashiki/config.yaml

zpool list -H "$POOL" >/dev/null

# 既存 module を in-place で更新するときだけ daemon が動いている。SQLite の
# WAL を含めて整合した状態でコピーするため、移行中は止めて最後に戻す。
was_active=false
restore_daemon() {
  if $was_active; then
    systemctl start sashikid || true
  fi
}
trap restore_daemon EXIT
if systemctl is-active --quiet sashikid; then
  was_active=true
  systemctl stop sashikid
fi

if ! zfs list -H -o name "$DATASET" >/dev/null 2>&1; then
  zfs create -o mountpoint="$MOUNTPOINT" "$DATASET"
else
  current_mountpoint=$(zfs get -H -o value mountpoint "$DATASET")
  if [ "$current_mountpoint" != "$MOUNTPOINT" ]; then
    zfs set mountpoint="$MOUNTPOINT" "$DATASET"
  fi
  if ! zfs mount | awk -v dataset="$DATASET" '$1 == dataset { found=1 } END { exit !found }'; then
    zfs mount "$DATASET"
  fi
fi

install -d -o sashiki -g sashiki -m 0750 "$MOUNTPOINT"

# 移行済みの永続側を常に正とする。初回移行だけ旧 root volume 側から SQLite
# 本体と WAL/SHM をコピーする。daemon は停止済みなので途中書き込みはない。
for suffix in '' '-wal' '-shm'; do
  if [ ! -e "$STATE$suffix" ] && [ -e "$OLD_STATE$suffix" ]; then
    cp -a -- "$OLD_STATE$suffix" "$STATE$suffix"
  fi
done
chown -R sashiki:sashiki "$MOUNTPOINT"

if [ ! -f "$CONFIG" ]; then
  echo "sashiki: config が見つかりません: $CONFIG" >&2
  exit 1
fi
sed -i -E "s#^state_db:.*#state_db: $STATE#" "$CONFIG"
grep -qx "state_db: $STATE" "$CONFIG" || {
  echo "sashiki: state_db を永続 dataset に変更できませんでした" >&2
  exit 1
}

if $was_active; then
  systemctl start sashikid
  was_active=false
fi
trap - EXIT
