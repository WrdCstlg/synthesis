#Requires -Version 5.1
# Automated repository hygiene, size, binary, commit-identity, network, and denylist audit.
#
# Generic checks run unconditionally. Confidential denylist patterns are read
# exclusively from the LEAK_AUDIT_DENYLIST environment variable (CI secret) or
# an untracked local file outside the repository (~/.claude/private/public-demo-denylist.txt).
# On a confidential-list match, only the 1-based rule index ("private-term #N")
# is printed, never the matched term. When unconfigured, the confidential leg
# reports SKIPPED and is never reported as PASS.

[CmdletBinding()]
param(
    [string]$RepoRoot = ''
)

$ErrorActionPreference = 'Stop'
if ([string]::IsNullOrWhiteSpace($RepoRoot)) {
    $RepoRoot = Split-Path -Parent $PSScriptRoot
}
$RepoRoot = (Resolve-Path -LiteralPath $RepoRoot).Path

$Failures = New-Object System.Collections.Generic.List[string]

$PublicTerms = @(
    @{ label = 'generic:host-user-dir';        pattern = '[A-Za-z]:[\\/]Users[\\/]|(^|[\s"''`])/Users/[A-Za-z0-9._-]+' },
    @{ label = 'generic:register-id';          pattern = '\b(D|OQ|BL|D-WS|D-PUB)-\d{2,4}\b' },
    @{ label = 'generic:retired-product-name'; pattern = '(?<!syn)(?<!hypo)the[s]is' },
    @{ label = 'generic:stripped-citation';    pattern = 'build[_]brief|design\s+record\.' }
)

$PrivatePatterns = New-Object System.Collections.Generic.List[string]
function Add-PrivateEntry([string]$rawLine) {
    if ([string]::IsNullOrWhiteSpace($rawLine)) { return }
    $trimmed = $rawLine.Trim()
    if ($trimmed.StartsWith('#')) { return }
    if ($trimmed.Length -lt 3) { return }
    if ($trimmed.StartsWith('regex:')) {
        $rx = $trimmed.Substring(6).Trim()
        if ($rx.Length -ge 3) {
            $PrivatePatterns.Add($rx)
        }
    } else {
        $PrivatePatterns.Add([regex]::Escape($trimmed))
    }
}

$PrivateSource = ''
if (-not [string]::IsNullOrWhiteSpace($env:LEAK_AUDIT_DENYLIST)) {
    $PrivateSource = 'env:LEAK_AUDIT_DENYLIST'
    foreach ($ln in ($env:LEAK_AUDIT_DENYLIST -split "\r?\n")) {
        Add-PrivateEntry $ln
    }
} else {
    $candidateFile = $env:LEAK_AUDIT_DENYLIST_FILE
    if ([string]::IsNullOrWhiteSpace($candidateFile)) {
        $candidateFile = Join-Path $HOME '.claude\private\public-demo-denylist.txt'
    }
    if (-not [string]::IsNullOrWhiteSpace($candidateFile) -and (Test-Path -LiteralPath $candidateFile -PathType Leaf)) {
        $PrivateSource = 'local-file'
        foreach ($ln in [System.IO.File]::ReadAllLines($candidateFile)) {
            Add-PrivateEntry $ln
        }
    }
}

Push-Location -LiteralPath $RepoRoot
try {
    $relFiles = @(& git ls-files --cached --others --exclude-standard)
    if ($LASTEXITCODE -ne 0) {
        throw "git ls-files failed ($LASTEXITCODE)"
    }
} finally {
    Pop-Location
}

$MaxBytes = 5 * 1024 * 1024
$AllowedOwners = @('piercepartners', 'wrdcstlg')
$GhOwnerRegex = New-Object System.Text.RegularExpressions.Regex('github\.com/([A-Za-z0-9_.-]+)', [System.Text.RegularExpressions.RegexOptions]::IgnoreCase)
$ExpectedName = 'Senan Sumrein'
$ExpectedEmail = '186500574+WrdCstlg@users.noreply.github.com'

