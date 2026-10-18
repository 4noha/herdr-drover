# herdr-drover agent の起動スクリプト（Windows）。install-task.ps1 が
# ~/.herdr-drover/bin/ へ配置し、タスクスケジューラが **ログオン時＋周期**で
# 呼ぶ。周期呼び出しが自己修復（daemon が死んでも次の tick で戻る）を担うので、
# **「既に動いていれば何もしない」が常態**である前提で書くこと。
#
# ⚠ 実障害（2026-07-26）: self-update が SAC にブロックされた新バイナリで
# 置換して daemon が消えたが、タスクは logon トリガのみ・RestartCount=0 で
# **誰も復帰させなかった**。周期トリガはこの穴を塞ぐためのもの。
# なお Task Scheduler の「失敗時に再起動」設定はここでは無力: 本スクリプトは
# Start-Process で子を投げて即 exit 0 する＝タスク自体は常に成功扱いになる。

$ErrorActionPreference = 'SilentlyContinue'

$herdrBin = Join-Path $env:LOCALAPPDATA 'Programs\Herdr\bin'
$env:Path = "$herdrBin;$env:Path"
$logDir   = Join-Path $env:USERPROFILE '.herdr-drover'
$drover   = Join-Path $logDir 'bin\herdr-drover.exe'
$errLog   = Join-Path $logDir 'agent.err.log'
$outLog   = Join-Path $logDir 'agent.out.log'
$pidFile  = Join-Path $logDir 'agent.pid'

# --- 既に稼働中なら即終了 -------------------------------------------------
# ⚠ ここを飛ばして Start-Process すると `-RedirectStandardError` が
# **稼働中 daemon のログを切り詰める**（死因の証拠ごと消える）。周期起動では
# 毎 tick でそれが起きるので、多重起動の抑止は pidfile ロック任せにしない。
#
# ⚠ **プロセス名だけで判定しない**: ↗窓（注入 pane）の attach client も
# 同じ herdr-drover.exe（`herdr-drover.exe attach <pc> <sid>`）で、注入 pane が
# ある限り常駐する。名前判定だと daemon が死んでも「稼働中」と誤認して**永久に
# 復帰させない**（2026-10-18 実測: update-all 後の設計どおりの exit から 7 分以上
# 戻らなかった）。コマンドラインが `agent` で終わるものだけを daemon とみなす。
function Get-DroverAgent {
    Get-CimInstance Win32_Process -Filter "Name='herdr-drover.exe'" -ErrorAction SilentlyContinue |
        Where-Object { $_.CommandLine -match '\sagent\s*$' }
}
if (Get-DroverAgent) { exit 0 }

if (-not (Test-Path $drover)) { exit 0 }  # 未インストール＝何もしない

# --- herdr server（無ければヘッドレス起動。二重起動は herdr 側が弾く）----
if (-not (Get-Process -Name herdr -ErrorAction SilentlyContinue)) {
    Start-Process -FilePath (Join-Path $herdrBin 'herdr.exe') -ArgumentList 'server' -WindowStyle Hidden
    Start-Sleep -Seconds 3
}

# --- ログを 1 世代退避してから起動 ---------------------------------------
# Start-Process のリダイレクトは追記でなく truncate なので、退避しないと
# 「なぜ落ちたか」が再起動のたびに消える（復旧調査の一次情報を失う）。
# ⚠ **空ログは退避しない**: 起動自体が失敗し続ける状況（例 SAC が新バイナリを
# 評価するまで弾く間）では、この関数が 5 分ごとに呼ばれて「空ログを .1 へ退避」
# を繰り返し、**本当の死因が入った .1 を数分で上書きしてしまう**。
$fi = Get-Item $errLog -ErrorAction SilentlyContinue
if ($fi -and $fi.Length -gt 0) { Move-Item -Path $errLog -Destination "$errLog.1" -Force }

Start-Process -FilePath $drover -ArgumentList 'agent' -WindowStyle Hidden `
    -RedirectStandardOutput $outLog -RedirectStandardError $errLog
