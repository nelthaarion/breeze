<#
.SYNOPSIS
    Repairs Breeze v2 module import paths that lost the slash after "v2".

.DESCRIPTION
    A bad bulk-edit rewrote "github.com/nelthaarion/breeze/<pkg>" into
    "github.com/nelthaarion/breeze/v2<pkg>" - the major-version element got
    glued onto the package element, so the paths no longer resolve.

    This script restores the separator:

        github.com/nelthaarion/breeze/v2events -> github.com/nelthaarion/breeze/v2/events
        github.com/nelthaarion/breeze/v2client -> github.com/nelthaarion/breeze/v2/client
        github.com/nelthaarion/breeze/v2scalar -> github.com/nelthaarion/breeze/v2/scalar
        github.com/nelthaarion/breeze/v2diag   -> github.com/nelthaarion/breeze/v2/diag

    It also normalizes the misplaced-slash variant ("breeze/v/2diag") to
    "breeze/v2/diag" before the main pass.

    Files are read and written as raw text, so CRLF/LF line endings and any
    existing byte-order mark are preserved. Files whose content does not change
    are left untouched, so their timestamps are not bumped.

.PARAMETER Path
    Root directory to process. Defaults to the script's own directory.

.PARAMETER All
    Also fix every other package glued to "v2" (middlewares, dashboard, fleet,
    workflow, observability, video, internal, ...) with a generic rule instead
    of only the four packages named above.

.PARAMETER DryRun
    Report what would change without writing anything.

.EXAMPLE
    .\fix-imports.ps1 -DryRun
    .\fix-imports.ps1
    .\fix-imports.ps1 -All
#>
[CmdletBinding()]
param(
    [string] $Path = $PSScriptRoot,
    [switch] $All,
    [switch] $DryRun
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if (-not $Path) { $Path = (Get-Location).Path }
$root = (Resolve-Path -LiteralPath $Path).Path

# ---------------------------------------------------------------------------
# Replacement rules, applied in order. Keys are regexes, values are the
# replacement text (${pkg} refers to a named capture group).
# ---------------------------------------------------------------------------
$module = [regex]::Escape('github.com/nelthaarion/breeze')

$rules = [ordered]@{}

# Misplaced slash: breeze/v/2diag -> breeze/v2/diag
$rules["$module/v/2(?<pkg>[A-Za-z])"] = 'github.com/nelthaarion/breeze/v2/${pkg}'

if ($All) {
    # Any package element glued to the major version, e.g. v2middlewares.
    # The lookahead keeps already-correct "v2/..." and bare "v2" untouched.
    $rules["$module/v2(?=[A-Za-z])"] = 'github.com/nelthaarion/breeze/v2/'
}
else {
    foreach ($pkg in 'events', 'client', 'scalar', 'diag') {
        $rules["$module/v2$pkg"] = "github.com/nelthaarion/breeze/v2/$pkg"
    }
}

# ---------------------------------------------------------------------------
# File selection
# ---------------------------------------------------------------------------
$includeExtensions = @(
    '.go', '.mod', '.work', '.md', '.mdx', '.tmpl', '.gotmpl', '.tpl',
    '.yml', '.yaml', '.json', '.txt', '.toml', '.ps1', '.sh', '.dockerfile'
)
$includeNames = @('Dockerfile', 'Makefile', 'go.mod', 'go.work')
$excludeDirs = @('.git', 'vendor', 'node_modules', '.idea', '.vscode', 'dist', 'bin', 'obj')

# Never rewrite this script: its own help text contains the broken patterns.
$selfPath = if ($PSCommandPath) { $PSCommandPath } else { '' }

$files = Get-ChildItem -LiteralPath $root -Recurse -File -Force |
    Where-Object {
        $relative = $_.FullName.Substring($root.Length).TrimStart('\', '/')
        $segments = $relative -split '[\\/]'
        $parents = if ($segments.Count -gt 1) { $segments[0..($segments.Count - 2)] } else { @() }

        ($_.FullName -ne $selfPath) -and
        (-not ($parents | Where-Object { $excludeDirs -contains $_ })) -and
        ($includeExtensions -contains $_.Extension.ToLowerInvariant() -or $includeNames -contains $_.Name) -and
        ($_.Length -gt 0) -and ($_.Length -lt 20MB)
    }

# ---------------------------------------------------------------------------
# Rewrite
# ---------------------------------------------------------------------------
$utf8NoBom = New-Object System.Text.UTF8Encoding($false)
$utf8Bom = New-Object System.Text.UTF8Encoding($true)

$changedFiles = 0
$totalHits = 0
$scanned = 0

foreach ($file in $files) {
    $scanned++

    $bytes = [System.IO.File]::ReadAllBytes($file.FullName)
    $hasBom = $bytes.Length -ge 3 -and
              $bytes[0] -eq 0xEF -and $bytes[1] -eq 0xBB -and $bytes[2] -eq 0xBF

    $offset = if ($hasBom) { 3 } else { 0 }
    $original = [System.Text.Encoding]::UTF8.GetString($bytes, $offset, $bytes.Length - $offset)

    # Cheap pre-filter: skip files that mention no Breeze module path at all.
    if (-not $original.Contains('nelthaarion/breeze/v')) { continue }

    $updated = $original
    $fileHits = 0
    $ruleHits = [ordered]@{}

    foreach ($pattern in $rules.Keys) {
        $found = [regex]::Matches($updated, $pattern)
        if ($found.Count -eq 0) { continue }

        $ruleHits[$pattern] = $found.Count
        $fileHits += $found.Count
        $updated = [regex]::Replace($updated, $pattern, $rules[$pattern])
    }

    if ($fileHits -eq 0 -or $updated -ceq $original) { continue }

    $changedFiles++
    $totalHits += $fileHits

    $relative = $file.FullName.Substring($root.Length).TrimStart('\', '/')
    $verb = if ($DryRun) { 'would fix' } else { 'fixed' }
    Write-Host ("{0,-9} {1,3}  {2}" -f $verb, $fileHits, $relative)

    foreach ($pattern in $ruleHits.Keys) {
        Write-Verbose ("    {0} x  {1}  ->  {2}" -f $ruleHits[$pattern], $pattern, $rules[$pattern])
    }

    if (-not $DryRun) {
        $encoding = if ($hasBom) { $utf8Bom } else { $utf8NoBom }
        [System.IO.File]::WriteAllText($file.FullName, $updated, $encoding)
    }
}

Write-Host ''
Write-Host ("Scanned {0} file(s) under {1}" -f $scanned, $root)
if ($DryRun) {
    Write-Host ("Dry run: {0} occurrence(s) in {1} file(s) would be rewritten." -f $totalHits, $changedFiles)
}
else {
    Write-Host ("Rewrote {0} occurrence(s) in {1} file(s)." -f $totalHits, $changedFiles)
}
if (-not $All) {
    Write-Host 'Note: only events, client, scalar and diag were targeted. Re-run with -All to fix every package glued to v2.'
}
