@echo off
rem SPDX-License-Identifier: Apache-2.0
rem Launcher for the Nurtuo JVM bridge (Windows). See scripts\nurtuo-bridge for the options.
rem stdout carries only the NURTUO-BRIDGE handshake line. On Windows use
rem --listen tcp:127.0.0.1:0 with NURTUO_BRIDGE_TOKEN, and --exit-on-stdin-eof so the bridge
rem exits when the runtime (its parent) closes the pipe or dies.
setlocal
set "APP_HOME=%~dp0.."

set "JAVA=java"
if defined JAVA_HOME if exist "%JAVA_HOME%\bin\java.exe" set "JAVA=%JAVA_HOME%\bin\java.exe"
if exist "%APP_HOME%\jre\bin\java.exe" set "JAVA=%APP_HOME%\jre\bin\java.exe"
if defined NURTUO_BRIDGE_JAVA set "JAVA=%NURTUO_BRIDGE_JAVA%"

set "OPTS=%NURTUO_BRIDGE_JAVA_OPTS%"
if not defined OPTS set "OPTS=-Xmx512m"

set PLUGINS=
if exist "%APP_HOME%\plugins\" set PLUGINS=--plugin-dir "%APP_HOME%\plugins"

"%JAVA%" %OPTS% -XX:+ExitOnOutOfMemoryError -jar "%APP_HOME%\lib\nurtuo-bridge.jar" %PLUGINS% %*
exit /b %ERRORLEVEL%
