#!/usr/bin/env bash
# install.sh を、ネットワークも root も使わずに通す(#287)。
#
# 確かめたいのは 3 つ:
#   1. macOS 標準の /bin/bash 3.2 で動くこと。`set -u` 下の空配列展開が
#      3.2 では unbound variable になり、README の `curl | bash` がそのまま落ちていた。
#      このスクリプト自身を /bin/bash で起動すれば install.sh も同じ bash で走る。
#   2. checksums.txt と照合していること。改竄した asset・行の無い checksums で
#      止まらなければ、検証は飾りになる。
#   3. latest API 経由とタグ直リンクの両経路で、同じ検証が入っていること。
#
# curl / uname / dpkg / apt-get を偽物に差し替え、release の asset は
# ここで作ったフィクスチャから返す。
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
INSTALL="$ROOT/install.sh"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

fail() { echo "INSTALL.SH E2E FAILED: $*" >&2; exit 1; }
log() { echo "=== $*"; }

echo "bash: $BASH_VERSION"

# ───── フィクスチャ: release の asset を作る ─────
VER=0.0.1
FIX="$WORK/release"
mkdir -p "$FIX" "$WORK/pkg"
# darwin tar.gz: 中身は version を答えるだけの偽 sashiki / sashikid
for b in sashiki sashikid; do
  printf '#!/bin/sh\necho "%s %s+test"\n' "$b" "$VER" > "$WORK/pkg/$b"
  chmod +x "$WORK/pkg/$b"
done
tar -czf "$FIX/sashiki_${VER}_darwin_arm64.tar.gz" -C "$WORK/pkg" sashiki sashikid
tar -czf "$FIX/sashiki_${VER}_darwin_amd64.tar.gz" -C "$WORK/pkg" sashiki sashikid
# linux deb: 中身は見ないので何かのバイト列で良い
printf 'not really a deb\n' > "$FIX/sashiki_${VER}_linux_amd64.deb"
printf 'not really a deb either\n' > "$FIX/sashiki_${VER}_linux_arm64.deb"
# goreleaser と同じ形("<sha256>  <name>")
sha() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}
: > "$FIX/checksums.txt"
for f in "$FIX"/sashiki_*; do
  printf '%s  %s\n' "$(sha "$f")" "$(basename "$f")" >> "$FIX/checksums.txt"
done

# latest API の応答。asset id と名前の対応を偽 curl が引く。
api_json() {
  local id=100
  # GitHub の実レスポンス同様、release 自体にも name がある。これを asset の
  # name と誤って組にすると、latest 経路が別 asset を掴む(#287)。property の
  # name はコロン前後を compact JSON と同じく空白無しに、url は空白ありにする(両方通ること)。
  echo '{"tag_name":"v'"$VER"'","name":"v'"$VER"'","assets":['
  local first=1
  for f in "$FIX"/sashiki_* "$FIX/checksums.txt"; do
    [ $first = 1 ] || echo ','
    first=0
    # url はコロン後に空白ありの形も混ぜて、install.sh のパースが両方通ることを見る。
    printf '{"url": "https://api.github.com/repos/rikukadev/sashiki/releases/assets/%d","name":"%s"}' "$id" "$(basename "$f")"
    id=$((id + 1))
  done
  echo ']}'
}
api_json > "$FIX/latest.json"

# ───── 偽コマンド ─────
mkdir -p "$WORK/bin"
export SASHIKI_FAKE_FIX="$FIX"
export SASHIKI_FAKE_LOG="$WORK/log"
: > "$SASHIKI_FAKE_LOG"

# 偽 curl: URL を見てフィクスチャを返す。-o があればそこへ、無ければ stdout。
cat > "$WORK/bin/curl" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
out=""; url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift 2 ;;
    -H) shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
echo "curl $url" >> "$SASHIKI_FAKE_LOG"
src=""
case "$url" in
  */releases/latest) src="$SASHIKI_FAKE_FIX/latest.json" ;;
  */releases/download/*/*) src="$SASHIKI_FAKE_FIX/${url##*/}" ;;
  */releases/assets/*)
    # id → name は latest.json の並び順(100 から連番)で引く
    id=${url##*/}
    name=$(grep -o '"url"[[:space:]]*:[[:space:]]*"[^"]*"\|"name":"[^"]*"' "$SASHIKI_FAKE_FIX/latest.json" \
      | awk '/^"url"/ { seen++ } seen == target && /^"name"/ { print; exit }' target="$((id - 99))" \
      | cut -d'"' -f4)
    src="$SASHIKI_FAKE_FIX/$name"
    ;;
esac
[ -n "$src" ] && [ -f "$src" ] || { echo "fake curl: 404 $url" >&2; exit 22; }
if [ -n "$out" ]; then cp "$src" "$out"; else cat "$src"; fi
FAKE

# 偽 uname / dpkg / apt-get: OS と arch を環境変数で決める
cat > "$WORK/bin/uname" <<'FAKE'
#!/usr/bin/env bash
case "${1:-}" in
  -s) echo "$SASHIKI_FAKE_OS" ;;
  -m) echo "$SASHIKI_FAKE_ARCH" ;;
  *) echo "$SASHIKI_FAKE_OS" ;;
