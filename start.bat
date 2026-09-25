@echo off
setlocal enabledelayedexpansion
title 素材审阅系统
rem 切换到本文件所在目录：数据库、缓存、日志均保存在这里
cd /d "%~dp0"

rem ====== 把文件夹拖到本文件图标上启动：路径作为参数 %1 传进来，直接开工，不再询问 ======
set "ROOT="
set "TOKEN="
set "PORT="
if not "%~1"=="" (
  set "ROOT=%~1"
  goto :check
)

echo ============================================================
echo                   素材审阅系统
echo ------------------------------------------------------------
echo   [1] 素材目录：将文件夹拖入本窗口后按回车，
echo       也可以直接粘贴完整路径后按回车；
echo       直接按回车，则继续使用上一次的素材目录。
echo.
echo   [2] 下载口令：在下载页打开 / 打包时需要输入的口令；
echo       直接按回车，则每次启动自动生成随机口令。
echo.
echo   [3] 监听端口：默认 8080；
echo       若提示端口被占用，请更换其他端口（例如 8081）。
echo ============================================================
echo.

set /p "ROOT=请输入素材目录: "
set "ROOT=!ROOT:"=!"
echo.

set /p "TOKEN=请设置下载口令: "
set "TOKEN=!TOKEN:"=!"
echo.

set /p "PORT=请输入监听端口: "
set "PORT=!PORT:"=!"
echo.

:check
rem 目录要么留空（用上次的），要么必须真实存在且是文件夹（拖快捷方式/文件不行）
if not "!ROOT!"=="" if not exist "!ROOT!" (
  echo.
  echo [错误] 素材目录不存在或不是文件夹：!ROOT!
  echo 请把文件夹本身拖到本文件图标上，或检查路径后重新运行。
  pause
  exit /b 1
)

set "EXTRA="
if not "!ROOT!"==""   set "EXTRA=!EXTRA! -root "!ROOT!""
if not "!TOKEN!"==""  set "EXTRA=!EXTRA! -token "!TOKEN!""
if not "!PORT!"==""   set "EXTRA=!EXTRA! -addr :!PORT!"

echo 正在启动服务，启动完成后浏览器将自动打开审阅页...
medreview.exe -vres 720 -ires 1600 -open!EXTRA!

echo.
echo 服务已停止。按任意键关闭本窗口。
pause >nul
