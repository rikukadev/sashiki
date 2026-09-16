#!/usr/bin/env bash
# sashiki インストーラ。
#   Linux : 最新 release の deb を取得して dpkg -i する。
#   macOS : 最新 release の darwin tar.gz を /usr/local/bin へ展開する(#118)。
#     curl -fsSL https://raw.githubusercontent.com/rikukadev/sashiki/main/install.sh | sudo bash
# private リポジトリの間は GITHUB_TOKEN(repo 読み取り)が必要:
#     curl -fsSL -H "Authorization: Bearer $GITHUB_TOKEN" .../install.sh | sudo -E bash
#
# macOS 標準の /bin/bash は 3.2 なので、4.x 以降の構文は使わない(#287)。
# 特に `set -u` 下の空配列展開 "${arr[@]}" は 3.2 では unbound variable になる。
set -euo pipefail

REPO=rikukadev/sashiki
OS=$(uname -s)

auth=()
if [ -n "${GITHUB_TOKEN:-}" ]; then
  auth=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
fi
api="https://api.github.com/repos/${REPO}/releases/latest"

# curl_auth ARGS... : GITHUB_TOKEN があれば付けて curl する。
# ${auth[@]+"${auth[@]}"} は bash 3.2 で空配列を安全に展開する書き方。
curl_auth() {
  curl -fsSL ${auth[@]+"${auth[@]}"} "$@"
}

# SASHIKI_RELEASE がタグ(例 v0.4.1)なら releases/download の直リンクで取得する。
# latest API 経由の JSON パースで非 deb を掴む事故を避ける(#166)。
# "latest" / "main" / 空 のときは従来どおり latest API を使う。
REL="${SASHIKI_RELEASE:-}"

# direct_url NAME : 直リンク URL を返す(タグ未指定なら空を返す)。
direct_url() {
  if [ -z "$REL" ] || [ "$REL" = latest ] || [ "$REL" = main ]; then
    return 0
  fi
  printf 'https://github.com/%s/releases/download/%s/%s' "$REPO" "$REL" "$1"
}

# find_asset PATTERN : 最新 release から PATTERN を名前に含む asset を探し、
# "URL<TAB>NAME" を 1 行返す(無ければ空)。checksums.txt の照合に NAME が要る。
find_asset() {
  local pattern="$1"
  curl_auth "$api" \
    | grep -o "\"url\": *\"[^\"]*assets/[0-9]*\"\|\"name\": *\"[^\"]*\"" \
    | paste - - \
    | grep "$pattern" \
    | head -1 \
    | sed -E 's/^"url": *"([^"]*)".*"name": *"([^"]*)".*$/\1\t\2/'
}

# resolve NAME_IF_TAGGED PATTERN : 取得先を決める。url と name をグローバルに置く。
#   タグ指定なら直リンク(名前は決め打ち)、そうでなければ latest API から探す。
resolve() {
  url=$(direct_url "$1")
  name="$1"
  if [ -z "$url" ]; then
    local line
    line=$(find_asset "$2")
    url=""; name=""
    [ -n "$line" ] && IFS=$'\t' read -r url name <<<"$line"
  fi
}

# fetch URL DEST : release asset を落とす。API の asset URL は octet-stream を要求する。
fetch() {
  curl_auth -H "Accept: application/octet-stream" -o "$2" "$1"
}

# verify FILE NAME CHECKSUMS : goreleaser の checksums.txt("<sha256>  <name>")と照合する。
# 行が無い・値が違う、のどちらも失敗にする(検証を黙って飛ばさない)。
verify() {
  local file="$1" name="$2" sums="$3" want got
  want=$(awk -v n="$name" '$2 == n { print $1 }' "$sums" | head -1)
  if [ -z "$want" ]; then
    echo "install.sh: checksums.txt に $name の行がありません" >&2
    return 1
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    got=$(sha256sum "$file" | awk '{ print $1 }')
  else
    got=$(shasum -a 256 "$file" | awk '{ print $1 }')
  fi
  if [ "$want" != "$got" ]; then
    echo "install.sh: $name の sha256 が checksums.txt と一致しません" >&2
    echo "  expected: $want" >&2
    echo "  actual:   $got" >&2
    return 1
  fi
  echo "verified: $name (sha256)"
}

