#!/usr/bin/env bash
# transport=ssm の送信経路だけを、偽の aws コマンドで検証する。
#
# 実 SSM を叩かずに確かめたいのは 1 点:
# **インスタンス上で実行されるスクリプトが、送ったものと一文字も違わないこと**。
# ここが崩れると症状はリモート側の文法エラーになり、原因が遠い。
#
# 実際 delete は複数行のスクリプトを送るのに --parameters の shorthand を
# 使っていて、改行で切られて尻切れで届いていた("Syntax error: end of file
# unexpected (expecting fi)")。create は 1 行だったので気づけなかった。
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
ENTRY="$ROOT/action/entrypoint.sh"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

fail() { echo "ACTION SSM E2E FAILED: $*" >&2; exit 1; }

# 偽 aws: send-command で受け取ったスクリプトを $WORK/sent に落とし、
# 以降の問い合わせには成功として応える。
mkdir -p "$WORK/bin"
cat > "$WORK/bin/aws" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
case "$2" in
  send-command)
    # --parameters の値(file://... か shorthand)を拾う
    params=""
    while [ $# -gt 0 ]; do
      [ "$1" = "--parameters" ] && { params=$2; shift 2; continue; }
      shift
    done
    case "$params" in
      file://*)
        python3 -c 'import json,sys
doc=json.load(open(sys.argv[1]))
open(sys.argv[2],"w").write(doc["commands"][0])
open(sys.argv[2]+".all","a").write(doc["commands"][0]+"\n---\n")' "${params#file://}" "$SASHIKI_FAKE_SENT"
        ;;
      *)
        # shorthand は改行を含む値を保てない。届いた形をそのまま記録して
        # テスト側で差分として検出させる。
        printf '%s' "$params" > "$SASHIKI_FAKE_SENT"
        ;;
    esac
    echo "cmd-0123456789"
    ;;
  wait) : ;;
  get-command-invocation)
    case "$*" in
      *Status*)               echo "Success" ;;
      *StandardOutputContent*) cat "$SASHIKI_FAKE_STDOUT" ;;
      *)                      echo "" ;;
    esac
    ;;
  *) echo "fake aws: unexpected: $*" >&2; exit 1 ;;
esac
FAKE
chmod +x "$WORK/bin/aws"

export PATH="$WORK/bin:$PATH"
export SASHIKI_FAKE_SENT="$WORK/sent"
export SASHIKI_FAKE_STDOUT="$WORK/stdout"
export SASHIKI_TRANSPORT=ssm
export SASHIKI_INSTANCE_ID=i-0123456789abcdef0
export SASHIKI_BRANCH=pr-2

echo "=== delete: 複数行のスクリプトが欠けずに届く ==="
# delete は show の結果を読まないので stdout は空でよい
: > "$SASHIKI_FAKE_STDOUT"
SASHIKI_ACTION=delete bash "$ENTRY" > /dev/null || fail "delete が失敗した"
sent=$(cat "$SASHIKI_FAKE_SENT")
# 届いたものが shell として成立していること。ここが今回の回帰点。
bash -n <<<"$sent" || fail "届いたスクリプトが shell として壊れている:
$sent"
grep -q "sashiki delete 'pr-2'" <<<"$sent" || fail "delete のコマンドが入っていない:
$sent"
grep -q "^ *fi$" <<<"$sent" || fail "複数行の末尾(fi)が届いていない:
$sent"
echo "  OK($(wc -l <<<"$sent") 行)"

echo "=== create: show --json の結果を出力に写す ==="
cat > "$SASHIKI_FAKE_STDOUT" <<'JSON'
{"name":"pr-2","port":3306,"engine_port":3401,"host":"sashiki.internal","user":"dev@pr-2"}
JSON
out="$WORK/gh-output"
: > "$out"
SASHIKI_ACTION=create SASHIKI_OUTPUT="$out" bash "$ENTRY" > /dev/null || fail "create が失敗した"
bash -n <<<"$(cat "$SASHIKI_FAKE_SENT")" || fail "create のスクリプトが shell として壊れている"
# 接続情報は proxy 宛の 3 つ組で出ること(#260)
grep -qx "port=3306" "$out"          || fail "port は proxy のものを出すべき: $(cat "$out")"
grep -qx "user=dev@pr-2" "$out"      || fail "user が proxy 形式でない: $(cat "$out")"
grep -qx "host=sashiki.internal" "$out" || fail "host が出ていない: $(cat "$out")"
echo "  OK"

echo "=== #339: シェルのメタ文字を含む branch / profile は送信前に拒否する ==="
for bad in "x'; touch /tmp/pwned; #" 'pr-1$(id)' 'pr-1`id`' "pr-1
id" 'pr 1' '-rf'; do
  rm -f "$SASHIKI_FAKE_SENT"
  if SASHIKI_ACTION=delete SASHIKI_BRANCH="$bad" bash "$ENTRY" > /dev/null 2>&1; then
    fail "branch $(printf %q "$bad") が拒否されなかった"
  fi
  [ -e "$SASHIKI_FAKE_SENT" ] && fail "branch $(printf %q "$bad") で SSM にコマンドが送られた:
$(cat "$SASHIKI_FAKE_SENT")"
done
rm -f "$SASHIKI_FAKE_SENT" "$SASHIKI_FAKE_SENT.all"
if SASHIKI_ACTION=create SASHIKI_PROFILE="ci'; id; #" bash "$ENTRY" > /dev/null 2>&1; then
  fail "profile の注入が拒否されなかった"
fi
[ -e "$SASHIKI_FAKE_SENT" ] && fail "不正な profile で SSM にコマンドが送られた"
# 正常な値は 1 引数ずつクォートされて届く(api と同じく . _ - を許す)
: > "$SASHIKI_FAKE_STDOUT"
cat > "$SASHIKI_FAKE_STDOUT" <<'JSON'
{"name":"feat_x.1","port":3306,"engine_port":3401,"host":"sashiki.internal","user":"dev@feat_x.1"}
JSON
rm -f "$SASHIKI_FAKE_SENT.all"
SASHIKI_ACTION=create SASHIKI_BRANCH=feat_x.1 SASHIKI_PROFILE=preview bash "$ENTRY" > /dev/null || fail "正常な branch / profile が通らない"
sent=$(cat "$SASHIKI_FAKE_SENT.all")   # create → show --json の順に 2 本送られる
grep -q "sashiki create 'feat_x.1' --profile 'preview'" <<<"$sent" || fail "create の引数がクォートされていない:
$sent"
grep -q "sashiki show 'feat_x.1' --json" <<<"$sent" || fail "show の引数がクォートされていない:
$sent"
bash -n <<<"$(cat "$SASHIKI_FAKE_SENT")" || fail "届いたスクリプトが shell として壊れている"
echo "  OK"

echo "ACTION SSM E2E PASSED"
