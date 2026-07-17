#!/bin/sh
# herdr action "install" の薄いラッパ。
#
# 実測済みの実行条件（herdr 0.7.4）:
#   - cwd = plugin_root で spawn される＝相対 bin/ 参照でよい
#   - ワンショット spawn（常駐禁止）・stdout/stderr 64KB cap
#     ＝ここでは登録だけ行い、常駐本体は launchd が起動する
#   - env に HERDR_SOCKET_PATH 等の runtime 変数が入っている＝
#     `herdr-drover install` がそのまま plist へ焼き込める
#
# GCP_PROJECT 等が env に無い場合、install は ~/.herdr-drover/config
# （KEY=VALUE）→ ~/.herdr-drover/config.json（enroll の永続設定）の順で
# 解決する。どこにも無ければ明示エラーで案内が出る。
set -eu

if [ ! -x bin/herdr-drover ]; then
  echo "bin/herdr-drover が無い。GitHub 'herdr plugin install' なら [[build]] が作る。" >&2
  echo "link 開発中なら先に: sh scripts/build.sh" >&2
  exit 1
fi

exec ./bin/herdr-drover install
