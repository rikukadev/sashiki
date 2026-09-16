#!/usr/bin/env bash
# EC2 側の準備。run.sh が S3 経由で置いて、user-data から root で実行する。
#
# ここでやるのは「利用者が最初にやること」と同じ手順:
#   パッケージ → sashiki init(**実 EBS デバイス**)→ baseline import → sashikid 起動
#
# ループバックの E2E(e2e/e2e.sh)と違うのはデバイスで、そこが要点。
# 実機では /dev/sdb と書いても /dev/nvme1n1 として見える、AppArmor が
# 効いている、systemd が本物、といった差はここでしか出ない。
set -euo pipefail

POOL=dbpool
SENTINEL=/var/tmp/sashiki-e2e-ready   # run.sh はこれが出るのを待つ
FAILED=/var/tmp/sashiki-e2e-failed

fail() {
  echo "PROVISION FAILED: $*" >&2
  echo "$*" > "$FAILED"
  exit 1
}
log() { echo "=== $* ==="; }

exec > >(tee -a /var/log/sashiki-e2e-provision.log) 2>&1

log "packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -q > /dev/null
apt-get install -y -q zfsutils-linux mysql-server-8.0 mysql-client-8.0 apparmor-utils > /dev/null

# **deb で入れる。** 利用者と同じ経路にしないと、パッケージが運ぶもの
# (sashikid.service / sashiki ユーザー / /var/lib・/var/log の用意)が
# 検証されない。バイナリを直接置くと、そこが抜けたまま緑になる。
log "install sashiki (deb)"
dpkg -i /var/tmp/sashiki.deb || fail "deb のインストールに失敗した"
id sashiki > /dev/null || fail "postinstall が sashiki ユーザーを作っていない"
[ -f /lib/systemd/system/sashikid.service ] || fail "deb が sashikid.service を置いていない"
sashiki version
# 既定の mysql は使わない。sashiki が branch ごとに mysqld を起こす。
systemctl stop mysql 2>/dev/null || true
systemctl disable mysql 2>/dev/null || true

log "find the extra volume"
# EBS の /dev/sdb は Nitro では /dev/nvme1n1 として見える。名前を決め打ちせず
# 「マウントされていない・パーティションでない・ルートでないブロックデバイス」を探す。
# ここを固定名で書くと、インスタンス種別を変えた瞬間に落ちる。
DEVICE=""
for d in $(lsblk -dpno NAME,TYPE | awk '$2=="disk"{print $1}'); do
  if [ -z "$(lsblk -no MOUNTPOINT "$d" | tr -d ' \n')" ]; then
    DEVICE="$d"
    break
  fi
done
[ -n "$DEVICE" ] || fail "zpool 用の空きディスクが見つからない: $(lsblk -dpno NAME,SIZE,TYPE | tr '\n' ' ')"
echo "  device: $DEVICE ($(lsblk -dno SIZE "$DEVICE" | tr -d ' '))"

log "sashiki init --device $DEVICE"
# --yes は必須。cloud-init には TTY も標準入力も無いので、確認プロンプトは
# 即 EOF で「中止しました」になる。自動化から使う経路はここを通る。
sashiki init --yes --pool "$POOL" --device "$DEVICE" || fail "init が失敗した"
zpool list "$POOL" > /dev/null || fail "zpool $POOL が作られていない"

log "baseline import"
cat > /var/tmp/sample.sql <<'SQL'
CREATE DATABASE app;
CREATE TABLE app.items (id INT PRIMARY KEY AUTO_INCREMENT, name VARCHAR(64));
INSERT INTO app.items (name) VALUES ('alpha'), ('beta'), ('gamma');
SQL
sashiki baseline import --from /var/tmp/sample.sql || fail "baseline import が失敗した"
zfs list "$POOL/base@baseline" > /dev/null || fail "baseline スナップショットが無い"

# Terraform の user-data と同じく、API を全 IF で listen させる(#286)。init の
# 既定は loopback で、それだと SG を開けても api_url に届かない。ここで同じ
# 書き換えをしておき、run.sh が「loopback 以外から Bearer で届く / 無しは 401」
# を確かめる。書き換えの sed は deploy/terraform/user-data.sh.tftpl と揃えること。
log "listen.api を 0.0.0.0 にして token を発行"
sed -i -E 's/^( *)api: *"127\.0\.0\.1:8080"/\1api: "0.0.0.0:8080"/' /etc/sashiki/config.yaml
grep -qE '^ *api: *"0\.0\.0\.0:8080"' /etc/sashiki/config.yaml || fail "listen.api を書き換えられない"
sashiki token create --name e2e | grep -oE 'sashiki_[0-9a-f]{64}' > /var/tmp/sashiki-e2e-token \
  || fail "token を発行できない"
chmod 0600 /var/tmp/sashiki-e2e-token

log "start sashikid"
systemctl enable --now sashikid || fail "sashikid を起動できない"
for _ in $(seq 1 30); do
  curl -sf http://127.0.0.1:8080/v1/healthz > /dev/null && break
  sleep 1
done
curl -sf http://127.0.0.1:8080/v1/healthz > /dev/null || fail "sashikid が応答しない"

log "ready"
date -u +%Y-%m-%dT%H:%M:%SZ > "$SENTINEL"
