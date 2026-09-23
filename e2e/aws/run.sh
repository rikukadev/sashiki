#!/usr/bin/env bash
# 実 AWS の E2E。EC2 を立てて利用者と同じ手順で sashiki を入れたあと、
# data EBS を新しい EC2 へ付け替えて compute replacement まで確認する。
#
#   ./e2e/aws/run.sh <sashiki.deb> <タグ>
#
# ループバックの E2E(e2e/e2e.sh)では見えないものを見るための枠:
#   - **実 EBS デバイス**に対する sashiki init(ループバックファイルではない)
#   - **deb 経由の導入**(sashikid.service / sashiki ユーザー / ディレクトリ)
#   - **GitHub Action の transport=ssm を端から端まで**。偽の aws を使う
#     e2e/action-ssm.sh は送信の形しか見ない。実際に SSM が届いて
#     インスタンス上の CLI が動いて出力が返ることは、ここでしか確かめられない
#
# **必ず terminate する。** CFN と違って生 EC2 は誰も片付けてくれない。
# trap と、呼び出し側 workflow の if: always() で二重に担保し、さらに
# タグを付けて取りこぼしを後から探せるようにする。
set -euo pipefail

DEB=${1:?usage: run.sh <sashiki.deb> <tag>}
TAG=${2:?usage: run.sh <sashiki.deb> <tag>}
REGION=${AWS_REGION:-ap-northeast-1}
BUCKET=${E2E_ARTIFACT_BUCKET:?E2E_ARTIFACT_BUCKET が未設定}
PROFILE=sashiki-e2e-instance
TYPE=t3.medium # mysqld を複数起こすので 2GB では足りない

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
PREFIX="s3://$BUCKET/$TAG"

fail() { echo "AWS E2E FAILED: $*" >&2; exit 1; }
log() { echo; echo "=== $* ==="; }

IID=""
OLD_IID=""
DATA_VOLUME_ID=""
cleanup() {
  local rc=$?
  log "cleanup (exit=$rc)"
  for cleanup_iid in "$IID" "$OLD_IID"; do
    if [ -n "$cleanup_iid" ]; then
      echo "  terminating $cleanup_iid"
      aws ec2 terminate-instances --region "$REGION" --instance-ids "$cleanup_iid" > /dev/null 2>&1 \
        || echo "  terminate に失敗。手で確認すること: $cleanup_iid" >&2
    fi
  done
  if [ -n "$DATA_VOLUME_ID" ]; then
    echo "  deleting $DATA_VOLUME_ID"
    aws ec2 wait volume-available --region "$REGION" --volume-ids "$DATA_VOLUME_ID" 2>/dev/null || true
    aws ec2 delete-volume --region "$REGION" --volume-id "$DATA_VOLUME_ID" > /dev/null 2>&1 \
      || echo "  volume削除に失敗。手で確認すること: $DATA_VOLUME_ID" >&2
  fi
  aws s3 rm "$PREFIX" --recursive > /dev/null 2>&1 || true
  exit "$rc"
}
trap cleanup EXIT

# ssm <script>: インスタンス上で root として実行し、標準出力を返す。
# --parameters は shorthand を使わない(改行で切れる。#263 と同じ理由)。
ssm() {
  local script=$1 cid status params
  params=$(mktemp)
  SCRIPT="$script" PARAMS="$params" python3 -c '
import json, os
json.dump({"commands": [os.environ["SCRIPT"]]}, open(os.environ["PARAMS"], "w"))'
  cid=$(aws ssm send-command --region "$REGION" --instance-ids "$IID" \
    --document-name AWS-RunShellScript --parameters "file://$params" \
    --timeout-seconds 1800 --query Command.CommandId --output text)
  rm -f "$params"
  aws ssm wait command-executed --region "$REGION" --command-id "$cid" --instance-id "$IID" 2>/dev/null || true
  status=$(aws ssm get-command-invocation --region "$REGION" --command-id "$cid" \
    --instance-id "$IID" --query Status --output text)
  aws ssm get-command-invocation --region "$REGION" --command-id "$cid" \
    --instance-id "$IID" --query StandardOutputContent --output text
  if [ "$status" != "Success" ]; then
    aws ssm get-command-invocation --region "$REGION" --command-id "$cid" \
      --instance-id "$IID" --query StandardErrorContent --output text >&2
    return 1
  fi
}

