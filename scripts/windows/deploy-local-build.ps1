# Windows 機へ **ローカルビルド**を配置する（配布物が SAC に弾かれる環境向け）。
#
#   powershell -ExecutionPolicy Bypass -File scripts\windows\deploy-local-build.ps1 -Version v0.5.32
#   （-DryRun ならビルドと起動確認だけ行い、稼働 exe には触れない）
#
# なぜ手作業でなくスクリプトか＝Windows 固有の落とし穴が 3 つあり、順序を誤ると
# daemon が起動不能のまま放置されるため（2026-07-26 に実際に起きた）:
#
#   1. **SAC は新しいファイルを一時的に実行拒否する**（未評価の間だけ・実測で
#      数分〜十数分）。配置してから気づくと復旧手段が無くなるので、**差し替える
#      前に必ず新バイナリを起動できることを確かめ**、評価が付くまで待つ。
#   2. **version は配布タグと同じ値を stamp する**。dev 版数のままだとクラウドが
#      「最新でない」と判定して update-all を再投入し、probe 無しの経路だと
#      再び落ちる。
#   3. **稼働 exe は上書きせず rename 退避**（Windows は実行中イメージを上書き
#      できない／退避 rename は許される。ロールバック点にもなる）。
#
# 前提: この機の稼働バイナリは配布物ではなくローカルビルド。`update` は
# probe 入り（drover-cloud v0.1.15+）が載るまで実行しないこと。

