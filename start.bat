@echo off
setlocal enabledelayedexpansion
title medreview 素材审阅系统
rem 切换到 bat 所在目录：数据库、缓存、日志均保存在本目录下
cd /d "%~dp0"

echo ============================================================
echo                 medreview 素材审阅系统
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

set "ROOT="
set /p "ROOT=请输入素材目录: "
set "ROOT=!ROOT:"=!"
echo.

set "TOKEN="
set /p "TOKEN=请设置下载口令: "
set "TOKEN=!TOKEN:"=!"
echo.

set "PORT="
set /p "PORT=请输入监听端口: "
set "PORT=!PORT:"=!"
echo.

set "EXTRA="
if not "!ROOT!"==""   set "EXTRA=!EXTRA! -root "!ROOT!""
if not "!TOKEN!"==""  set "EXTRA=!EXTRA! -token "!TOKEN!""
if not "!PORT!"==""   set "EXTRA=!EXTRA! -addr :!PORT!"

echo 正在启动服务，启动完成后浏览器将自动打开审阅页...
medreview.exe -vres 720 -ires 1600 -open!EXTRA!

echo.
echo 服务已停止。按任意键关闭本窗口。
pause >nul
