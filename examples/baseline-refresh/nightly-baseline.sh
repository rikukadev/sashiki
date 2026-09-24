#!/usr/bin/env bash
# baseline を build → validate → publish の 3 段階で更新する運用スクリプト(#277)。
# nightly(systemd timer)と merge トリガー(GitHub Actions → SSM)の両方から同じものを呼ぶ。
#
#   /usr/local/bin/nightly-baseline.sh            # 通常
#   SASHIKI_SKIP_VALIDATE=1 ...                   # validate を飛ばす(require_validated なら publish は 412)
#   SASHIKI_NOTIFY_CMD='curl -X POST ...' ...     # 失敗時に実行する通知コマンド(本文は標準入力)
#
# 前提: sashikid ホスト上で、sashiki CLI が loopback から API を叩ける(トークン不要)。
# build の中身は config の baseline.refresh_script / source_dir(refresh.sh 参照)。
# PII マスクを build に組み込み、baseline.require_masked: true にしておけば未マスクは
# publish できない(refresh.sh が masked_sentinel を touch する)。
#
# 終了コード: 0 = publish 済み / 既に実行中(409、他の実行に任せる) 1 = 失敗(通知済み)
set -euo pipefail

TIMEOUT=${SASHIKI_BUILD_TIMEOUT:-2h}     # build の待ち上限(refresh_timeout と揃える)
VALIDATE_TIMEOUT=${SASHIKI_VALIDATE_TIMEOUT:-30m}
LOG=${SASHIKI_NIGHTLY_LOG:-/var/log/sashiki/nightly-baseline.log}

log()    { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" | tee -a "$LOG"; }
notify() {
  local msg=$1
  log "FAILED: $msg"
  if [ -n "${SASHIKI_NOTIFY_CMD:-}" ]; then
    printf 'sashiki baseline (%s): %s\n' "$(hostname)" "$msg" | bash -c "$SASHIKI_NOTIFY_CMD" || true
  fi
  exit 1
}

# 1. build(202 + operation。既定で完了まで待つ)。他の build / refresh が走っていれば
#    409 = 終了コード 4。二重起動はしない(timer と merge トリガーが重なった場合)。
log "build start"
set +e
out=$(sashiki baseline build --timeout "$TIMEOUT" 2>&1)
rc=$?
set -e
case $rc in
  0) ;;
  4) log "another build / refresh is in progress; skipping: $out"; exit 0 ;;
  6) notify "build timed out after $TIMEOUT (operation still running; see sashiki op list)" ;;
  *) notify "build failed (exit $rc): $out" ;;
esac
snap=$(grep -oE 'baseline built: [^ ]+' <<<"$out" | awk '{print $3}')
[ -n "$snap" ] || notify "could not parse the built snapshot from: $out"
log "built $snap"

# 2. validate(候補を _validate で起動し on-baseline-validate を流す。落ちたら current にしない)
if [ -z "${SASHIKI_SKIP_VALIDATE:-}" ]; then
  set +e
  vout=$(sashiki baseline validate "$snap" --timeout "$VALIDATE_TIMEOUT" 2>&1)
  vrc=$?
  set -e
  if [ $vrc -ne 0 ]; then
    # 登録は残る(baseline list に出る)。current は前の正常版のまま。
    notify "validate failed for $snap (exit $vrc): $vout — current baseline is unchanged; inspect with 'sashiki baseline list' and delete with 'sashiki baseline delete $snap'"
  fi
  log "validated $snap"
fi

# 3. publish(同期。require_masked / require_validated を満たさないと 412 = 終了コード 1)
set +e
pout=$(sashiki baseline publish "$snap" 2>&1)
prc=$?
set -e
[ $prc -eq 0 ] || notify "publish refused for $snap (exit $prc): $pout — policy (require_masked / require_validated) not met?"
log "published $snap (current)"

# 4. 古い候補を掃除(keep_last / retention は config)。失敗しても publish は済んでいる。
sashiki baseline gc >> "$LOG" 2>&1 || log "gc failed (non-fatal)"
