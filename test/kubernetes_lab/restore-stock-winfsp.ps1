# Explicit lab-only MSI transition. Invoke through native Labcontainers serial
# control after TestRetainedWindowsMountUpgrade quiesce. Never reboots itself.
[CmdletBinding()]
param(
    [Parameter(Mandatory)][ValidatePattern('^\{[0-9A-Fa-f-]{36}\}$')][string]$LabProductCode,
    [Parameter(Mandatory)][ValidatePattern('^[0-9a-f]{64}$')][string]$LabDriverSHA256,
    [Parameter(Mandatory)][string]$StockMSI
)
$ErrorActionPreference = 'Stop'
if ([Security.Principal.WindowsIdentity]::GetCurrent().User.Value -ne 'S-1-5-18') {
    throw 'requires isolated Windows lab SYSTEM token'
}
if (!(Test-Path 'C:\LabQualification\k0s.exe')) { throw 'isolated lab prerequisite missing' }
if ((Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control').PSObject.Properties['ContainerType']) {
    throw 'requires native VM, not a container'
}
if (@(Get-Process weed,seaweedfs-mount -ErrorAction SilentlyContinue).Count) {
    throw 'quiesce all mount processes first'
}
$product = Get-ItemProperty -LiteralPath ('HKLM:\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\' + $LabProductCode)
if ($product.DisplayName -cne 'WinFsp AppMana LAB ONLY') { throw 'refuses to uninstall a non-lab product' }
$root = (Get-ItemProperty 'HKLM:\SOFTWARE\WOW6432Node\WinFsp').InstallDir
if ((Get-FileHash (Join-Path $root 'bin\winfsp-x64.sys')).Hash -ine $LabDriverSHA256) {
    throw 'installed lab driver does not match explicit pin'
}
if ((Get-FileHash -LiteralPath $StockMSI).Hash -ine '073a70e00f77423e34bed98b86e600def93393ba5822204fac57a29324db9f7a') {
    throw 'requires pinned official MSI'
}
New-Item -ItemType Directory -Force C:\LabQualification\stock-transition | Out-Null
$uninstall = Start-Process msiexec.exe -Wait -PassThru -ArgumentList '/x',$LabProductCode,'/qn','/norestart','/l*v','C:\LabQualification\stock-transition\uninstall.log'
if ($uninstall.ExitCode -notin @(0,3010)) { throw "lab MSI uninstall failed: $($uninstall.ExitCode)" }
$install = Start-Process msiexec.exe -Wait -PassThru -ArgumentList '/i',('"'+$StockMSI+'"'),'/qn','/norestart','INSTALLLEVEL=1000','/l*v','C:\LabQualification\stock-transition\install.log'
if ($install.ExitCode -notin @(0,3010)) { throw "official MSI install failed: $($install.ExitCode)" }
$root = (Get-ItemProperty 'HKLM:\SOFTWARE\WOW6432Node\WinFsp').InstallDir
$driver = Join-Path $root 'bin\winfsp-x64.sys'
if ((Get-FileHash $driver).Hash -ine '03553fffacd362f4a9a08c00b4f236a82354a183bc6028494fb32055386e13c9') { throw 'installed stock SYS mismatch' }
$signature = Get-AuthenticodeSignature $driver
if ($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Subject -notmatch 'CN=Microsoft Windows Hardware Compatibility Publisher(,|$)') { throw 'invalid stock driver signature' }
& bcdedit.exe /set testsigning off
if ($LASTEXITCODE -ne 0) { throw 'failed to disable lab test signing' }
& bcdedit.exe /set nointegritychecks off
if ($LASTEXITCODE -ne 0) { throw 'failed to enforce integrity checks' }
Write-Output 'STOCK_MSI_RESTORED_REBOOT_REQUIRED'
