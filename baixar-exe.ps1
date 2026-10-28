# Baixa o termografo.exe da ultima release do GitHub (repositorio publico: nao precisa de login).
$ErrorActionPreference = 'Stop'
$repo = 'GHSoliveira/termografo-publico'
Set-Location $PSScriptRoot
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$cab = @{ 'User-Agent' = 'termografo-instalador' }
$rel = Invoke-RestMethod -Headers $cab -Uri "https://api.github.com/repos/$repo/releases/latest"
$asset = $rel.assets | Where-Object { $_.name -eq 'termografo.exe' } | Select-Object -First 1
if (-not $asset) { Write-Host 'A release nao tem o termografo.exe.'; exit 1 }
Invoke-WebRequest -Headers $cab -Uri $asset.browser_download_url -OutFile termografo.exe -UseBasicParsing
Write-Host ("termografo.exe baixado (release {0})." -f $rel.tag_name)
exit 0
