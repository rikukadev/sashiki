#!/usr/bin/env bash
# transport=ssm の送信経路だけを、偽の aws コマンドで検証する。
#
# 実 SSM を叩かずに、インスタンスへ届く script と復元される argv を検証する。
# 公開 input を shell 文字列へ連結すると、SSM Agent の権限で command injection
# になるため、全操作が同じ安全な argv 転送 helper を通ることもここで固定する。
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
open(sys.argv[2],"w").write(doc["commands"][0])' "${params#file://}" "$SASHIKI_FAKE_SENT"
        python3 -c 'import json,sys
with open(sys.argv[2], "a") as f:
    f.write(json.dumps(open(sys.argv[1]).read()) + "\n")' "$SASHIKI_FAKE_SENT" "$SASHIKI_FAKE_SENT_LOG"
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
export SASHIKI_FAKE_SENT_LOG="$WORK/sent.log"
export SASHIKI_FAKE_STDOUT="$WORK/stdout"
export SASHIKI_TRANSPORT=ssm
export SASHIKI_INSTANCE_ID=i-0123456789abcdef0
export SASHIKI_BRANCH=pr-2

assert_argv_log() {
  local expected=$1
  SASHIKI_EXPECTED="$expected" python3 - "$SASHIKI_FAKE_SENT_LOG" <<'PY'
import base64, json, os, re, sys

scripts = [json.loads(line) for line in open(sys.argv[1])]
actual = []
for script in scripts:
    match = re.fullmatch(r"SASHIKI_ARGV_B64='([A-Za-z0-9_=-]+)' python3 -c '.+'", script)
    if not match:
        raise SystemExit(f"input was interpolated into remote shell script: {script!r}")
    actual.append(json.loads(base64.urlsafe_b64decode(match.group(1))))
expected = json.loads(os.environ["SASHIKI_EXPECTED"])
if actual != expected:
    raise SystemExit(f"argv mismatch:\nactual={actual!r}\nexpected={expected!r}")
PY
}

echo "=== invalid input: shell metacharacter を SSM 送信前に拒否する ==="
bad_inputs=(
  "x'; touch /tmp/pwned; #"
  $'line\nbreak'
  '$(touch /tmp/pwned)'
  '`touch /tmp/pwned`'
  'semi;colon'
)
for bad in "${bad_inputs[@]}"; do
  rm -f "$SASHIKI_FAKE_SENT" "$SASHIKI_FAKE_SENT_LOG"
  if SASHIKI_BRANCH="$bad" SASHIKI_ACTION=create bash "$ENTRY" >/dev/null 2>&1; then
    fail "危険な branch が通った: $bad"
  fi
  [ ! -e "$SASHIKI_FAKE_SENT" ] || fail "危険な branch が SSM へ送られた: $bad"

  if SASHIKI_PROFILE="$bad" SASHIKI_ACTION=create bash "$ENTRY" >/dev/null 2>&1; then
    fail "危険な profile が通った: $bad"
  fi
  [ ! -e "$SASHIKI_FAKE_SENT" ] || fail "危険な profile が SSM へ送られた: $bad"
done
echo "  OK"

echo "=== delete: argv を shell 展開せずに転送する ==="
: > "$SASHIKI_FAKE_STDOUT"
: > "$SASHIKI_FAKE_SENT_LOG"
SASHIKI_ACTION=delete bash "$ENTRY" > /dev/null || fail "delete が失敗した"
assert_argv_log '[["delete","pr-2"]]'
grep -q 'pr-2' "$SASHIKI_FAKE_SENT" && fail "branch が remote shell に平文で連結された"
echo "  OK"

echo "=== create: show --json の結果を出力に写す ==="
cat > "$SASHIKI_FAKE_STDOUT" <<'JSON'
{"name":"pr-2","port":3306,"engine_port":3401,"host":"sashiki.internal","user":"dev@pr-2"}
JSON
out="$WORK/gh-output"
: > "$out"
: > "$SASHIKI_FAKE_SENT_LOG"
SASHIKI_ACTION=create SASHIKI_PROFILE=preview SASHIKI_OUTPUT="$out" bash "$ENTRY" > /dev/null || fail "create が失敗した"
assert_argv_log '[["create","pr-2","--exist-ok","--profile","preview"],["show","pr-2","--json"]]'
# 接続情報は proxy 宛の 3 つ組で出ること(#260)
grep -qx "port=3306" "$out"          || fail "port は proxy のものを出すべき: $(cat "$out")"
grep -qx "user=dev@pr-2" "$out"      || fail "user が proxy 形式でない: $(cat "$out")"
grep -qx "host=sashiki.internal" "$out" || fail "host が出ていない: $(cat "$out")"
echo "  OK"

echo "=== reset: reset と show が同じ argv helper を通る ==="
: > "$SASHIKI_FAKE_SENT_LOG"
SASHIKI_ACTION=reset bash "$ENTRY" > /dev/null || fail "reset が失敗した"
assert_argv_log '[["reset","pr-2"],["show","pr-2","--json"]]'
echo "  OK"

echo "ACTION SSM E2E PASSED"