wait_for_ready() {
  local state=None out=""
  log "wait for SSM ($IID)"
  for _ in $(seq 1 60); do
    state=$(aws ssm describe-instance-information --region "$REGION" \
      --filters "Key=InstanceIds,Values=$IID" \
      --query 'InstanceInformationList[0].PingStatus' --output text 2>/dev/null || echo None)
    [ "$state" = "Online" ] && break
    sleep 10
  done
  [ "$state" = "Online" ] || fail "SSM が Online にならない(インスタンスプロファイル / 経路を確認)"
  echo "  online"

  log "wait for provisioning ($IID)"
  for _ in $(seq 1 90); do
    out=$(ssm 'if [ -f /var/tmp/sashiki-e2e-failed ]; then echo "FAILED: $(cat /var/tmp/sashiki-e2e-failed)";
               elif [ -f /var/tmp/sashiki-e2e-ready ]; then echo READY; else echo WAIT; fi' 2>/dev/null || echo WAIT)
    case "$out" in
      READY*) break ;;
      FAILED*)
        echo "$out" >&2
        ssm 'tail -40 /var/log/sashiki-e2e-provision.log' >&2 || true
        fail "provision が失敗した"
        ;;
    esac
    sleep 10
  done
  grep -q READY <<<"${out:-}" || {
    ssm 'echo "--- provision.log ---"; tail -40 /var/log/sashiki-e2e-provision.log 2>/dev/null || echo "(無い = user-data が走る前に失敗)"
         echo "--- cloud-init ---"; tail -20 /var/log/cloud-init-output.log 2>/dev/null' >&2 || true
    fail "provision が終わらない"
  }
  ssm 'curl -fsS http://127.0.0.1:8080/v1/healthz >/dev/null' \
    || fail "provision完了直後にhealth endpointが応答しない"
  echo "  provisioned and healthy"
}

log "upload artifacts"
[ -f "$DEB" ] || fail "deb が無い: $DEB"
aws s3 cp "$DEB" "$PREFIX/sashiki.deb" > /dev/null
aws s3 cp "$SCRIPT_DIR/provision.sh" "$PREFIX/provision.sh" > /dev/null
aws s3 cp "$SCRIPT_DIR/../../deploy/terraform/persist-state.sh" "$PREFIX/persist-state.sh" > /dev/null
# 署名付き URL で渡す。Ubuntu の素のイメージに **AWS CLI は入っていない**ので、
# user-data で aws s3 cp を呼ぶと cloud-init が "aws: command not found" で
# 落ちる(しかも user-data の失敗は静かで、SSM は Online になるため
# 「起動はしたのに何も始まらない」という分かりにくい形で出る)。
# curl だけで済ませれば起動時の依存がゼロになり、インスタンス側に
# S3 の読み取り権限も要らない。
# **バケットのリージョンで署名する。** 別リージョンで署名した URL に S3 は
# エラー XML を返すので、curl はそれをファイルとして保存し、cloud-init が
# XML を shell として実行しようとして落ちる(症状は `<?xml ...` の構文エラーで、
# 原因のリージョンには何も言及されない)。
BUCKET_REGION=$(aws s3api get-bucket-location --bucket "$BUCKET" --query LocationConstraint --output text)
[ "$BUCKET_REGION" = "None" ] && BUCKET_REGION=us-east-1
DEB_URL=$(aws s3 presign "$PREFIX/sashiki.deb" --region "$BUCKET_REGION" --expires-in 3600)
PROVISION_URL=$(aws s3 presign "$PREFIX/provision.sh" --region "$BUCKET_REGION" --expires-in 3600)
PERSIST_STATE_URL=$(aws s3 presign "$PREFIX/persist-state.sh" --region "$BUCKET_REGION" --expires-in 3600)
echo "  $PREFIX (region $BUCKET_REGION)"

log "launch ec2"
AMI=$(aws ssm get-parameter --region "$REGION" \
  --name /aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id \
  --query Parameter.Value --output text)
# 既定 VPC のサブネットを 1 つ使う。SSM は **アウトバウンドだけ** で足りるので
# インバウンドは開けない(SSH 鍵も踏み台も要らないのが ssm 経由の利点)。
SUBNET=$(aws ec2 describe-subnets --region "$REGION" \
  --filters Name=default-for-az,Values=true \
  --query 'Subnets[0].SubnetId' --output text)
[ "$SUBNET" != "None" ] || fail "既定 VPC のサブネットが見つからない"

