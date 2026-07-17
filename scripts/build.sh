#!/bin/sh
# herdr plugin [[build]] 実体（GitHub `herdr plugin install` 時のみ実行される。
# `plugin link` では走らない＝link 開発時は手動で `sh scripts/build.sh`）。
#
# 実測済みの実行条件（herdr 0.7.4）:
#   - cwd = checkout の manifest_root（このリポジトリ root）
#   - HERDR_* runtime env は scrub 済み（素のユーザ環境＝go/git は PATH 前提）
#   - 失敗（非 0 exit）で install 中止・末尾 64KB がユーザに表示される
#   - ⚠ herdr-plugin.toml を書き換えると preview 不一致で install 中止
#     ＝出力は bin/ のみ（.gitignore 済み）
#
# 将来: Release 発行後は go build の代わりに Release バイナリ DL＋sha256 検証へ
# 切替可能（install.sh と同じ asset 名 herdr-drover_<os>_<arch>）。
set -eu

command -v go >/dev/null 2>&1 || {
  echo "go が見つからない（herdr-drover のビルドに Go 1.25+ が必要）" >&2
  exit 1
}

# VERSION は cm と同型の ldflags 注入（shallow checkout では describe が
# tag を持たないことがある→ その場合は commit hash / dev）。
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)

mkdir -p bin
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=${VERSION}" \
  -o bin/herdr-drover ./cmd/herdr-drover

# ビルド成果物の自己申告（install ログに版が残る＝障害調査の一次情報）
./bin/herdr-drover version
