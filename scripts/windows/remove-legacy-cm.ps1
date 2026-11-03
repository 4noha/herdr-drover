# cm（claude-master）時代の常駐を Windows から外して herdr-drover へ一本化する。冪等。
#
#   powershell -ExecutionPolicy Bypass -File scripts\windows\remove-legacy-cm.ps1 [-DryRun]
#
# **管理者の PowerShell** で実行する（S4U タスクの削除に要る）。-DryRun は変更せず計画だけ表示。
#
# なぜ要るか（2026-11-03 実測）: cm の cloud agent（S4U タスク claude-master-cloud）が残って
# いると、cm の reconcile が herdr-drover の全セッションへ ↗ viewer を作ろうとして
#   - 相手 PC の wake/{pc} へ 1 分に数十回 書く（相手の agent は wake ごとに空振りの
#     source 接続 → 5 秒で畳む、を繰り返す）
#   - 旧 relay（Cloud Run）の /session へ viewer として dial して 403 を受け続ける
# ＝止めるだけでは次のログオンで戻るので、タスクごと外す。
#
# 行うこと:
#   1. S4U タスク claude-master-monitor / claude-master-cloud を停止・削除
#   2. claude-master.exe のプロセスを停止
#   3. User PATH から cm のシム dir（~\.claude-master\bin）を外す（`claude` が cm に吸われない）
#   4. ~\.claude-master を ~\.claude-master.bak-<日付> へ退避（削除はしない。~\go\bin の
#      claude-master.exe も残す＝常駐経路さえ無ければ無害）
#   5. herdr-drover を最新へ（herdr-drover.exe update）
#   6. 稼働中の herdr-drover プロセス（daemon・↗窓の attach）を止める＝周期タスク
#      （install-task.ps1）が 1 分以内に新版の daemon を起こし、daemon が ↗窓を作り直す
param([switch]$DryRun)
$ErrorActionPreference = 'Continue'
function Step([string]$m){ Write-Host "==> $m" -ForegroundColor Cyan }
function Act([string]$what,[scriptblock]$act){
  if($DryRun){ Write-Host "  [dry-run] $what" -ForegroundColor DarkYellow } else { Write-Host "  $what"; & $act }
}

Step "1. cm の常駐タスク"
foreach($tn in 'claude-master-monitor','claude-master-cloud'){
  if(Get-ScheduledTask -TaskName $tn -ErrorAction SilentlyContinue){
    Act "stop + unregister task $tn" {
      Stop-ScheduledTask -TaskName $tn -ErrorAction SilentlyContinue
      Unregister-ScheduledTask -TaskName $tn -Confirm:$false
    }
  } else { Write-Host "  task $tn: 無し" }
}

Step "2. cm のプロセス"
$ps = @(Get-Process -Name 'claude-master' -ErrorAction SilentlyContinue)
if($ps.Count -gt 0){ Act "stop $($ps.Count) claude-master.exe" { $ps | Stop-Process -Force } }
else { Write-Host "  claude-master.exe: 動いていない" }

Step "3. User PATH の cm シム"
$cm   = Join-Path $env:USERPROFILE '.claude-master'
$shim = (Join-Path $cm 'bin').TrimEnd('\')
$cur  = [Environment]::GetEnvironmentVariable('Path','User')
$parts = @(($cur -split ';') | Where-Object { $_ -ne '' })
$keep  = @($parts | Where-Object { $_.TrimEnd('\') -ne $shim })
if($keep.Count -ne $parts.Count){
  Act "remove $shim from User PATH" { [Environment]::SetEnvironmentVariable('Path', ($keep -join ';'), 'User') }
} else { Write-Host "  PATH に cm シム無し" }

Step "4. cm のディレクトリを退避"
if(Test-Path $cm){
  $bak = "$cm.bak-$(Get-Date -Format yyyyMMdd)"
  Act "rename $cm -> $bak" { Rename-Item -Path $cm -NewName (Split-Path $bak -Leaf) }
} else { Write-Host "  $cm: 無し" }

Step "5. herdr-drover を最新へ"
$hd = Join-Path $env:USERPROFILE '.herdr-drover\bin\herdr-drover.exe'
if(Test-Path $hd){
  Act "$hd update" { & $hd update }
  & $hd version
} else { Write-Host "  $hd が無い（scripts\windows\install-task.ps1 で導入する）" }

Step "6. herdr-drover の稼働プロセスを入れ替える"
$hp = @(Get-Process -Name 'herdr-drover' -ErrorAction SilentlyContinue)
if($hp.Count -gt 0){ Act "stop $($hp.Count) herdr-drover.exe（周期タスクが新版で起こし直す）" { $hp | Stop-Process -Force } }
else { Write-Host "  herdr-drover.exe: 動いていない（周期タスクが起こす）" }
Write-Host "完了。1〜2 分後に `"$hd status`" で daemon の版を確認する。"