USERDATA=$(cat <<EOF
#!/bin/bash
set -eux
curl -fsSL "$DEB_URL" -o /var/tmp/sashiki.deb
curl -fsSL "$PROVISION_URL" -o /var/tmp/provision.sh
curl -fsSL "$PERSIST_STATE_URL" -o /var/tmp/persist-state.sh
# 取れたものが本当にスクリプトか確かめる。S3 がエラー XML を返しても
# curl -f が拾えない場合があり、そのまま実行すると原因の遠い構文エラーになる。
head -1 /var/tmp/provision.sh | grep -q '^#!' || {
  echo "provision.sh が取得できていない(先頭 200 バイト):" >&2
  head -c 200 /var/tmp/provision.sh >&2
  echo "S3 のエラー応答ではないか(署名リージョンを確認)" > /var/tmp/sashiki-e2e-failed
  exit 1
}
chmod +x /var/tmp/provision.sh /var/tmp/persist-state.sh
/var/tmp/provision.sh
EOF
)

IID=$(aws ec2 run-instances --region "$REGION" \
  --image-id "$AMI" --instance-type "$TYPE" --subnet-id "$SUBNET" \
  --iam-instance-profile "Name=$PROFILE" \
  --metadata-options "HttpTokens=required,HttpEndpoint=enabled" \
  --block-device-mappings \
    'DeviceName=/dev/sda1,Ebs={VolumeSize=20,VolumeType=gp3,DeleteOnTermination=true}' \
    'DeviceName=/dev/sdb,Ebs={VolumeSize=20,VolumeType=gp3,DeleteOnTermination=false}' \
  --user-data "$USERDATA" \
  --tag-specifications \
    "ResourceType=instance,Tags=[{Key=Name,Value=$TAG},{Key=sashiki-e2e,Value=true}]" \
    "ResourceType=volume,Tags=[{Key=Name,Value=$TAG},{Key=sashiki-e2e,Value=true}]" \
  --query 'Instances[0].InstanceId' --output text) || fail "run-instances が失敗した"
echo "  $IID ($TYPE, $SUBNET)"

DATA_VOLUME_ID=$(aws ec2 describe-volumes --region "$REGION" \
  --filters "Name=attachment.instance-id,Values=$IID" "Name=attachment.device,Values=/dev/sdb" \
  --query 'Volumes[0].VolumeId' --output text)
[ "$DATA_VOLUME_ID" != "None" ] || fail "data volumeを特定できない"
echo "  data volume: $DATA_VOLUME_ID"

wait_for_ready

