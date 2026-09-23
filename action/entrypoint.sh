#!/usr/bin/env bash
# sashiki GitHub Action の本体。action.yml から呼ばれるが、単体でも実行できる
# (E2E はローカル sashikid 相手にこのスクリプトを直接検証する)。
#
# 入力(環境変数):
#   SASHIKI_TRANSPORT   api(既定) | ssm。ssm は SSM Run Command で CLI を叩く(#244)
#   SASHIKI_API_URL     sashikid の API URL (transport=api のとき必須)
#   SASHIKI_API_TOKEN   Bearer トークン (sashikid がリモートの場合必須)
#   SASHIKI_INSTANCE_ID 対象 EC2 の instance id (transport=ssm のとき必須)
#   SASHIKI_BRANCH      ブランチ名 (必須, 例: pr-123)
#   SASHIKI_EVENT       PR イベント (opened|reopened|synchronize|closed)
#   SASHIKI_ACTION      操作を明示する (create|delete|reset)。指定時は EVENT より優先(#244)
#   SASHIKI_PROFILE     lifecycle profile (省略可)
#   SASHIKI_SOURCE      provenance の source JSON (省略可。未指定なら github_pr を自動生成)
#   SASHIKI_ON_CLOSE    closed 時の動作 delete|keep (省略時 delete)
#   SASHIKI_PR          PR 番号 (source 自動生成用, 省略可)
#   SASHIKI_OUTPUT      GITHUB_OUTPUT のパス (省略可)
#
# 出力(GITHUB_OUTPUT): host / port / user / created (true|false)
set -euo pipefail

: "${SASHIKI_BRANCH:?SASHIKI_BRANCH is required}"

TRANSPORT="${SASHIKI_TRANSPORT:-api}"
case "$TRANSPORT" in
  api) : "${SASHIKI_API_URL:?SASHIKI_API_URL is required (transport=api)}" ;;
  ssm) : "${SASHIKI_INSTANCE_ID:?SASHIKI_INSTANCE_ID is required (transport=ssm)}" ;;
  *)   echo "sashiki: transport '$TRANSPORT' is not supported (api|ssm)" >&2; exit 1 ;;
esac

auth=()
if [ -n "${SASHIKI_API_TOKEN:-}" ]; then
  auth=(-H "Authorization: Bearer ${SASHIKI_API_TOKEN}")
fi

# 応答ボディの書き先。固定パスだと別ユーザーの残骸ファイルで書き込みに
# 失敗する(curl exit 23)ため mktemp を使う。
RESP=$(mktemp "${TMPDIR:-/tmp}/sashiki-action-resp.XXXXXX")
trap 'rm -f "$RESP"' EXIT

api() {
  local method=$1 path=$2 body=${3:-}
  # 空配列の "${auth[@]}" は set -u + bash < 4.4 で落ちる(macOS の /bin/bash 3.2)。
  local args=(-sS -o "$RESP" -w '%{http_code}' -X "$method" ${auth[@]+"${auth[@]}"} \
    -H "Content-Type: application/json" "${SASHIKI_API_URL}${path}")
  if [ -n "$body" ]; then args+=(-d "$body"); fi
  curl "${args[@]}"
}

emit() {
  [ -n "${SASHIKI_OUTPUT:-}" ] && echo "$1=$2" >> "$SASHIKI_OUTPUT"
  echo "  $1: $2"
}

# 応答($RESP)から JSON フィールドを読む
resp_field() { RESP="$RESP" python3 -c "import json,os;print(json.load(open(os.environ['RESP'])).get('$1',''))"; }

# poll_op <operation_id>: operation が completed になれば 0、failed/timeout で 1。
# 変更操作は 202 + operation_id を返す(#82)。fsx-zfs では数分かかる。
poll_op() {
  local id=$1 i state
  for i in $(seq 1 1200); do  # 最大 ~10 分(1200 * 0.5s)
    api GET "/v1/operations/${id}" >/dev/null || true
    state=$(resp_field state)
    case "$state" in
      completed) return 0 ;;
      failed)    echo "sashiki: operation failed: $(resp_field error)" >&2; return 1 ;;
    esac
    sleep 0.5
  done
  echo "sashiki: operation ${id} timed out" >&2; return 1
}

