@echo off
rem Termografo - SIMULACAO (so para teste): servidor + simulador de O.S. e de quedas + Zabbix e WhatsApp de mentira.
rem Para usar de verdade, rode o iniciar.bat.
rem Cada parte abre numa janela propria; para parar tudo, rode parar.bat.
setlocal
cd /d "%~dp0"
title Termografo - simulacao

rem 1) executavel: usa o termografo.exe da pasta; se nao tiver, baixa da release; se nao der, compila (Go)
if not exist termografo.exe if exist baixar-exe.ps1 (
  echo Baixando o termografo.exe da release...
  powershell -NoProfile -ExecutionPolicy Bypass -File baixar-exe.ps1
)
if not exist termografo.exe (
  where go >nul 2>nul
  if errorlevel 1 (
    echo.
    echo  Nao achei o termografo.exe nesta pasta.
    echo  Baixe na pagina de releases do repositorio e coloque aqui:
    echo    https://github.com/GHSoliveira/termografo-publico/releases/latest
    echo  ou instale o Go ^(https://go.dev/dl^) para compilar automaticamente.
    echo.
    pause
    exit /b 1
  )
  echo Compilando o termografo.exe...
  go build -ldflags "-s -w" -o termografo.exe .
  if errorlevel 1 ( echo Falha ao compilar. & pause & exit /b 1 )
)

rem 2) token dos webhooks: gera um aleatorio na primeira vez (fica em token-webhook.txt)
if not exist token-webhook.txt (
  powershell -NoProfile -Command "$b=New-Object byte[] 24; [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b); [IO.File]::WriteAllText('token-webhook.txt', (-join ($b | ForEach-Object { '{0:x2}' -f $_ })).Substring(0,32))"
)
set /p WEBHOOK_TOKEN=<token-webhook.txt

rem 3) se algo ja estiver rodando, para antes
taskkill /im termografo.exe /f >nul 2>nul

rem 4) sobe as tres partes
start "Termografo - WhatsApp simulado (8082)" termografo.exe whatsapp
start "Termografo - servidor (8080)" termografo.exe -ponte-zabbix http://127.0.0.1:8081 -monitoramento http://127.0.0.1:8081 -whatsapp http://127.0.0.1:8082
timeout /t 3 /nobreak >nul
start "Termografo - simulador e Zabbix simulado (8081)" termografo.exe simular -alarmes ponte
timeout /t 3 /nobreak >nul

rem 5) abre no navegador: a aba do Zabbix simulado ativa a ponte (deixe ela aberta)
start "" "http://127.0.0.1:8081/#ponte"
start "" "http://localhost:8082"
start "" "http://localhost:8080"

echo.
echo  Termografo rodando:
echo    Termografo ............ http://localhost:8080   (qualquer codigo de entrada)
echo    WhatsApp simulado ..... http://localhost:8082
echo    Zabbix simulado ....... http://127.0.0.1:8081   (deixe esta aba aberta: e nela que roda a ponte)
echo.
echo  Para parar tudo: parar.bat
echo.
pause
