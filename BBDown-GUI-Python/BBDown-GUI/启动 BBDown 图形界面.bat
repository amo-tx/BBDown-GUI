@echo off
chcp 65001 >nul
setlocal enabledelayedexpansion
title BBDown GUI

set "HERE=%~dp0"
set "PY="

rem --- locate a Python 3 interpreter -------------------------------------
rem 优先使用带 pywebview 的虚拟环境（独立窗口需要），否则退回系统 Python。
rem 路径基于 %USERPROFILE%，换机器/换用户名后无需修改。
if exist "%USERPROFILE%\.workbuddy\binaries\python\envs\default\Scripts\python.exe" (
    set "PY=%USERPROFILE%\.workbuddy\binaries\python\envs\default\Scripts\python.exe"
)
if not defined PY if exist "%HERE%.venv\Scripts\python.exe" (
    set "PY=%HERE%.venv\Scripts\python.exe"
)
if not defined PY if exist "%USERPROFILE%\.workbuddy\binaries\python\versions\3.13.12\python.exe" (
    set "PY=%USERPROFILE%\.workbuddy\binaries\python\versions\3.13.12\python.exe"
)
if not defined PY if exist "%LOCALAPPDATA%\Programs\Python\Python313\python.exe" (
    set "PY=%LOCALAPPDATA%\Programs\Python\Python313\python.exe"
)
if not defined PY for /f "delims=" %%I in ('where python 2^>nul') do (
    if not defined PY set "PY=%%I"
)

if not defined PY (
    echo.
    echo   [ERROR] Python 3 not found on this machine.
    echo   Please install Python 3.8+ and make sure it is in PATH.
    echo.
    pause
    exit /b 1
)

echo.
echo   Starting BBDown GUI ...
echo   Python : %PY%
echo.

"%PY%" "%HERE%server.py" %*

echo.
echo   Server stopped.
pause
