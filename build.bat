@ECHO OFF
REM builds deej-dev.exe and deej-release.exe into the repo root
CALL "%~dp0pkg\deej\scripts\windows\build-all.bat"
IF "%~1"=="" PAUSE