# --- transport=ssm ---
# VPC 内の API に GitHub-hosted runner から到達できない構成向け(#244)。
# API を叩く代わりに、SSM Run Command でインスタンス上の sashiki CLI を実行する
# (CLI は loopback の API を叩くのでトークン不要)。
ssm_run() {
  local script=$1 cid status out params
  # --parameters は **shorthand を使わない**。shorthand("commands=[...]")は
  # 改行を含む値を途中で切ってしまい、複数行のスクリプトが尻切れで届く
  # (delete の "if ... then ... fi" が `Syntax error: end of file unexpected` で
  # 落ちていた)。file:// で JSON をそのまま渡せば中身を問わず通る。
  params=$(mktemp "${TMPDIR:-/tmp}/sashiki-ssm-params.XXXXXX")
  SASHIKI_SSM_SCRIPT="$script" SASHIKI_SSM_PARAMS="$params" python3 -c 'import json, os
with open(os.environ["SASHIKI_SSM_PARAMS"], "w") as f:
    json.dump({"commands": [os.environ["SASHIKI_SSM_SCRIPT"]]}, f)'
  cid=$(aws ssm send-command \
    --instance-ids "$SASHIKI_INSTANCE_ID" \
    --document-name AWS-RunShellScript \
    --parameters "file://$params" \
    --query Command.CommandId --output text)
  rm -f "$params"
  # 完了まで待つ。wait は失敗時に非 0 を返すが、理由は invocation 側にあるので
  # ここでは握って下で StandardErrorContent を出す。
  aws ssm wait command-executed --command-id "$cid" --instance-id "$SASHIKI_INSTANCE_ID" 2>/dev/null || true
  status=$(aws ssm get-command-invocation --command-id "$cid" \
    --instance-id "$SASHIKI_INSTANCE_ID" --query Status --output text)
  out=$(aws ssm get-command-invocation --command-id "$cid" \
    --instance-id "$SASHIKI_INSTANCE_ID" --query StandardOutputContent --output text)
  if [ "$status" != "Success" ]; then
    echo "sashiki: SSM command failed (status=$status)" >&2
    aws ssm get-command-invocation --command-id "$cid" \
      --instance-id "$SASHIKI_INSTANCE_ID" --query StandardErrorContent --output text >&2 || true
    return 1
  fi
  printf '%s' "$out"
}

# ssm_branch_json <branch>: show --json の結果を $RESP に落とす(api() と同じ形にする)
ssm_branch_json() {
  ssm_run "sashiki show '$1' --json" > "$RESP"
}

# create の body を組み立てる。source は JSON として埋め込むため python で安全に構築する
build_body() {
  python3 - <<'PY'
import json, os, sys
body = {"name": os.environ["SASHIKI_BRANCH"]}
if os.environ.get("SASHIKI_PROFILE"):
    body["profile"] = os.environ["SASHIKI_PROFILE"]
src = os.environ.get("SASHIKI_SOURCE", "")
if src:
    try:
        body["source"] = json.loads(src)
    except ValueError as e:
        print(f"sashiki: source is not valid JSON: {e}", file=sys.stderr)
        sys.exit(1)
elif os.environ.get("SASHIKI_PR") and os.environ.get("GITHUB_REPOSITORY"):
    body["source"] = {
        "type": "github_pr",
        "repository": os.environ["GITHUB_REPOSITORY"],
        "ref": os.environ["SASHIKI_PR"],
    }
print(json.dumps(body))
PY
}

# --- 操作(transport ごとに実装を分ける) ---

do_create() {
  local created
  if [ "$TRANSPORT" = "ssm" ]; then
    local prof=""
    [ -n "${SASHIKI_PROFILE:-}" ] && prof=" --profile '${SASHIKI_PROFILE}'"
    # 既にあれば作らない(冪等)。エラーを握りつぶさず、失敗はそのまま出す。
    ssm_run "if ! sashiki show '${SASHIKI_BRANCH}' >/dev/null 2>&1; then sashiki create '${SASHIKI_BRANCH}'${prof} >&2; fi" >/dev/null
    ssm_branch_json "$SASHIKI_BRANCH"
    created=unknown  # SSM 経由では新規/既存の区別を取らない
  else
    # exist_ok=true で冪等: 202=新規作成(operation を待つ)/ 200=既存。
    # synchronize でも create のみ(TTL 削除後の復活を兼ねる)。migration の
    # 再適用は利用者が recreate を明示的に選ぶ(v2 仕様 22-1)。
    local body code
    body=$(build_body) || exit 1
    code=$(api POST "/v1/branches?exist_ok=true" "$body")
    case "$code" in
      200) created=false ;;  # exist_ok で既存。$RESP に branch がそのまま入る
      202)                    # 新規作成(非同期)。operation を待って branch を取得
        created=true
        local opid
        opid=$(resp_field operation_id)
        poll_op "$opid" || { echo "sashiki: create failed"; exit 1; }
        code=$(api GET "/v1/branches/${SASHIKI_BRANCH}")
        [ "$code" = "200" ] || { echo "sashiki: fetch branch failed (HTTP $code)"; cat "$RESP"; exit 1; }
        ;;
      *)   echo "sashiki: create failed (HTTP $code)"; cat "$RESP"; exit 1 ;;
    esac
  fi
  echo "sashiki: branch '${SASHIKI_BRANCH}' ready (created=${created})"
  emit host "$(resp_field host)"
  emit port "$(resp_field port)"
  emit user "$(resp_field user)"
  emit created "$created"
}

