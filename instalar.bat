@echo off
rem Termografo - instala (clona do GitHub) ou atualiza, baixa o executavel e inicia tudo.
rem Pode ficar em qualquer pasta. O projeto vai para %USERPROFILE%\termografo.
rem   instalar.bat               clona/atualiza e inicia
rem   instalar.bat --sem-iniciar so clona/atualiza e baixa o executavel
setlocal
title Termografo - instalar
set "REPO=GHSoliveira/termografo-publico"
if not defined DESTINO set "DESTINO=%USERPROFILE%\termografo"

rem 1) Git (precisa para clonar)
where git >nul 2>nul
if errorlevel 1 (
  echo.
  echo  O Git nao esta instalado.
  choice /c SN /m " Instalar o Git agora pelo winget"
  if errorlevel 2 ( echo  Instale em https://git-scm.com/download/win e rode de novo. & pause & exit /b 1 )
  winget install --id Git.Git -e --source winget --accept-package-agreements --accept-source-agreements
  if errorlevel 1 ( echo  Falha ao instalar o Git. & pause & exit /b 1 )
  set "PATH=%PATH%;%ProgramFiles%\Git\cmd"
)

rem 2) clona ou atualiza
if exist "%DESTINO%\.git" (
  echo Atualizando %DESTINO% ...
  git -C "%DESTINO%" pull --ff-only
) else (
  echo Clonando %REPO% em %DESTINO% ...
  git clone "https://github.com/%REPO%.git" "%DESTINO%"
)
if errorlevel 1 (
  echo.
  echo  Nao consegui clonar/atualizar. Confira a conexao com a internet.
  pause
  exit /b 1
)

rem 3) executavel da release (ou o iniciar.bat compila, se tiver Go)
if not exist "%DESTINO%\termografo.exe" (
  echo Baixando o termografo.exe ...
  powershell -NoProfile -ExecutionPolicy Bypass -File "%DESTINO%\baixar-exe.ps1"
)

if /i "%~1"=="--sem-iniciar" (
  echo Pronto. Para iniciar: "%DESTINO%\iniciar.bat"
  exit /b 0
)

rem 4) inicia tudo
call "%DESTINO%\iniciar.bat"
