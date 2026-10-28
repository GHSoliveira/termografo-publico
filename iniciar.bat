@echo off
rem Termografo - modo real: so o servidor, consultando o Zabbix real pela ponte (sessao do navegador, sem token).
rem A ponte roda na aba do Zabbix pelo Tampermonkey (ponte-zabbix.user.js). Para parar: parar.bat.
setlocal
cd /d "%~dp0"
title Termografo

rem 0) precisa estar na pasta do projeto (ex.: rodou de dentro do ZIP sem extrair, ou copiou so este arquivo)
if not exist "%~dp0main.go" (
  if exist "%USERPROFILE%\termografo\main.go" (
    echo Usando a instalacao em %USERPROFILE%\termografo
    cd /d "%USERPROFILE%\termografo"
  ) else (
    echo.
    echo  Este iniciar.bat esta fora da pasta do projeto.
    echo  - Se baixou o ZIP: clique com o botao direito no ZIP, "Extrair tudo", e rode o iniciar.bat da pasta extraida.
    echo  - Ou rode o instalar.bat: ele baixa o projeto em %USERPROFILE%\termografo e ja inicia.
    echo.
    pause
    exit /b 1
  )
)
rem endereco do seu Zabbix: fica em zabbix-url.txt (uma linha, ex.: https://zabbix.suaempresa.com.br)
set "ZABBIX="
if exist zabbix-url.txt set /p ZABBIX=<zabbix-url.txt
if not defined ZABBIX (
  echo.
  echo  Falta o endereco do Zabbix. Crie o arquivo zabbix-url.txt nesta pasta com uma linha,
  echo  por exemplo: https://zabbix.suaempresa.com.br
  echo  e troque o @match do ponte-zabbix.user.js pelo mesmo endereco.
  echo.
  pause
  exit /b 1
)

rem 1) executavel: usa o da pasta; se nao tiver, baixa da release; se nao der, compila (Go)
if not exist termografo.exe if exist baixar-exe.ps1 (
  echo Baixando o termografo.exe da release...
  powershell -NoProfile -ExecutionPolicy Bypass -File baixar-exe.ps1
)
if not exist termografo.exe (
  where go >nul 2>nul
  if errorlevel 1 (
    echo.
    echo  Nao achei o termografo.exe nesta pasta.
    echo  Baixe em https://github.com/GHSoliveira/termografo-publico/releases/latest e coloque aqui,
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

rem 3) se ja estiver rodando, para antes
taskkill /im termografo.exe /f >nul 2>nul

rem 4) servidor: sem dados de exemplo, ponte autorizada so para o Zabbix real,
rem    dados reais na pasta dados\ (separados dos arquivos da simulacao)
if not exist dados mkdir dados
start "Termografo - servidor (8080)" termografo.exe -sem-exemplo -ponte-zabbix %ZABBIX% -alarmes-severidade 5 -dados dados\os-recebidas.jsonl -dados-rede dados\eventos-rede.jsonl
timeout /t 2 /nobreak >nul

rem 5) abre o Zabbix (o Tampermonkey liga a ponte nele) e o Termografo
start "" "%ZABBIX%/"
start "" "http://localhost:8080"

echo.
echo  Termografo rodando em http://localhost:8080
echo.
echo  A consulta ao Zabbix depende da ponte, que roda na aba do Zabbix:
echo    - instale o Tampermonkey no navegador e o script ponte-zabbix.user.js (desta pasta);
echo    - faca login no Zabbix e deixe a aba aberta: no canto aparece "Ponte com o Termografo - conectada".
echo  Alarmes abertos (so Desastre): buscados a cada 5 min, ou em "Buscar agora" na aba Rede.
echo.
echo  Para parar: parar.bat
echo.
pause
