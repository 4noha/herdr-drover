#!/bin/sh
# herdr-drover ワンライナー導入 / 更新スクリプト（cm install.sh と同型の雛形）。
#
#   curl -fsSL https://raw.githubusercontent.com/4noha/herdr-drover/main/install.sh | sh
#
# 現状 GitHub Release は未発行のため、Release 取得に失敗したら Go による
# ソースビルドへ fallback する（Release 発行後はそのまま DL 経路が有効になる）。
# 再実行で最新へ更新（冪等）。
# 環境変数:
#   HD_REPO     OWNER/REPO（既定 4noha/herdr-drover）
#   HD_VERSION  入れたい tag（既定 latest）
#   HD_BINDIR   インストール先（既定 ~/.local/bin）
set -eu

REPO="${HD_REPO:-4noha/herdr-drover}"
VER="${HD_VERSION:-latest}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in linux|darwin) ;; *) echo "未対応 OS: $os（Windows は out-of-scope）" >&2; exit 1;; esac
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "未対応 arch: $arch" >&2; exit 1 ;;
esac
asset="herdr-drover_${os}_${arch}"

if [ "$VER" = "latest" ]; then
  base="https://github.com/${REPO}/releases/latest/download"
else
  base="https://github.com/${REPO}/releases/download/${VER}"
fi

bindir="${HD_BINDIR:-$HOME/.local/bin}"
mkdir -p "$bindir"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fetched=0
if curl -fsSL --proto '=https' "$base/$asset" -o "$tmp/hd" 2>/dev/null; then
  # Release が取れたら checksums.txt の sha256 検証は必須（改竄/破損防止）。
  # ここで失敗したら fallback せず中止する（黙って未検証バイナリを置かない）。
  curl -fSL --proto '=https' "$base/checksums.txt" -o "$tmp/sums"
  want=$(grep " ${asset}\$" "$tmp/sums" | awk '{print $1}')
  [ -n "$want" ] || { echo "checksums.txt に $asset が無い" >&2; exit 1; }
  if command -v sha256sum >/dev/null 2>&1; then
    got=$(sha256sum "$tmp/hd" | awk '{print $1}')
  else
    got=$(shasum -a 256 "$tmp/hd" | awk '{print $1}')
  fi
  [ "$got" = "$want" ] || { echo "sha256 不一致（中止）" >&2; exit 1; }
  fetched=1
  echo "↓ Release ${REPO} ${VER} (${os}/${arch}) を取得・検証済み"
fi

if [ "$fetched" != 1 ]; then
  echo "Release が取得できない（未発行の可能性）→ ソースビルドへ fallback"
  command -v go  >/dev/null 2>&1 || { echo "go が無い（Go 1.25+ が必要）" >&2; exit 1; }
  command -v git >/dev/null 2>&1 || { echo "git が無い" >&2; exit 1; }
  git clone --quiet --depth 1 "https://github.com/${REPO}.git" "$tmp/src"
  (cd "$tmp/src" && sh scripts/build.sh) >/dev/null
  cp "$tmp/src/bin/herdr-drover" "$tmp/hd"
fi

chmod 0755 "$tmp/hd"
# rm→cp＝新 inode（macOS の同 inode 上書きは署名キャッシュ不整合で
# exec SIGKILL になる実挙動＝cm 教訓）。
rm -f "$bindir/herdr-drover"
cp "$tmp/hd" "$bindir/herdr-drover"
echo "✔ $bindir/herdr-drover"
"$bindir/herdr-drover" version 2>/dev/null || true

case ":$PATH:" in
  *":$bindir:"*) ;;
  *) echo "  ※ PATH に $bindir を追加してください（例: echo 'export PATH=\"$bindir:\$PATH\"' >> ~/.zshrc）" ;;
esac

echo "常駐（launchd）登録は: $bindir/herdr-drover install"
echo "  （GCP_PROJECT 等は env / ~/.herdr-drover/config の KEY=VALUE / enroll 済なら config.json から解決）"
