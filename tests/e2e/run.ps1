[CmdletBinding()]
param(
    [string]$Select = '',
    [string]$Exclude = '',
    [string]$Case = ''
)

$ErrorActionPreference = 'Stop'

if ($env:DOTFILES_TEST_NATIVE -ne '1') {
    throw 'This test installs and removes real tools. Set DOTFILES_TEST_NATIVE=1 only on a disposable runner.'
}

$Checkout = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..\..')).ProviderPath
$TempRoot = if ($env:RUNNER_TEMP) { $env:RUNNER_TEMP } else { [IO.Path]::GetTempPath() }
$Work = Join-Path $TempRoot ('dotfiles-e2e.' + [Guid]::NewGuid().ToString('N'))
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
        if ($env:DOTFILES_E2E_INSTALLER) {
            $env:DOTFILES_CHECKOUT = $Checkout
            & $env:DOTFILES_E2E_INSTALLER @Arguments
        } else {
            & (Join-Path $Checkout 'setup.ps1') @Arguments
        }
        if ($LASTEXITCODE -eq 0) { return }
        if ($LASTEXITCODE -ne 3) { throw "setup $($Arguments -join ' ') exited with code $LASTEXITCODE" }
        Write-Output "e2e: setup stopped for a new plan (exit 3), run $attempt"
    }
    throw "setup $($Arguments[0]) still returned exit 3 after three repeats"
}

function Add-PersonalText {
    param(
        [Parameter(Mandatory)] [string]$Path,
        [Parameter(Mandatory)] [string]$Text
    )
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $Path) | Out-Null
    [IO.File]::AppendAllText($Path, $Text, (New-Object System.Text.UTF8Encoding($false)))
}

$Helper = $env:DOTFILES_E2E_HELPER
if (-not $Helper) {
    $Helper = Join-Path $Work 'helper.exe'
    $env:CGO_ENABLED = '0'
    Invoke-Native go @('build', '-o', $Helper, (Join-Path $Checkout 'tests\e2e\helper\main.go'))
}

$HomeFolder = [Environment]::GetFolderPath([Environment+SpecialFolder]::UserProfile)
$Documents = [Environment]::GetFolderPath([Environment+SpecialFolder]::MyDocuments)
$AppData = [Environment]::GetFolderPath([Environment+SpecialFolder]::ApplicationData)
$LocalAppData = [Environment]::GetFolderPath([Environment+SpecialFolder]::LocalApplicationData)
$State = Join-Path $LocalAppData 'dotfiles'
$Skip = "$State,$Work"

$System = (& $Helper system)
if ($LASTEXITCODE -ne 0) { throw 'The helper cannot name this system.' }
$Name = if ($Case) { "$System.$Case" } else { $System }
$Retained = Join-Path $Checkout "tests\e2e\expected\$Name.retained"
$Remains = Join-Path $Checkout "tests\e2e\expected\$Name.remains"
if (-not (Test-Path -LiteralPath $Retained -PathType Leaf) -or -not (Test-Path -LiteralPath $Remains -PathType Leaf)) {
    throw "tests/e2e/expected has no $Name.retained and $Name.remains"
}
if (-not $Select) {
    $Select = (& $Helper ids --checkout $Checkout "--exclude=$Exclude")
    if ($LASTEXITCODE -ne 0) { throw 'The helper cannot list the menu IDs.' }
}
$Shared = (& $Helper shared --checkout $Checkout)
if ($LASTEXITCODE -ne 0) { throw 'The helper cannot list the shared resources.' }
Write-Output "e2e: system $Name"
Write-Output "e2e: selection $Select"

Write-Step 'Personal files and the baseline of the home folder'
Add-PersonalText (Join-Path $Documents 'PowerShell\Microsoft.PowerShell_profile.ps1') "# personal PowerShell line`n`$env:DOTFILES_E2E_PERSONAL = 'pwsh'`n"
Add-PersonalText (Join-Path $Documents 'WindowsPowerShell\Microsoft.PowerShell_profile.ps1') "# personal Windows PowerShell line`n`$env:DOTFILES_E2E_PERSONAL = 'powershell'`n"
Add-PersonalText (Join-Path $HomeFolder '.gitconfig') "[alias]`n`te2e = status --short`n"
$CodeSettings = Join-Path $AppData 'Code\User\settings.json'
if (-not (Test-Path -LiteralPath $CodeSettings)) {
    Add-PersonalText $CodeSettings "{`n  // personal comment`n  `"dotfiles.e2e.personal`": true`n}`n"
}
$Baseline = Join-Path $Work 'baseline.tsv'
Invoke-Native $Helper @('snapshot', '--root', $HomeFolder, '--exclude', $Skip, '--out', $Baseline)

Write-Step 'Install the selection'
Invoke-Setup @('apply', '--yes', "--select=$Select")

Write-Step 'Smoke each private build'
Invoke-Native $Helper @('smoke', '--checkout', $Checkout, '--state', $State)

Write-Step 'Check, update, check'
Invoke-Setup @('check')
Invoke-Setup @('update', '--yes')
Invoke-Setup @('check')

Write-Step 'Remove the selection and the shared resources'
Invoke-Setup @('apply', '--yes', '--select=', "--remove-shared=$Shared")
Invoke-Setup @('check')

Write-Step 'Retained resources'
$Plan = Join-Path $Work 'plan.txt'
$PlanText = Invoke-Setup @('plan')
[IO.File]::WriteAllLines($Plan, [string[]]@($PlanText))
Write-Output $PlanText
Invoke-Native $Helper @('retained', '--expected', $Retained, '--plan', $Plan)

Write-Step 'Home folder against its baseline'
Invoke-Native $Helper @('compare', '--root', $HomeFolder, '--exclude', $Skip, '--baseline', $Baseline, '--remains', $Remains)

Write-Output ''
Write-Output "e2e: $Name passed"
