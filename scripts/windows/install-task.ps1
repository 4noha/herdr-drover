# Windows の常駐化（launchd 相当）。タスクスケジューラへ herdr-drover agent を
# 登録/更新する。**冪等**＝何度実行しても同じ状態に収束する。
#
#   powershell -ExecutionPolicy Bypass -File scripts\windows\install-task.ps1
#
# なぜ「ログオン時」だけでなく「周期」も登録するのか:
#   launchd の KeepAlive に相当する仕組みが Task Scheduler には無い。
#   タスクの「失敗時に再起動」は start-agent.ps1 が Start-Process で子を投げて
#   即 exit 0 する形では発火しない（タスクは常に成功扱い）。よって
#   **周期トリガ＋起動スクリプト側の多重起動チェック**で自己修復を作る。
#   実障害（2026-07-26）: self-update 失敗で daemon が消え、次のログオンまで
#   誰も戻さなかった。
#
# ⚠ install.go（launchd plist）は Windows 未対応＝当面この PowerShell が正。

param(
    [string]$TaskName      = 'herdr-drover-agent',
    [int]   $RepeatMinutes = 5,
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'

$repoScript = Join-Path $PSScriptRoot 'start-agent.ps1'
if (-not (Test-Path $repoScript)) { throw "起動スクリプトが見つからない: $repoScript" }

# 稼働コピーは repo と分離する（repo を消しても常駐が壊れない・cm 教訓の binDst 同型）
$binDir = Join-Path $env:USERPROFILE '.herdr-drover\bin'
$dst    = Join-Path $binDir 'start-agent.ps1'

$action = New-ScheduledTaskAction -Execute 'powershell.exe' `
    -Argument ('-NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File "{0}"' -f $dst)

# ログオン時（起動直後の復帰）＋ N 分ごと（死んでも自己修復）
# ⚠ `-RepetitionDuration ([TimeSpan]::MaxValue)` は使わない: XML が
# `P99999999DT23H59M59S` になり Task Scheduler に「範囲外」で蹴られる（実測）。
# RepetitionDuration を省略すると **無期限**（XML の Duration 空）になる。
$trigLogon  = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"
$trigRepeat = New-ScheduledTaskTrigger -Once -At (Get-Date).AddMinutes(1) `
    -RepetitionInterval (New-TimeSpan -Minutes $RepeatMinutes)

$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -StartWhenAvailable `
    -MultipleInstances IgnoreNew `
    -ExecutionTimeLimit ([TimeSpan]::Zero)

$principal = New-ScheduledTaskPrincipal -UserId "$env:USERDOMAIN\$env:USERNAME" `
    -LogonType Interactive -RunLevel Limited

if ($DryRun) {
    Write-Output "dry-run: 配置先      = $dst"
    Write-Output "dry-run: タスク名    = $TaskName"
    Write-Output "dry-run: トリガ      = ログオン時 ＋ $RepeatMinutes 分ごと（無期限）"
    Write-Output "dry-run: 実行ユーザー = $env:USERDOMAIN\$env:USERNAME（Interactive）"
    exit 0
}

if (-not (Test-Path $binDir)) { New-Item -ItemType Directory -Path $binDir -Force | Out-Null }
Copy-Item -Path $repoScript -Destination $dst -Force
Write-Output "配置: $dst"

Register-ScheduledTask -TaskName $TaskName -Action $action `
    -Trigger @($trigLogon, $trigRepeat) -Settings $settings -Principal $principal -Force | Out-Null
Write-Output "登録: $TaskName（ログオン時 ＋ $RepeatMinutes 分ごと）"

# 登録内容を読み戻して自己申告する（silent に想定と違う状態にしない）
$t = Get-ScheduledTask -TaskName $TaskName
foreach ($tr in $t.Triggers) {
    $rep = if ($tr.Repetition -and $tr.Repetition.Interval) { " repeat=$($tr.Repetition.Interval)" } else { '' }
    Write-Output ("  トリガ: {0}{1}" -f $tr.CimClass.CimClassName, $rep)
}
Write-Output ("  多重起動: {0} / 実行時間制限: {1}" -f $t.Settings.MultipleInstances, $t.Settings.ExecutionTimeLimit)
Write-Output "確認: Get-ScheduledTaskInfo -TaskName $TaskName"
