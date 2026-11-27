param(
    [ValidateSet('megalove_demo', 'megalove_rehearsal', 'megalove')]
    [string]$Database = 'megalove_demo',
    [string]$Archive,
    [ValidatePattern('^megalove_restore_[a-z0-9_]{1,36}$')]
    [string]$RestoreTo
)

$ErrorActionPreference = 'Stop'
$vault = Join-Path $env:LOCALAPPDATA 'MegaLove'
$scope = [System.Security.Cryptography.DataProtectionScope]::CurrentUser
$ssh = [System.Diagnostics.ProcessStartInfo]::new('ssh.exe')
$ssh.UseShellExecute = $false
foreach ($arg in @('-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', 'ConnectTimeout=8', 'selectel-megabot')) {
    $ssh.ArgumentList.Add($arg)
}

if ($RestoreTo) {
    if (-not $Archive) { throw 'Укажи архив через -Archive.' }
    $plain = [System.Security.Cryptography.ProtectedData]::Unprotect(
        [System.IO.File]::ReadAllBytes($Archive), $null, $scope)
    $ssh.RedirectStandardInput = $true
    $ssh.ArgumentList.Add("set -eu; runuser -u postgres -- createdb --owner=megalove $RestoreTo; exec runuser -u megalove -- pg_restore --exit-on-error --single-transaction --no-owner --no-acl --dbname=$RestoreTo")
    $process = [System.Diagnostics.Process]::Start($ssh)
    try {
        $process.StandardInput.BaseStream.Write($plain, 0, $plain.Length)
        $process.StandardInput.Close()
        $process.WaitForExit()
        if ($process.ExitCode -ne 0) { throw 'Восстановление PostgreSQL завершилось ошибкой.' }
        Write-Output "Восстановлено в $RestoreTo"
    } finally {
        [Array]::Clear($plain, 0, $plain.Length)
        $process.Dispose()
    }
    return
}

if (-not $Archive) {
    New-Item -ItemType Directory -Force -Path $vault | Out-Null
    $Archive = Join-Path $vault "$Database-$(Get-Date -Format 'yyyyMMdd-HHmmss').dump.dpapi"
}
$ssh.RedirectStandardOutput = $true
$ssh.ArgumentList.Add("exec runuser -u megalove -- pg_dump -Fc --no-owner --no-acl $Database")
$process = [System.Diagnostics.Process]::Start($ssh)
$buffer = [System.IO.MemoryStream]::new()
try {
    $process.StandardOutput.BaseStream.CopyTo($buffer)
    $process.WaitForExit()
    if ($process.ExitCode -ne 0 -or $buffer.Length -eq 0) { throw 'Не удалось получить копию PostgreSQL.' }
    $plain = $buffer.ToArray()
    try {
        $protected = [System.Security.Cryptography.ProtectedData]::Protect($plain, $null, $scope)
    } finally {
        [Array]::Clear($plain, 0, $plain.Length)
    }
    $output = [System.IO.File]::Open($Archive, [System.IO.FileMode]::CreateNew)
    try {
        $output.Write($protected, 0, $protected.Length)
    } finally {
        $output.Dispose()
    }
    Write-Output $Archive
} finally {
    [Array]::Clear($buffer.GetBuffer(), 0, $buffer.Capacity)
    $buffer.Dispose()
    $process.Dispose()
}