foreach ($rel in $relFiles) {
    if ([string]::IsNullOrWhiteSpace($rel)) { continue }
    $normRel = $rel.Replace('\', '/')
    $full = Join-Path $RepoRoot $rel
    if (-not (Test-Path -LiteralPath $full -PathType Leaf)) { continue }

    foreach ($entry in $PublicTerms) {
        if ([regex]::IsMatch($normRel, $entry.pattern, [System.Text.RegularExpressions.RegexOptions]::IgnoreCase)) {
            $Failures.Add("filename hit [$($entry.label)]: $normRel")
        }
    }
    for ($idx = 0; $idx -lt $PrivatePatterns.Count; $idx++) {
        if ([regex]::IsMatch($normRel, $PrivatePatterns[$idx], [System.Text.RegularExpressions.RegexOptions]::IgnoreCase)) {
            $Failures.Add("filename hit [private-term #$($idx + 1)]: $normRel")
        }
    }

    $fi = Get-Item -LiteralPath $full -Force
    if ($fi.Length -gt $MaxBytes) {
        $Failures.Add("size violation (>5MB): $normRel ($($fi.Length) bytes)")
    }

    $ext = [System.IO.Path]::GetExtension($normRel).ToLowerInvariant()
    $forbiddenExts = @('.exe', '.dll', '.so', '.dylib', '.bin', '.bundle', '.tar', '.gz', '.zip', '.7z', '.wasm')
    if ($forbiddenExts -contains $ext) {
        $Failures.Add("binary extension forbidden: $normRel")
    }

    $bytes = [System.IO.File]::ReadAllBytes($full)
    $hasNull = $false
    $scanLen = [Math]::Min($bytes.Length, 8192)
    for ($b = 0; $b -lt $scanLen; $b++) {
        if ($bytes[$b] -eq 0) {
            $hasNull = $true
            break
        }
    }
    if ($hasNull) {
        $isAllowedDocImage = $normRel.StartsWith('docs/') -and (@('.png', '.jpg', '.jpeg', '.gif', '.webp') -contains $ext)
        if (-not $isAllowedDocImage) {
            $Failures.Add("binary file outside docs/ images: $normRel")
        }
        continue
    }

    $text = [System.Text.Encoding]::UTF8.GetString($bytes)

    foreach ($entry in $PublicTerms) {
        if ([regex]::IsMatch($text, $entry.pattern, [System.Text.RegularExpressions.RegexOptions]::IgnoreCase)) {
            $Failures.Add("content hit [$($entry.label)] in $normRel")
        }
    }

    foreach ($m in $GhOwnerRegex.Matches($text)) {
        $owner = $m.Groups[1].Value.ToLowerInvariant()
        if ($AllowedOwners -notcontains $owner) {
            $Failures.Add("disallowed GitHub owner in ${normRel}: github.com/$($m.Groups[1].Value)")
        }
    }

    for ($idx = 0; $idx -lt $PrivatePatterns.Count; $idx++) {
        if ([regex]::IsMatch($text, $PrivatePatterns[$idx], [System.Text.RegularExpressions.RegexOptions]::IgnoreCase)) {
            $Failures.Add("content hit [private-term #$($idx + 1)] in $normRel")
        }
    }

    if ($normRel.EndsWith('.go') -and -not $normRel.EndsWith('_test.go')) {
        $urlMatches = [regex]::Matches($text, 'https?://[^\s"''`]+', [System.Text.RegularExpressions.RegexOptions]::IgnoreCase)
        foreach ($um in $urlMatches) {
            $u = $um.Value
            $allowedLocal = ($u -match '^http://(127\.0\.0\.1|localhost|\{host\}|n[123]-peer|kv-n[123]|%s|www\.w3\.org/2000/svg)')
            if (-not $allowedLocal) {
                $Failures.Add("external URL in runtime Go source ${normRel}: $u")
            }
        }
    }
}

Push-Location -LiteralPath $RepoRoot
try {
    $prevEAP = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    & git rev-parse --verify HEAD *> $null
    $hasHead = ($LASTEXITCODE -eq 0)
    $ErrorActionPreference = $prevEAP

    if ($hasHead) {
        $commits = @(& git log --format='%H|%an|%ae|%cn|%ce')
        foreach ($c in $commits) {
            $parts = $c -split '\|', 5
            $sha = $parts[0]
            $an = $parts[1]
            $ae = $parts[2]
            $cn = $parts[3]
            $ce = $parts[4]
            if ($an -cne $ExpectedName -or $ae -cne $ExpectedEmail -or $cn -cne $ExpectedName -or $ce -cne $ExpectedEmail) {
                $Failures.Add("commit $sha does not match required author/committer identity ($ExpectedName <$ExpectedEmail>)")
            }
        }
        $fullMessages = (& git log --format='%H%n%B%n---END---') -join "`n"
        $postRootMessages = ''
        $prevEAP2 = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        & git cat-file -e d03c24733659bb6bd0e4c8fa4ad507dab4e4825d *> $null
        $hasLegacyRoot = ($LASTEXITCODE -eq 0)
        $ErrorActionPreference = $prevEAP2
        if ($hasLegacyRoot) {
            $postRootMessages = (& git log d03c24733659bb6bd0e4c8fa4ad507dab4e4825d..HEAD --format='%H%n%B%n---END---') -join "`n"
        } else {
            $postRootMessages = $fullMessages
        }
        $trailerPat = '[Cc]o-[Aa]uthored-[Bb]y:|[Ss]igned-[Oo]ff-[Bb]y:'
        if ([regex]::IsMatch($fullMessages, $trailerPat)) {
            $Failures.Add("commit history contains attribution or sign-off trailer")
        }
        foreach ($entry in $PublicTerms) {
            $msgTarget = if ($entry.label -eq 'generic:retired-product-name') { $postRootMessages } else { $fullMessages }
            if (-not [string]::IsNullOrWhiteSpace($msgTarget) -and [regex]::IsMatch($msgTarget, $entry.pattern, [System.Text.RegularExpressions.RegexOptions]::IgnoreCase)) {
                $Failures.Add("commit message hit [$($entry.label)]")
            }
        }
        for ($idx = 0; $idx -lt $PrivatePatterns.Count; $idx++) {
            if ([regex]::IsMatch($fullMessages, $PrivatePatterns[$idx], [System.Text.RegularExpressions.RegexOptions]::IgnoreCase)) {
                $Failures.Add("commit message hit [private-term #$($idx + 1)]")
            }
        }
    }
} finally {
    Pop-Location
}

if ($Failures.Count -gt 0) {
    Write-Error ("LEAK AUDIT FAILED ($($Failures.Count) finding(s)):`n  - " + ($Failures -join "`n  - "))
    exit 1
}

if ($PrivatePatterns.Count -gt 0) {
    Write-Host ("leak-audit: public checks PASS ({0} file(s)); private denylist PASS ({1} rule(s) from {2} -> 0 hits)" -f $relFiles.Count, $PrivatePatterns.Count, $PrivateSource) -ForegroundColor Green
} else {
    if ($env:LEAK_AUDIT_REQUIRE_PRIVATE -eq '1') {
        Write-Error 'LEAK AUDIT FAILED: LEAK_AUDIT_REQUIRE_PRIVATE=1 and no private denylist is configured'
        exit 1
    }
    Write-Host ("leak-audit: public checks PASS ({0} file(s))" -f $relFiles.Count) -ForegroundColor Green
    Write-Host 'leak-audit: private denylist SKIPPED (neither LEAK_AUDIT_DENYLIST secret nor ~/.claude/private/public-demo-denylist.txt is configured)' -ForegroundColor Yellow
}
exit 0