# download_verified NAME_IF_TAGGED PATTERN DEST : asset と checksums.txt を落として照合する。
download_verified() {
  local dest="$3"
  resolve "$1" "$2"
  if [ -z "$url" ]; then
    echo "install.sh: $2 に一致する asset が見つかりません" >&2
    exit 1
  fi
  local asset_name="$name"
  echo "downloading $asset_name ..."
  fetch "$url" "$dest"

  resolve checksums.txt checksums.txt
  if [ -z "$url" ]; then
    echo "install.sh: checksums.txt が release に無いため検証できません" >&2
    exit 1
  fi
  fetch "$url" "$tmp/checksums.txt"
  verify "$dest" "$asset_name" "$tmp/checksums.txt"
}

case "$OS" in
Linux)
  ARCH=$(dpkg --print-architecture 2>/dev/null || uname -m)
  case "$ARCH" in
    x86_64) ARCH=amd64 ;;
    aarch64) ARCH=arm64 ;;
  esac
  case "$ARCH" in amd64 | arm64) ;; *) echo "install.sh: unsupported arch: $ARCH" >&2; exit 1 ;; esac

  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  # タグ指定時の asset 名は sashiki_<ver>_linux_<arch>.deb(goreleaser の name_template)。
  download_verified "sashiki_${REL#v}_linux_${ARCH}.deb" "_linux_${ARCH}.deb" "$tmp/sashiki.deb"
  # 起動直後は dpkg ロックを他プロセスが握っていることがある。apt-get 経由なら
  # DPkg::Lock::Timeout でロック解放を待てる(#192)。古い apt では dpkg にフォールバック。
  apt-get -o DPkg::Lock::Timeout=300 install -y "$tmp/sashiki.deb" || dpkg -i "$tmp/sashiki.deb"
  echo "installed: $(sashiki version)"
  echo "next: sudo sashiki init --pool dbpool --device <dev>  (lsblk でデバイス確認)"
  ;;

Darwin)
  ARCH=$(uname -m)
  case "$ARCH" in
    x86_64) ARCH=amd64 ;;
    arm64) ARCH=arm64 ;;
  esac
  case "$ARCH" in amd64 | arm64) ;; *) echo "install.sh: unsupported arch: $ARCH" >&2; exit 1 ;; esac

  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  download_verified "sashiki_${REL#v}_darwin_${ARCH}.tar.gz" "_darwin_${ARCH}.tar.gz" "$tmp/sashiki.tar.gz"
  tar -xzf "$tmp/sashiki.tar.gz" -C "$tmp"
  # /usr/local/bin へ設置(書けなければ sudo を促す)。
  dest=${SASHIKI_INSTALL_DIR:-/usr/local/bin}
  install_bin() { install -m 0755 "$tmp/$1" "$dest/$1"; }
  if [ -w "$dest" ]; then
    install_bin sashiki
    install_bin sashikid
  else
    echo "install.sh: $dest への書き込みに sudo が要ります"
    sudo install -m 0755 "$tmp/sashiki" "$dest/sashiki"
    sudo install -m 0755 "$tmp/sashikid" "$dest/sashikid"
  fi
  echo "installed: $("$dest/sashiki" version)"
  echo "next: sashiki init --platform darwin  (Homebrew mysql + apfs + launchd。詳細は docs/LOCAL-DEV.md)"
  ;;

*)
  echo "install.sh: unsupported OS: $OS (Linux / Darwin のみ)" >&2
  exit 1
  ;;
esac