esac
FAKE
cat > "$WORK/bin/dpkg" <<'FAKE'
#!/usr/bin/env bash
[ "${1:-}" = "--print-architecture" ] && { echo amd64; exit 0; }
echo "dpkg $*" >> "$SASHIKI_FAKE_LOG"
FAKE
cat > "$WORK/bin/apt-get" <<'FAKE'
#!/usr/bin/env bash
echo "apt-get $*" >> "$SASHIKI_FAKE_LOG"
FAKE
# Linux 経路の最後に `sashiki version` を呼ぶ
cat > "$WORK/bin/sashiki" <<'FAKE'
#!/usr/bin/env bash
echo "sashiki 0.0.1+fake"
FAKE
chmod +x "$WORK/bin/"*
export PATH="$WORK/bin:$PATH"

# run OS ARCH [env...] : install.sh を今の bash で走らせ、stdout+stderr を返す
run() {
  local os=$1 arch=$2; shift 2
  : > "$SASHIKI_FAKE_LOG"
  env SASHIKI_FAKE_OS="$os" SASHIKI_FAKE_ARCH="$arch" "$@" "$BASH" "$INSTALL" 2>&1
}

# ───── 1. darwin / latest API / GITHUB_TOKEN 無し(bash 3.2 で落ちていた経路)─────
log "darwin, latest API, token 無し"
DEST="$WORK/dest1"; mkdir -p "$DEST"
out=$(run Darwin arm64 SASHIKI_INSTALL_DIR="$DEST") || fail "install.sh が失敗した:
$out"
grep -q "verified: sashiki_${VER}_darwin_arm64.tar.gz" <<<"$out" || fail "checksum を照合していない:
$out"
[ -x "$DEST/sashiki" ] && [ -x "$DEST/sashikid" ] || fail "バイナリが置かれていない"
grep -q "releases/latest" "$SASHIKI_FAKE_LOG" || fail "latest API を引いていない"
echo "  ok"

# ───── 2. darwin / タグ直リンク ─────
log "darwin, SASHIKI_RELEASE=v$VER(直リンク)"
DEST="$WORK/dest2"; mkdir -p "$DEST"
out=$(run Darwin x86_64 SASHIKI_INSTALL_DIR="$DEST" SASHIKI_RELEASE="v$VER") || fail "直リンク経路が失敗した:
$out"
grep -q "verified: sashiki_${VER}_darwin_amd64.tar.gz" <<<"$out" || fail "直リンク経路で checksum を照合していない:
$out"
grep -q "releases/latest" "$SASHIKI_FAKE_LOG" && fail "タグ指定なのに latest API を引いている"
grep -q "releases/download/v$VER/checksums.txt" "$SASHIKI_FAKE_LOG" || fail "checksums.txt を直リンクで取っていない"
echo "  ok"

# ───── 3. linux / latest API / GITHUB_TOKEN あり(auth 配列が非空の経路)─────
log "linux, latest API, GITHUB_TOKEN あり"
out=$(run Linux x86_64 GITHUB_TOKEN=ghp_fake) || fail "linux 経路が失敗した:
$out"
grep -q "verified: sashiki_${VER}_linux_amd64.deb" <<<"$out" || fail "deb の checksum を照合していない:
$out"
grep -q "apt-get .*sashiki.deb" "$SASHIKI_FAKE_LOG" || fail "apt-get install を呼んでいない"
echo "  ok"

# ───── 4. 改竄した asset は止まる ─────
log "改竄した tar.gz を拒否する"
cp "$FIX/sashiki_${VER}_darwin_arm64.tar.gz" "$WORK/orig.tgz"
printf 'x' >> "$FIX/sashiki_${VER}_darwin_arm64.tar.gz"
DEST="$WORK/dest4"; mkdir -p "$DEST"
if out=$(run Darwin arm64 SASHIKI_INSTALL_DIR="$DEST"); then
  fail "改竄した asset で成功してしまった:
$out"
fi
grep -q "sha256 が checksums.txt と一致しません" <<<"$out" || fail "不一致の理由が出ていない:
$out"
[ -e "$DEST/sashiki" ] && fail "検証に落ちたのにバイナリを置いている"
cp "$WORK/orig.tgz" "$FIX/sashiki_${VER}_darwin_arm64.tar.gz"
echo "  ok"

# ───── 5. checksums.txt に行が無ければ止まる ─────
log "checksums.txt に行が無い asset を拒否する"
cp "$FIX/checksums.txt" "$WORK/sums.orig"
grep -v linux_amd64 "$WORK/sums.orig" > "$FIX/checksums.txt"
if out=$(run Linux x86_64); then
  fail "checksums.txt に無い asset で成功してしまった:
$out"
fi
grep -q "checksums.txt に .* の行がありません" <<<"$out" || fail "行が無い理由が出ていない:
$out"
grep -q "apt-get" "$SASHIKI_FAKE_LOG" && fail "検証に落ちたのに apt-get を呼んでいる"
cp "$WORK/sums.orig" "$FIX/checksums.txt"
echo "  ok"

# ───── 6. asset が無い場合は set -e で無言終了せず診断する ─────
log "checksums.txt asset が無い release を診断する"
cp "$FIX/latest.json" "$WORK/latest.orig"
grep -v '"name":"checksums.txt"' "$WORK/latest.orig" > "$FIX/latest.json"
DEST="$WORK/dest6"; mkdir -p "$DEST"
if out=$(run Darwin arm64 SASHIKI_INSTALL_DIR="$DEST"); then
  fail "checksums.txt asset が無いのに成功してしまった:
$out"
fi
grep -q "checksums.txt が release に無いため検証できません" <<<"$out" || fail "欠落理由が出ていない:
$out"
cp "$WORK/latest.orig" "$FIX/latest.json"
echo "  ok"

log "INSTALL.SH E2E PASSED ($BASH_VERSION)"
