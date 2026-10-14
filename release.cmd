@echo off
rem Build the single-file client release (same as: sh release.sh).
rem Requires Go and Git Bash's sh on PATH (both ship with Git for Windows).
where sh >nul 2>nul
if errorlevel 1 (
    echo Git Bash "sh" is required - install Git for Windows, or run the script from Git Bash. >&2
    exit /b 1
)
sh "%~dp0release.sh" %*
