# Lab-only preparation for TestCSICandidateWinFsp. Run as SYSTEM only after
# CSI workload/mount processes have been stopped. This script never reboots.
[CmdletBinding()]
param(
    [Parameter(Mandatory)][ValidatePattern('^[A-Za-z0-9-]+$')][string]$Token,
    [Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{64}$')][string]$ManifestSHA256,
    [Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{64}$')][string]$InstallerSHA256,
    [Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{64}$')][string]$StockMSISHA256,
    [Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{64}$')][string]$NativeTestSHA256
)
$ErrorActionPreference = 'Stop'

function Assert-Hash([string]$Path, [string]$Expected, [string]$Description) {
    if (!(Test-Path -LiteralPath $Path -PathType Leaf)) { throw "$Description missing: $Path" }
    if ((Get-FileHash -Algorithm SHA256 -LiteralPath $Path).Hash -ine $Expected) {
        throw "$Description digest mismatch: $Path"
    }
}

function Assert-CandidateManifest($Manifest) {
    if ($Manifest.lab_only -ne $true) { throw 'candidate manifest is not lab-only' }
    foreach ($name in @('source_revision', 'driver_source_revision')) {
        if ([string]$Manifest.$name -cnotmatch '^[0-9a-f]{40}$') { throw "invalid $name" }
    }
    foreach ($name in @('source_archive_sha256', 'driver_source_archive_sha256', 'driver_sha256', 'dll_sha256')) {
        if ([string]$Manifest.$name -cnotmatch '^[0-9a-f]{64}$') { throw "invalid $name" }
    }
    if ([string]$Manifest.certificate_thumbprint -cnotmatch '^[0-9A-Fa-f]{40}$') {
        throw 'invalid certificate_thumbprint'
    }
}

function Assert-CandidateBootPolicy($BootLines) {
    $testSigning = $false
    foreach ($line in @($BootLines)) {
        if ($line -match '^\s*testsigning\s+Yes\s*$') { $testSigning = $true }
        if ($line -match '^\s*nointegritychecks\s+Yes\s*$') { throw 'nointegritychecks must remain disabled' }
    }
    if (!$testSigning) { throw 'testsigning was not enabled by the lab installer' }
}

function Assert-CleanStockInstall([bool]$RegistryExists, [string]$InstallDir, [string]$SignatureStatus) {
    if (!$RegistryExists) { return 'absent' }
    if (!$InstallDir -or $InstallDir.StartsWith('C:\WinFspCandidates\', [StringComparison]::OrdinalIgnoreCase)) {
        throw 'existing WinFsp registry state is not a clean stock InstallDir'
    }
    if ($SignatureStatus -ne 'Valid') { throw 'existing stock WinFsp DLL is not validly signed' }
    return 'present'
}

if ([Security.Principal.WindowsIdentity]::GetCurrent().User.Value -ne 'S-1-5-18') {
    throw 'candidate preparation requires the isolated Windows lab SYSTEM token'
}
if ((Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control').PSObject.Properties['ContainerType']) {
    throw 'candidate preparation requires a native Windows VM'
}
if (@(Get-Process weed -ErrorAction SilentlyContinue).Count) {
    throw 'stop all SeaweedFS mount processes before candidate preparation'
}
$running = @(Get-CimInstance Win32_SystemDriver | Where-Object { $_.Name -like 'WinFsp*' -and $_.State -eq 'Running' })
if ($running.Count) { throw 'stop the active WinFsp driver before candidate preparation' }

$output = 'C:\lab\output'
$manifestPath = Join-Path $output 'manifest.json'
$installer = 'C:\lab\install.ps1'
$stockMSI = 'C:\lab\winfsp.msi'
$nativeInput = 'C:\lab\winfsp-csi-candidate.test.exe'
$sourceArchive = 'C:\lab\source.zip'
Assert-Hash $manifestPath $ManifestSHA256 'candidate manifest'
Assert-Hash $installer $InstallerSHA256 'committed WinFsp lab installer'
Assert-Hash $stockMSI $StockMSISHA256 'stock WinFsp MSI'
Assert-Hash $nativeInput $NativeTestSHA256 'candidate native test executable'

$manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
Assert-CandidateManifest $manifest
Assert-Hash $sourceArchive $manifest.source_archive_sha256 'candidate source archive'
if ($manifest.driver_source_revision -eq $manifest.source_revision) {
    if ($manifest.driver_source_archive_sha256 -ine $manifest.source_archive_sha256) {
        throw 'same-revision driver source archive digest differs'
    }
} else {
    Assert-Hash 'C:\lab\driver-source.zip' $manifest.driver_source_archive_sha256 'candidate driver source archive'
}
$candidateDriver = Join-Path $output 'winfsp-x64.sys'
$candidateDll = Join-Path $output 'winfsp-x64.dll'
$candidateCertificate = Join-Path $output 'lab.cer'
Assert-Hash $candidateDriver $manifest.driver_sha256 'candidate driver'
Assert-Hash $candidateDll $manifest.dll_sha256 'candidate DLL'

$certificate = New-Object Security.Cryptography.X509Certificates.X509Certificate2($candidateCertificate)
if ($certificate.Thumbprint -ine $manifest.certificate_thumbprint) { throw 'lab certificate digest does not match manifest' }
if ($certificate.Subject -ine 'CN=AppMana WinFsp LAB ONLY') { throw "unexpected lab certificate subject: $($certificate.Subject)" }
$driverSignature = Get-AuthenticodeSignature -LiteralPath $candidateDriver
if (!$driverSignature.SignerCertificate -or $driverSignature.SignerCertificate.Thumbprint -ine $manifest.certificate_thumbprint) {
    throw 'candidate driver signer does not match manifest'
}

$registryPath = 'HKLM:\SOFTWARE\WOW6432Node\WinFsp'
$previousInstallDir = $null
$registryExists = Test-Path $registryPath
$stockSignatureStatus = $null
if ($registryExists) {
    $previousInstallDir = (Get-ItemProperty -LiteralPath $registryPath -Name InstallDir -ErrorAction SilentlyContinue).InstallDir
    if ($previousInstallDir) {
        $stockSignatureStatus = (Get-AuthenticodeSignature -LiteralPath (Join-Path $previousInstallDir 'bin\winfsp-x64.dll')).Status
    }
}
$stockState = Assert-CleanStockInstall $registryExists $previousInstallDir $stockSignatureStatus
if ($stockState -eq 'absent') {
    $staleServices = @(Get-CimInstance Win32_SystemDriver | Where-Object { $_.Name -like 'WinFsp*' })
    if ($staleServices.Count) { throw 'WinFsp service exists without its stock installation registry state' }
}

# Reuse the fork's installer for certificate trust, test-signing policy, stock
# tools and driver registration. On a fresh VM this installs the exact
# hash-pinned stock MSI first; on an existing clean stock VM it repairs that
# same MSI. Its exact bytes were checked above.
& $installer -Token $Token
if ($LASTEXITCODE -ne 0) { throw "candidate installer failed: $LASTEXITCODE" }

# The stock MSI owns this directory and is the rollback anchor. Require it
# after either fresh install or repair before selecting the isolated DLL.
$installedStockRoot = (Get-ItemProperty -LiteralPath $registryPath -Name InstallDir -ErrorAction Stop).InstallDir
$installedStockDLL = Join-Path $installedStockRoot 'bin\winfsp-x64.dll'
$installedStockSignature = Get-AuthenticodeSignature -LiteralPath $installedStockDLL
if ((Assert-CleanStockInstall $true $installedStockRoot $installedStockSignature.Status) -ne 'present') {
    throw 'stock WinFsp bootstrap did not complete'
}
if (!$previousInstallDir) { $previousInstallDir = $installedStockRoot }

# Do not replace the MSI-owned DLL. Select an isolated InstallDir that satisfies
# WinFsp/cgofuse's documented <InstallDir>\bin\winfsp-x64.dll lookup.
$candidateRoot = "C:\WinFspCandidates\$($manifest.driver_source_revision)"
$candidateBin = Join-Path $candidateRoot 'bin'
New-Item -ItemType Directory -Force -Path $candidateBin | Out-Null
$selectedDll = Join-Path $candidateBin 'winfsp-x64.dll'
if (Test-Path -LiteralPath $selectedDll) {
    Assert-Hash $selectedDll $manifest.dll_sha256 'existing selected candidate DLL'
} else {
    Copy-Item -LiteralPath $candidateDll -Destination $selectedDll
}
Assert-Hash $selectedDll $manifest.dll_sha256 'selected candidate DLL'
Set-ItemProperty -LiteralPath $registryPath -Name InstallDir -Value ($candidateRoot + '\')

# Preserve the image's stock native executable; the candidate lane has its own
# immutable filename and clients receive C:\LabInputs read-only as C:\tools.
New-Item -ItemType Directory -Force -Path C:\LabInputs | Out-Null
$selectedNative = 'C:\LabInputs\winfsp-csi-candidate.test.exe'
if (Test-Path -LiteralPath $selectedNative) {
    Assert-Hash $selectedNative $NativeTestSHA256 'existing candidate native test executable'
} else {
    Copy-Item -LiteralPath $nativeInput -Destination $selectedNative
}
Assert-Hash $selectedNative $NativeTestSHA256 'selected candidate native test executable'

$boot = & bcdedit.exe /enum '{current}'
if ($LASTEXITCODE -ne 0) { throw 'cannot attest candidate boot policy' }
Assert-CandidateBootPolicy $boot

[ordered]@{
    token = $Token
    lab_only = $true
    manifest_sha256 = $ManifestSHA256.ToLowerInvariant()
    driver_source_revision = $manifest.driver_source_revision
    driver_source_archive_sha256 = $manifest.driver_source_archive_sha256
    driver_sha256 = $manifest.driver_sha256
    dll_sha256 = $manifest.dll_sha256
    certificate_thumbprint = $manifest.certificate_thumbprint
    candidate_install_dir = $candidateRoot + '\'
    previous_install_dir = $previousInstallDir
    native_test_sha256 = $NativeTestSHA256.ToLowerInvariant()
    reboot_required = $true
} | ConvertTo-Json | Set-Content -LiteralPath C:\lab\csi-candidate-preparation.json -Encoding UTF8

Write-Output "CSI_CANDIDATE_PREPARED_REBOOT_REQUIRED:$Token"