param(
    [Parameter(Mandatory = $true)][string]$Version,
    [string]$RepoDir,
    [string]$TaskName = 'herdr-drover-agent',
    [int]   $ProbeTimeoutMinutes = 20,
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'

# ⚠ param ブロックの既定値では $PSScriptRoot が空になる（本体でのみ有効）＝
# 既定の解決は本体で行う。
if (-not $RepoDir) {
    $here = Split-Path -Parent $MyInvocation.MyCommand.Path
    $RepoDir = (Resolve-Path (Join-Path $here '..\..')).Path
}

$binDir   = Join-Path $env:USERPROFILE '.herdr-drover\bin'
$dst      = Join-Path $binDir 'herdr-drover.exe'
$logPath  = Join-Path $env:USERPROFILE '.herdr-drover\agent.err.log'
$pidPath  = Join-Path $env:USERPROFILE '.herdr-drover\agent.pid'
$built    = Join-Path $RepoDir 'bin\herdr-drover.exe'

function Say($m) { Write-Output ("[deploy] " + $m) }

# 起動確認に渡す引数（副作用が無く即 exit 0 する呼び出し）。
$ProbeArgLine = 'version'

# Invoke-Probe は exe を実際に起動して {Started, ExitCode, Output} を返す。
#
# ⚠ **PowerShell の呼び出し演算子を使わない**理由:
#   - `& $exe version 2>&1` は PS 5.1 で native exe の stderr を ErrorRecord に
#     包み、$ErrorActionPreference='Stop' 下では **1 回目の失敗で throw** する＝
#     「評価が付くまでポーリングする」という安全弁の目的が壊れる（実際に壊れた）。
#   - 引数配列の解釈を PowerShell に委ねない（argv に exe パスが紛れる事象を
#     一度観測しており、原因は特定できていない。ProcessStartInfo なら
#     コマンドラインを此方で完全に決められる）。
# SAC 等でイメージのロードが拒否される場合は Process.Start が例外を投げるので
# Started=$false で返す＝**判定不能は全て「起動できない」に倒す（fail-closed）**。
function Invoke-Probe([string]$Exe, [string]$ArgLine) {
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName               = $Exe
    $psi.Arguments              = $ArgLine
    $psi.UseShellExecute        = $false
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError  = $true
    $psi.CreateNoWindow         = $true
    try { $p = [System.Diagnostics.Process]::Start($psi) }
    catch { return [pscustomobject]@{ Started = $false; ExitCode = -1; Output = $_.Exception.Message } }
    $so = $p.StandardOutput.ReadToEnd()
    $se = $p.StandardError.ReadToEnd()
    if (-not $p.WaitForExit(60000)) { try { $p.Kill() } catch {}; return [pscustomobject]@{ Started = $true; ExitCode = -1; Output = '60s で終了しない' } }
    return [pscustomobject]@{ Started = $true; ExitCode = $p.ExitCode; Output = ($so + $se).Trim() }
}

# --- 1. ビルド（配布と同じ version を stamp）--------------------------------
Say "repo    = $RepoDir"
Say "version = $Version"
Push-Location $RepoDir
try {
    $env:CGO_ENABLED = '0'
    & go build -trimpath -ldflags "-s -w -X main.version=$Version" -o $built ./cmd/herdr-drover
    if ($LASTEXITCODE -ne 0) { throw "go build 失敗 (exit=$LASTEXITCODE)" }
} finally { Pop-Location }
Say ("built   = {0} ({1:N0} bytes)" -f $built, (Get-Item $built).Length)

# 依存の自己申告（probe 入りかを配置前に確認できるようにする）
$dep = (& go version -m $built 2>$null | Select-String 'drover-cloud').ToString().Trim()
Say "dep     = $dep"

# --- 2. SAC ゲート: 起動できるまで待つ（できなければ配置しない）-------------
Say "起動確認（SAC の評価待ちを最大 $ProbeTimeoutMinutes 分ポーリング）..."
$deadline = (Get-Date).AddMinutes($ProbeTimeoutMinutes)
$ok = $false
Say "probe: exe=[$built] args=[$ProbeArgLine]"
while ((Get-Date) -lt $deadline) {
    $r = Invoke-Probe -Exe $built -ArgLine $ProbeArgLine
    if ($r.Started -and $r.ExitCode -eq 0 -and $r.Output -match [regex]::Escape($Version)) { $ok = $true; break }
    $why = if (-not $r.Started) { "起動できない: $($r.Output)" } else { "exit=$($r.ExitCode) out='$($r.Output)'" }
    Say "  まだ通らない（$why）— 30 秒後に再試行"
    Start-Sleep -Seconds 30
}
if (-not $ok) {
    throw "新バイナリが $ProbeTimeoutMinutes 分たっても実行できない（SAC 評価待ち?）。" +
          "**稼働バイナリには触れていない**ので現状は無傷。時間をおいて再実行すること。"
}
Say "起動確認 OK: $(& $built version)"

if ($DryRun) {
    Say "dry-run: ここで終了（稼働 exe・daemon には触れていない）"
    exit 0
}

# --- 3. 自 daemon だけ停止（裸の kill は恒久禁止）---------------------------
if (Test-Path $pidPath) {
    $agentPid = [int](Get-Content $pidPath -TotalCount 1).Trim()
    $proc = Get-Process -Id $agentPid -ErrorAction SilentlyContinue
    if ($proc -and $proc.ProcessName -eq 'herdr-drover') {
        Say "停止: pid $agentPid"
        Stop-Process -Id $agentPid -Force
        Start-Sleep -Seconds 2
    } else { Say "停止不要（pid $agentPid は不在＝stale pidfile）" }
}

# --- 4. 退避 rename → 配置（ロールバック点を残す）---------------------------
$retired = Join-Path $binDir ("herdr-drover.exe.prev-" + (Get-Date -Format 'yyyyMMdd-HHmmss'))
if (Test-Path $dst) { Rename-Item $dst $retired; Say "退避: $retired" }
Copy-Item $built $dst
Say "配置: $dst"

# --- 5. 起動して確認。ダメなら退避したものへ戻す ----------------------------
Start-ScheduledTask -TaskName $TaskName
Say "タスク実行: $TaskName（起動待ち）"
$up = $false
for ($i = 0; $i -lt 20; $i++) {
    Start-Sleep -Seconds 3
    $p = Get-Process -Name herdr-drover -ErrorAction SilentlyContinue
    if ($p) { $up = $true; break }
}
if (-not $up) {
    Say "🔴 起動しない → ロールバックする"
    Remove-Item $dst -Force
    Rename-Item $retired $dst
    Start-ScheduledTask -TaskName $TaskName
    throw "新バイナリで daemon が起動しなかったため退避版へ戻した（$retired → $dst）。"
}

Say "起動: pid $((Get-Process -Name herdr-drover | Select-Object -First 1).Id)"
Start-Sleep -Seconds 5
Say "--- 起動ログ ---"
Get-Content $logPath -TotalCount 6 | ForEach-Object { "        $_" }
$errs = (Select-String -Path $logPath -Pattern 'tick エラー|spawn 失敗' -AllMatches | Measure-Object).Count
Say "起動直後のエラー行: $errs（0 が期待値）"
Say "完了。ロールバックが要るときは $retired を herdr-drover.exe へ戻して Start-ScheduledTask。"
