[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string]$Select
)

$ErrorActionPreference = 'Stop'

if ($env:DOTFILES_TEST_NATIVE -ne '1') {
    throw 'This test installs and removes real tools. Set DOTFILES_TEST_NATIVE=1 only on a disposable runner.'
}
if (-not $env:V2_REF) { throw 'Set V2_REF to the last v2 commit or tag.' }

$Checkout = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..\..')).ProviderPath
$TempRoot = if ($env:RUNNER_TEMP) { $env:RUNNER_TEMP } else { [IO.Path]::GetTempPath() }
$Work = Join-Path $TempRoot ('dotfiles-migrate.' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $Work | Out-Null

function Write-Step {
    param([Parameter(Mandatory)] [string]$Name)
    Write-Output ''
    Write-Output "=== $Name ==="
}

function Invoke-Native {
    param(
        [Parameter(Mandatory)] [string]$Path,
        [string[]]$Arguments = @()
    )
    & $Path @Arguments
    if ($LASTEXITCODE -ne 0) { throw "$Path $($Arguments -join ' ') exited with code $LASTEXITCODE" }
}

function Invoke-Setup {
    param([Parameter(Mandatory)] [string[]]$Arguments)
    for ($attempt = 1; $attempt -le 4; $attempt++) {
        & (Join-Path $Checkout 'setup.ps1') @Arguments
        if ($LASTEXITCODE -eq 0) { return }
        if ($LASTEXITCODE -ne 3) { throw "setup $($Arguments -join ' ') exited with code $LASTEXITCODE" }
        Write-Output "e2e migrate: setup stopped for a new plan (exit 3), run $attempt"
    }
    throw "setup $($Arguments[0]) still returned exit 3 after three repeats"
}

if (& git -C $Checkout status --porcelain) { throw 'The checkout has changes. This test changes the checked out commit.' }
$PullRequestHead = (& git -C $Checkout rev-parse HEAD)
if ($LASTEXITCODE -ne 0) { throw 'git cannot read the checked out commit.' }
$Helper = Join-Path $Work 'helper.exe'
$env:CGO_ENABLED = '0'
Invoke-Native go @('build', '-o', $Helper, (Join-Path $Checkout 'tests\e2e\helper\main.go'))
$State = Join-Path ([Environment]::GetFolderPath([Environment+SpecialFolder]::LocalApplicationData)) 'dotfiles'
$PowerShell = (Get-Process -Id $PID).Path
$Launcher = @('--', $PowerShell, '-NoLogo', '-NoProfile', '-NonInteractive', '-File', (Join-Path $Checkout 'setup.ps1'))

try {
    Write-Step "Install $Select with v2 ($env:V2_REF)"
    Invoke-Native git @('-C', $Checkout, 'checkout', '--quiet', '--detach', $env:V2_REF)
    Invoke-Native $Helper (@('v2', '--mode', 'apply', '--select', $Select, '--state', $State) + $Launcher)
    Invoke-Native $Helper (@('v2', '--mode', 'check', '--state', $State) + $Launcher)

    Write-Step 'Preview the import'
    Invoke-Native git @('-C', $Checkout, 'checkout', '--quiet', '--detach', $PullRequestHead)
    Invoke-Setup @('plan')

    Write-Step 'Import'
    Invoke-Setup @('apply', '--yes')

    Write-Step 'Check the imported installation'
    Invoke-Setup @('check')

    Write-Step 'Restore v2'
    Invoke-Setup @('restore-v2', '--yes')

    Write-Step "Check with v2 ($env:V2_REF)"
    Invoke-Native git @('-C', $Checkout, 'checkout', '--quiet', '--detach', $env:V2_REF)
    Invoke-Native $Helper (@('v2', '--mode', 'check', '--state', $State) + $Launcher)
} finally {
    & git -C $Checkout checkout --quiet --detach $PullRequestHead
}

Write-Output ''
Write-Output 'e2e migrate: passed'
