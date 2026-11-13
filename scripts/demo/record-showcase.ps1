#Requires -Version 5.1
# Record the four showcase scenes against testdata/kvfixture and render SVGs.
#
#   powershell -ExecutionPolicy Bypass -File scripts/demo/record-showcase.ps1
#   powershell -ExecutionPolicy Bypass -File scripts/demo/record-showcase.ps1 -RenderOnly

[CmdletBinding()]
param(
    [string]$Media = 'docs/media',
    [switch]$RenderOnly
)

$ErrorActionPreference = 'Stop'
$RepoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location -LiteralPath $RepoRoot

$DemoDir = Join-Path $Media 'demo'
New-Item -ItemType Directory -Force -Path $DemoDir | Out-Null

$IsWinHost = ($env:OS -eq 'Windows_NT')
$Exe = if ($IsWinHost) { '.exe' } else { '' }
$SynthesisRel = "../../bin/synthesis$Exe"
$Fixture = 'testdata/kvfixture'
$RunsDir = Join-Path $Fixture '.synthesis/runs'

$SceneDefs = @(
    @{ step = '01-catch';    title = 'catch the planted stale read';      expect = 1 },
    @{ step = '02-patched';  title = 'patch the defect and prove the fix'; expect = 0 },
    @{ step = '03-cheat';    title = 'try to weaken the test';            expect = 4 },
    @{ step = '04-narrowed'; title = 'test less, prove less';             expect = 2 }
)

function Invoke-RenderAll([string]$CatchRunId) {
    foreach ($s in $SceneDefs) {
        $castPath = Join-Path $DemoDir "$($s.step).cast"
        $svgPath = Join-Path $Media "$($s.step).svg"
        & node scripts/demo/render-cast-svg.mjs $castPath $svgPath --title $s.title
        if ($LASTEXITCODE -ne 0) { throw "render-cast-svg failed for $($s.step)" }
    }
    if ($CatchRunId) {
        $verdictPath = Join-Path $RunsDir "$CatchRunId/verdict.json"
        if (Test-Path -LiteralPath $verdictPath) {
            & node scripts/demo/witness-svg.mjs $verdictPath (Join-Path $Media 'witness.svg')
            if ($LASTEXITCODE -ne 0) { throw 'witness-svg.mjs failed' }
        }
    }
}

if ($RenderOnly) {
    Invoke-RenderAll ''
    exit 0
}

$BinPath = Join-Path $RepoRoot "bin/synthesis$Exe"
if (-not (Test-Path -LiteralPath $BinPath)) {
    throw "$BinPath is missing; build ./cmd/synthesis first"
}

function Get-ExistingRuns {
    if (-not (Test-Path -LiteralPath $RunsDir)) { return @() }
    return @(Get-ChildItem -LiteralPath $RunsDir -Directory | ForEach-Object { $_.Name })
}

function Invoke-RecordScene([string]$Name, [string]$Title, [int]$Expect, [object[]]$Steps) {
    $spec = [ordered]@{
        title = $Title
        cols  = 100
        rows  = 24
        steps = $Steps
    }
    $specPath = Join-Path $DemoDir "$Name.steps.json"
    $json = $spec | ConvertTo-Json -Depth 8
    [System.IO.File]::WriteAllText($specPath, $json, (New-Object System.Text.UTF8Encoding($false)))

    $before = Get-ExistingRuns
    $castPath = Join-Path $DemoDir "$Name.cast"
    try {
        & node scripts/demo/record.mjs $specPath $castPath | Write-Host
        if ($LASTEXITCODE -ne 0) { throw "record.mjs failed for $Name" }
    } finally {
        if (Test-Path -LiteralPath $specPath) {
            Remove-Item -LiteralPath $specPath -Force
        }
    }

    $firstLine = Get-Content -LiteralPath $castPath -TotalCount 1
    $header = $firstLine | ConvertFrom-Json
    $execSteps = @($header.synthesis.steps | Where-Object { $null -ne $_.exit_code })
    $lastStep = $execSteps[-1]
    if ([int]$lastStep.exit_code -ne $Expect) {
        throw "$Name returned exit $($lastStep.exit_code), expected $Expect"
    }

    $after = Get-ExistingRuns
    $created = @($after | Where-Object { $before -notcontains $_ })
    return $created
}

# Scene 1: Catch the planted stale read (FAIL, exit 1) + explain verdict.
$catchRuns = Invoke-RecordScene '01-catch' 'catch the planted stale read' 1 @(
    @{
        show = 'synthesis verify-lock'
        argv = @($SynthesisRel, 'verify-lock')
        cwd  = $Fixture
    },
    @{
        show = 'synthesis run --profile buggy --seed 20261109'
        argv = @($SynthesisRel, 'run', '--profile', 'buggy', '--seed', '20261109')
        cwd  = $Fixture
    }
)
$catchRunId = $catchRuns[0]

# Scene 2: Fixed variant over the full locked 2-world budget (PASS, exit 0).
[void](Invoke-RecordScene '02-patched' 'patch the defect and prove the fix' 0 @(
    @{
        show = 'synthesis run --profile patched --seed 20261109'
        argv = @($SynthesisRel, 'run', '--profile', 'patched', '--seed', '20261109')
        cwd  = $Fixture
    }
))

# Scene 3: Quietly weaken workload ops: 60000 -> 100; refused before boot (ORACLE_DRIFT, exit 4).
$origYamlPath = Join-Path $RepoRoot "$Fixture/synthesis.yaml"
$origYamlBytes = [System.IO.File]::ReadAllBytes($origYamlPath)
try {
    [void](Invoke-RecordScene '03-cheat' 'try to weaken the test' 4 @(
        @{
            show = "sed -i 's/ops: 60000/ops: 100/' synthesis.yaml"
            edit = @{
                file = "$Fixture/synthesis.yaml"
                from = 'ops: 60000'
                to   = 'ops: 100'
            }
        },
        @{
            show = 'synthesis run --profile patched'
            argv = @($SynthesisRel, 'run', '--profile', 'patched')
            cwd  = $Fixture
        }
    ))
} finally {
    [System.IO.File]::WriteAllBytes($origYamlPath, $origYamlBytes)
}

# Scene 4: Narrow budget below locked 2 worlds (--worlds 1); PASS refused (INCONCLUSIVE, exit 2).
[void](Invoke-RecordScene '04-narrowed' 'test less, prove less' 2 @(
    @{
        show = 'synthesis run --profile patched --worlds 1 --seed 20261109'
        argv = @($SynthesisRel, 'run', '--profile', 'patched', '--worlds', '1', '--seed', '20261109')
        cwd  = $Fixture
    }
))

Invoke-RenderAll $catchRunId
Write-Host "Recorded and rendered 4 showcase scenes + witness.svg (catch run: $catchRunId)" -ForegroundColor Green