do_delete() {
  if [ "${SASHIKI_ON_CLOSE:-delete}" = "keep" ] && [ -z "${SASHIKI_ACTION:-}" ]; then
    echo "sashiki: on_close=keep のため削除しない (TTL 回収に任せる)"
    return 0
  fi
  if [ "$TRANSPORT" = "ssm" ]; then
    # 冪等にする: delete が失敗しても「もう存在しない」なら成功扱い。
    # 逆にまだ残っているなら本当の失敗なので非 0 で落とす。
    ssm_run "if ! sashiki delete '${SASHIKI_BRANCH}' >&2; then
               if sashiki show '${SASHIKI_BRANCH}' >/dev/null 2>&1; then exit 1; fi
             fi" >/dev/null
    echo "sashiki: branch '${SASHIKI_BRANCH}' deleted (or already absent)"
    return 0
  fi
  local code
  code=$(api DELETE "/v1/branches/${SASHIKI_BRANCH}")
  case "$code" in
    202)  # 非同期削除。operation の完了を待つ
      local opid
      opid=$(resp_field operation_id)
      poll_op "$opid" || exit 1
      echo "sashiki: branch '${SASHIKI_BRANCH}' deleted"
      ;;
    404) echo "sashiki: branch '${SASHIKI_BRANCH}' not found (already deleted)" ;;  # 冪等
    *)   echo "sashiki: delete failed (HTTP $code)"; cat "$RESP"; exit 1 ;;
  esac
}

# reset は「作成時点へ戻す」。ラベル駆動などで明示的に呼ぶ(#244)。
do_reset() {
  if [ "$TRANSPORT" = "ssm" ]; then
    ssm_run "sashiki reset '${SASHIKI_BRANCH}' >&2" >/dev/null
    ssm_branch_json "$SASHIKI_BRANCH"
  else
    local code
    code=$(api POST "/v1/branches/${SASHIKI_BRANCH}/reset")
    case "$code" in
      202)
        local opid
        opid=$(resp_field operation_id)
        poll_op "$opid" || exit 1
        code=$(api GET "/v1/branches/${SASHIKI_BRANCH}")
        [ "$code" = "200" ] || { echo "sashiki: fetch branch failed (HTTP $code)"; cat "$RESP"; exit 1; }
        ;;
      200) : ;;
      *)   echo "sashiki: reset failed (HTTP $code)"; cat "$RESP"; exit 1 ;;
    esac
  fi
  echo "sashiki: branch '${SASHIKI_BRANCH}' reset"
  emit host "$(resp_field host)"
  emit port "$(resp_field port)"
  emit user "$(resp_field user)"
}

# --- 実行する操作を決める ---
# action が明示されていればそれを使う(ラベル駆動など)。無ければ PR イベントから導く。
action="${SASHIKI_ACTION:-}"
if [ -z "$action" ]; then
  : "${SASHIKI_EVENT:?SASHIKI_EVENT is required (SASHIKI_ACTION 未指定時)}"
  case "$SASHIKI_EVENT" in
    closed)                      action=delete ;;
    opened|reopened|synchronize) action=create ;;
    *) echo "sashiki: event '${SASHIKI_EVENT}' is not handled (noop)"; exit 0 ;;
  esac
fi

case "$action" in
  create) do_create ;;
  delete) do_delete ;;
  reset)  do_reset ;;
  *) echo "sashiki: action '${action}' is not supported (create|delete|reset)" >&2; exit 1 ;;
esac