# Terraform の api_url は「SG 内から Bearer で叩く」経路(#286)。loopback から
# 叩くだけの検証では、sashikid が 127.0.0.1 にしか bind していなくても通って
# しまう。自分の private IP 宛てに叩けば loopback 免除を通らないので、bind と
# 認証の両方が見える。
log "API に loopback 以外から届く(トークン必須)"
out=$(ssm '
set -eu
IP=$(hostname -I | awk "{print \$1}")
TOKEN=$(cat /var/tmp/sashiki-e2e-token)
noauth=$(curl -s -o /dev/null -w "%{http_code}" "http://$IP:8080/v1/branches")
auth=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $TOKEN" "http://$IP:8080/v1/branches")
echo "ip=$IP noauth=$noauth auth=$auth"
') || fail "API の到達確認に失敗した"
echo "  $out"
grep -q "noauth=401" <<<"$out" || fail "loopback 以外からトークン無しで通ってしまう: $out"
grep -q "auth=200" <<<"$out" || fail "loopback 以外からトークン付きで届かない(listen.api が loopback のまま?): $out"

log "branch を作って proxy 経由で読む"
out=$(ssm '
set -eu
sashiki create pr-1 > /dev/null
mysql -udev@pr-1 -pdev -h127.0.0.1 -P3306 -N -e "SELECT COUNT(*) FROM app.items" 2>/dev/null
') || fail "branch の作成 / 接続に失敗した"
[ "$(tr -d ' \n' <<<"$out")" = "3" ] || fail "baseline の 3 行が読めない: $out"
echo "  3 rows via proxy"

log "ブランチが独立している"
out=$(ssm '
set -eu
mysql -udev@pr-1 -pdev -h127.0.0.1 -P3306 -e "INSERT INTO app.items (name) VALUES (\"only-in-pr-1\")" 2>/dev/null
sashiki create pr-2 > /dev/null
mysql -udev@pr-2 -pdev -h127.0.0.1 -P3306 -N -e "SELECT COUNT(*) FROM app.items" 2>/dev/null
') || fail "2 本目のブランチを作れない"
[ "$(tr -d ' \n' <<<"$out")" = "3" ] || fail "pr-2 に pr-1 の書き込みが見えている: $out"
echo "  pr-1 の書き込みは pr-2 に見えない"

# ここが実 AWS でしか通せない経路。偽の aws を使う e2e/action-ssm.sh は
# 「送った形」しか見ておらず、実際に届いて動くかは確かめていない。
log "GitHub Action の transport=ssm(端から端まで)"
OUT=$(mktemp)
SASHIKI_TRANSPORT=ssm \
SASHIKI_INSTANCE_ID="$IID" \
SASHIKI_ACTION=create \
SASHIKI_BRANCH=pr-action \
SASHIKI_OUTPUT="$OUT" \
AWS_REGION="$REGION" \
  bash "$SCRIPT_DIR/../../action/entrypoint.sh" || fail "action(create)が失敗した"
cat "$OUT"
grep -qx "port=3306" "$OUT" || fail "port が proxy のものでない(#260): $(cat "$OUT")"
grep -qx "user=dev@pr-action" "$OUT" || fail "user が proxy 形式でない: $(cat "$OUT")"
grep -q "^host=" "$OUT" || fail "host が出ていない"

SASHIKI_TRANSPORT=ssm \
SASHIKI_INSTANCE_ID="$IID" \
SASHIKI_ACTION=delete \
SASHIKI_BRANCH=pr-action \
AWS_REGION="$REGION" \
  bash "$SCRIPT_DIR/../../action/entrypoint.sh" || fail "action(delete)が失敗した"
ssm 'sashiki show pr-action > /dev/null 2>&1 && echo LEFT || echo GONE' | grep -q GONE \
  || fail "action の delete でブランチが消えていない"
echo "  create → 接続情報 → delete まで通った"

log "compute replacement: data EBSを新しいEC2へ付け替える"
ssm '
set -eu
sashiki drain
systemctl stop sashikid
zpool export dbpool
' || fail "置換前のdrain / zpool exportに失敗した"

OLD_IID=$IID
aws ec2 terminate-instances --region "$REGION" --instance-ids "$OLD_IID" >/dev/null \
  || fail "旧EC2をterminateできない"
aws ec2 wait instance-terminated --region "$REGION" --instance-ids "$OLD_IID" \
  || fail "旧EC2のterminateが完了しない"
aws ec2 wait volume-available --region "$REGION" --volume-ids "$DATA_VOLUME_ID" \
  || fail "data volumeがavailableにならない"

IID=$(aws ec2 run-instances --region "$REGION" \
  --image-id "$AMI" --instance-type "$TYPE" --subnet-id "$SUBNET" \
  --iam-instance-profile "Name=$PROFILE" \
  --metadata-options "HttpTokens=required,HttpEndpoint=enabled" \
  --block-device-mappings \
    'DeviceName=/dev/sda1,Ebs={VolumeSize=20,VolumeType=gp3,DeleteOnTermination=true}' \
  --user-data "$USERDATA" \
  --tag-specifications \
    "ResourceType=instance,Tags=[{Key=Name,Value=$TAG},{Key=sashiki-e2e,Value=true}]" \
    "ResourceType=volume,Tags=[{Key=Name,Value=$TAG},{Key=sashiki-e2e,Value=true}]" \
  --query 'Instances[0].InstanceId' --output text) || fail "置換先EC2の起動に失敗した"
echo "  replacement: $IID"

aws ec2 wait instance-running --region "$REGION" --instance-ids "$IID" \
  || fail "置換先EC2がrunningにならない"
aws ec2 attach-volume --region "$REGION" --volume-id "$DATA_VOLUME_ID" \
  --instance-id "$IID" --device /dev/sdf >/dev/null \
  || fail "data volumeを置換先へattachできない"

wait_for_ready

log "compute replacement後もshow / proxy / resetが動く"
out=$(ssm '
set -eu
sashiki show pr-1 >/dev/null
sashiki show pr-2 >/dev/null
before=$(mysql -udev@pr-1 -pdev -h127.0.0.1 -P3306 -N -e "SELECT COUNT(*) FROM app.items" 2>/dev/null)
sashiki reset pr-1 >/dev/null
after=$(mysql -udev@pr-1 -pdev -h127.0.0.1 -P3306 -N -e "SELECT COUNT(*) FROM app.items" 2>/dev/null)
echo "before=$before after=$after"
') || fail "置換後のbranch操作に失敗した"
echo "  $out"
grep -q 'before=4 after=3' <<<"$out" \
  || fail "置換前の書き込み保持またはreset結果が不正: $out"

log "後始末(ブランチと baseline)"
ssm '
set -eu
sashiki delete pr-1 > /dev/null
sashiki delete pr-2 > /dev/null
sashiki list
' || fail "ブランチを消せない"

log "AWS E2E PASSED"
