param([string]$Database = 'megalove_rehearsal')
$ErrorActionPreference = 'Stop'
# The snapshot cannot be newer than the start of pg_dump.
$started = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
$archive = $null
for ($attempt = 1; $attempt -le 3; $attempt++) {
    try {
        $archive = & (Join-Path $PSScriptRoot 'backup.ps1') -Database $Database
        break
    } catch {
        if ($attempt -eq 3) { throw }
        Start-Sleep -Seconds 2
    }
}
if (!(Test-Path -LiteralPath $archive -PathType Leaf)) { throw 'Отсутствует файл резервной копии.' }
if ($Database -notin 'megalove','megalove_demo','megalove_rehearsal') { throw 'Неизвестная база.' }
$sql = "INSERT INTO backup_receipts VALUES (true,to_timestamp($started)) ON CONFLICT(singleton) DO UPDATE SET snapshot_at=greatest(backup_receipts.snapshot_at,excluded.snapshot_at);"
for ($attempt = 1; $attempt -le 3; $attempt++) {
    $sql | ssh.exe -o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=10 selectel-megabot "runuser -u megalove -- psql -Xq -v ON_ERROR_STOP=1 -d $Database"
    if ($LASTEXITCODE -eq 0) { return }
    if ($attempt -lt 3) { Start-Sleep -Seconds 2 }
}
throw 'Копия сохранена. Сервер не подтвердил отметку времени.'
