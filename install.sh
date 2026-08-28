#!/bin/sh
# herdr-drover ワンライナー導入 / 更新スクリプト（cm install.sh と同型の雛形）。
#
#   curl -fsSL https://raw.githubusercontent.com/4noha/herdr-drover/main/install.sh | sh
#
# 現状 GitHub Release は未発行のため、Release 取得に失敗したら Go による
# ソースビルドへ fallback する（Release 発行後はそのまま DL 経路が有効になる）。
# 再実行で最新へ更新（冪等）。
# 環境変数:
#   HD_REPO        OWNER/REPO（既定 4noha/herdr-drover）
#   HD_VERSION     入れたい tag（既定 latest）
#   HD_BINDIR      インストール先（既定 ~/.local/bin）
#   HD_SKILLS      0 なら Claude Code Skill を入れない（既定 1）
#   HD_SKILLDIR    Skill の配置先（既定 ~/.claude/skills）
#   HD_SKILLS_ONLY 1 ならバイナリを触らず Skill だけ入れる（既に本体がある PC 用）
set -eu

REPO="${HD_REPO:-4noha/herdr-drover}"
VER="${HD_VERSION:-latest}"
SKILLS="${HD_SKILLS:-1}"
SKILLDIR="${HD_SKILLDIR:-$HOME/.claude/skills}"
SKILLS_ONLY="${HD_SKILLS_ONLY:-0}"

# 同梱 Skill の一覧。**skills/ に足したらここにも足すこと**（リポの checkout が
# 手元にある経路では実ディレクトリを列挙するので、この表が効くのは
# raw.githubusercontent から個別に取ってくる経路だけ）。
SKILL_NAMES="mv-tab herdr-drover"

# install_one_skill <name> <SKILL.md のパス>
# 既存を勝手に潰さない: symlink（開発用の checkout 直結）と、herdr-drover が
# 置いたのではないディレクトリには触らず、**理由を出して skip する**（silent 変更禁止）。
install_one_skill() {
  _name="$1"; _src="$2"; _dst="$SKILLDIR/$_name"
  if [ -L "$_dst" ]; then
    echo "  - $_name: symlink 済み（$(readlink "$_dst") ＝開発用）→ 触らない"
    return 0
  fi
  if [ -e "$_dst" ] && [ ! -f "$_dst/.herdr-drover-installed" ]; then
    echo "  ! $_name: $_dst は herdr-drover 管理外 → skip（中身を確認して手動で入れてください）" >&2
    return 0
  fi
  mkdir -p "$_dst"
  cp "$_src" "$_dst/SKILL.md"
  printf '%s %s\n' "$REPO" "$VER" > "$_dst/.herdr-drover-installed"
  echo "  ✔ $_name → $_dst/SKILL.md"
}

# skills_srcdir は手元にある herdr-drover checkout の skills/ を返す
# （無ければ空文字＝raw 取得へ）。ソースビルド fallback の clone と、リポの
# checkout から直接叩いた場合を拾う。
#
# ⚠ 「skills/ があるディレクトリ」で判定しない。たまたま skills/ を持つ無関係な
# cwd から叩かれたとき他人の SKILL.md を配ってしまう（推測で分類しない＝鉄則）。
# **go.mod の module 行 exact 一致**を checkout の証拠にする。
is_drover_checkout() {
  [ -f "$1/go.mod" ] || return 1
  grep -qx "module github.com/4noha/herdr-drover" "$1/go.mod"
}

skills_srcdir() {
  for _c in "$tmp/src" "$PWD"; do
    is_drover_checkout "$_c" || continue
    [ -d "$_c/skills" ] || continue
    echo "$_c/skills"
    return 0
  done
  echo ""
  return 0
}

# install_skills <skills ディレクトリ or 空文字>
# 第 1 引数があればそこから copy、無ければ raw.githubusercontent から個別に取得する。
install_skills() {
  _srcdir="${1:-}"
  [ "$SKILLS" = "1" ] || { echo "Skill 導入は HD_SKILLS=0 で無効化されています"; return 0; }
  mkdir -p "$SKILLDIR"
  echo "Claude Code Skill を $SKILLDIR へ:"
  if [ -n "$_srcdir" ] && [ -d "$_srcdir" ]; then
    for _d in "$_srcdir"/*/; do
      [ -f "$_d/SKILL.md" ] || continue
      install_one_skill "$(basename "$_d")" "$_d/SKILL.md"
    done
    return 0
  fi
  _ref=main; [ "$VER" = "latest" ] || _ref="$VER"
  for _n in $SKILL_NAMES; do
    if curl -fsSL --proto '=https' \
        "https://raw.githubusercontent.com/${REPO}/${_ref}/skills/${_n}/SKILL.md" \
        -o "$tmp/skill-$_n.md" 2>/dev/null; then
      install_one_skill "$_n" "$tmp/skill-$_n.md"
    else
      # ⚠ ${} で括る。全角括弧の直前で $_ref と書くと後続バイトを変数名に
      # 食われて "unbound variable" で落ちる（macOS /bin/sh で実測）。
      echo "  ! $_n: 取得できませんでした（ref=${_ref}）→ skip" >&2
    fi
  done
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

os=$(uname -s | tr '[:upper:]' '[:lower:]')
# ${os} の括りは必須（全角括弧が変数名に食われる。上の _ref と同じ既知バグ）。
case "$os" in linux|darwin) ;; *) echo "未対応 OS: ${os}（Windows は out-of-scope）" >&2; exit 1;; esac
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

if [ "$SKILLS_ONLY" = "1" ]; then
  echo "HD_SKILLS_ONLY=1 ＝ バイナリには触らず Skill だけ入れます"
  install_skills "$(skills_srcdir)"
  exit 0
fi

mkdir -p "$bindir"

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

install_skills "$(skills_srcdir)"

echo "常駐（launchd）登録は: $bindir/herdr-drover install"
echo "  （GCP_PROJECT 等は env / ~/.herdr-drover/config の KEY=VALUE / enroll 済なら config.json から解決）"
