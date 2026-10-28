@echo off
rem Para o Termografo (servidor, simulador e WhatsApp simulado).
taskkill /im termografo.exe /f >nul 2>nul
if errorlevel 1 ( echo Nada rodando. ) else ( echo Termografo parado. )
timeout /t 2 /nobreak >nul
