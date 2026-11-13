#Requires -Version 5.1
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$RepoRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$FixtureDir = Join-Path $RepoRoot 'testdata\kvfixture'
$BinDir = Join-Path $RepoRoot 'bin'
$FixtureBinDir = Join-Path $FixtureDir 'bin'

Write-Host '=== [1/5] Building synthesis CLI, loadgen, and linearizable-kv ===' -ForegroundColor Cyan
New-Item -ItemType Directory -Force -Path $BinDir | Out-Null
New-Item -ItemType Directory -Force -Path $FixtureBinDir | Out-Null

Push-Location -LiteralPath $RepoRoot
try {
    & go build -trimpath -ldflags='-s -w' -o (Join-Path $BinDir 'synthesis.exe') .\cmd\synthesis
    if ($LASTEXITCODE -ne 0) { throw "go build synthesis failed ($LASTEXITCODE)" }

    & go build -trimpath -ldflags='-s -w' -o (Join-Path $FixtureBinDir 'linearizable-kv.exe') .\cmd\synthesis-oracle-linearizable
    if ($LASTEXITCODE -ne 0) { throw "go build linearizable-kv failed ($LASTEXITCODE)" }
} finally {
    Pop-Location
}

Push-Location -LiteralPath $FixtureDir
try {
    & go build -trimpath -ldflags='-s -w' -o (Join-Path $FixtureBinDir 'loadgen.exe') .\cmd\loadgen
    if ($LASTEXITCODE -ne 0) { throw "go build loadgen failed ($LASTEXITCODE)" }

    Write-Host '=== [2/5] Building and smoke-booting kvfixture images (buggy & kvfixed) ===' -ForegroundColor Cyan
    foreach ($variant in @('buggy', 'kvfixed')) {
        $env:KV_VARIANT = $variant
        $env:KV_PREFIX = "syn_smoke_$variant"
        try {
            & docker compose build kv-n1
            if ($LASTEXITCODE -ne 0) { throw "docker compose build kv-n1 ($variant) failed ($LASTEXITCODE)" }
            & docker compose up --build -d
            if ($LASTEXITCODE -ne 0) { throw "docker compose up --build ($variant) failed ($LASTEXITCODE)" }
        } finally {
            $prevEAP = $ErrorActionPreference
            $ErrorActionPreference = 'Continue'
            & docker compose down -v --remove-orphans *> $null
            $ErrorActionPreference = $prevEAP
            Remove-Item Env:\KV_VARIANT -ErrorAction SilentlyContinue
            Remove-Item Env:\KV_PREFIX -ErrorAction SilentlyContinue
        }
    }
} finally {
    Pop-Location
}

$SynthesisExe = Join-Path $BinDir 'synthesis.exe'

Write-Host '=== Verifying tamper-evident test lock (.synthesis/lock) ===' -ForegroundColor Cyan
& $SynthesisExe verify-lock --project $FixtureDir
if ($LASTEXITCODE -ne 0) { throw "verify-lock failed ($LASTEXITCODE)" }

Write-Host "`n=== [3/5] Scenario 1 (AT-1): Defective Raft read-lease caught with witness ===" -ForegroundColor Cyan
& $SynthesisExe run --project $FixtureDir --profile buggy --seed 20261109
$at1Code = $LASTEXITCODE
if ($at1Code -ne 1) {
    throw "AT-1 expected exit code 1 (FAIL), got $at1Code"
}
$latestBuggy = Get-ChildItem -LiteralPath (Join-Path $FixtureDir '.synthesis\runs') -Directory |
    Sort-Object LastWriteTimeUtc -Descending |
    Select-Object -First 1
if ($null -ne $latestBuggy) {
    $verdictJson = Join-Path $latestBuggy.FullName 'verdict.json'
    if (Test-Path -LiteralPath $verdictJson) {
        & $SynthesisExe explain $verdictJson
        if ($LASTEXITCODE -ne 0) { throw "synthesis explain failed ($LASTEXITCODE)" }
    }
}

Write-Host "`n=== [4/5] Scenario 2 (AT-2): Fixed Raft quorum-read passes full locked budget ===" -ForegroundColor Cyan
& $SynthesisExe run --project $FixtureDir --profile patched --seed 20261109
$at2Code = $LASTEXITCODE
if ($at2Code -ne 0) {
    throw "AT-2 expected exit code 0 (PASS), got $at2Code"
}

Write-Host "`n=== [5/5] Scenario 3 (AT-3): Quiet workload weakening refused before boot ===" -ForegroundColor Cyan
$tamperDir = Join-Path ([System.IO.Path]::GetTempPath()) ("synthesis-tamper-" + [System.Guid]::NewGuid().ToString('N').Substring(0, 8))
New-Item -ItemType Directory -Force -Path $tamperDir | Out-Null
try {
    Copy-Item -LiteralPath (Join-Path $FixtureDir 'synthesis.yaml') -Destination (Join-Path $tamperDir 'synthesis.yaml')
    Copy-Item -LiteralPath (Join-Path $FixtureDir 'docker-compose.yaml') -Destination (Join-Path $tamperDir 'docker-compose.yaml')
    New-Item -ItemType Directory -Force -Path (Join-Path $tamperDir '.synthesis\oracles') | Out-Null
    Copy-Item -LiteralPath (Join-Path $FixtureDir '.synthesis\lock') -Destination (Join-Path $tamperDir '.synthesis\lock')
    Copy-Item -LiteralPath (Join-Path $FixtureDir '.synthesis\oracles\linearizable.kv.yaml') -Destination (Join-Path $tamperDir '.synthesis\oracles\linearizable.kv.yaml')

    $tamperCfg = Join-Path $tamperDir 'synthesis.yaml'
    $raw = [System.IO.File]::ReadAllText($tamperCfg)
    [System.IO.File]::WriteAllText($tamperCfg, $raw.Replace('ops: 60000', 'ops: 100'))

    & $SynthesisExe run --project $tamperDir --profile buggy
    $at3Code = $LASTEXITCODE
    if ($at3Code -ne 4) {
        throw "AT-3 expected exit code 4 (ORACLE_DRIFT), got $at3Code"
    }
} finally {
    Remove-Item -LiteralPath $tamperDir -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Host "`n=== DEMO SUMMARY ===" -ForegroundColor Green
Write-Host "  Scenario 1 (AT-1, defective fixture): FAIL         (exit $at1Code) - witnessed stale read + witness.svg"
Write-Host "  Scenario 2 (AT-2, fixed fixture):     PASS         (exit $at2Code) - full locked budget consumed"
Write-Host "  Scenario 3 (AT-3, weakened workload): ORACLE_DRIFT (exit $at3Code) - refused before cluster boot"
