@echo off
rem Swap in lark-gateway.new.exe: check it, stop the service, keep the old exe, start.
cd /d "%~dp0"
for /f %%d in ('powershell -NoProfile -Command "Get-Date -Format yyyyMMdd-HHmm"') do set STAMP=%%d
lark-gateway.new.exe -check >nul || (echo new exe failed -check & exit /b 1)
nssm stop LarkGateway
move /y lark-gateway.exe lark-gateway.exe.bak-%STAMP% || (nssm start LarkGateway & exit /b 1)
move /y lark-gateway.new.exe lark-gateway.exe
nssm start LarkGateway
