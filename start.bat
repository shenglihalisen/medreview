@echo off
setlocal enabledelayedexpansion
title 素材审阅系统
rem 切换到本文件所在目录：数据库、缓存、日志均保存在这里
cd /d "%~dp0"

rem ====== 把文件夹拖到本文件图标上启动：路径作为参数 %1 传进来，直接开工，不再询问 ======
echo   [3] 监听端口：默认 8080；
echo       若提示端口被占用，请更换其他端口（例如 8081）。
echo   [4] QC 显卡加速：质量检测的解码是否用显卡（独显/核显）；
echo       直接回车 = 是（本机显卡不支持时自动用 CPU）。
echo ============================================================
echo.

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
echo   [4] QC 显卡加速：质量检测的解码是否用显卡（独显/核显）；
echo       直接回车 = 是（本机显卡不支持时自动用 CPU）。
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

set /p "GPUIN=GPU speedup: transcode encoder (nvenc/qsv/amf) + QC decode? (Y=yes/n=no, Enter=yes): "
if /i "!GPUIN!"=="n" set "GPUQC=n"
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

rem 引号一律用 !Q! 变量代替：直接写 set "EXTRA=... "!ROOT!"" 这种嵌套引号会被 cmd
rem 解析错位（实测素材目录填 D: 时整行被拼成 -root D:" -token xxx，启动直接失败）。
set "Q=""
rem 端口必须是纯数字：把 0-9 全删掉，还有残留就说明混进了别的字符
rem （实测误输入过 = ，程序会报 lookup tcp/=: unknown port 看不懂）。
rem 纯变量替换实现，不用管道（管道会另起一个 cmd，延迟扩展会失效）。
set "PNUM="
set /a PNUM=!PORT! 2>nul
if not "!PORT!"=="" if not "!PNUM!"=="!PORT!" set "PORT="
set "EXTRA="
if not "!ROOT!"==""   set "EXTRA=!EXTRA! -root !Q!!ROOT!!Q!"
if not "!TOKEN!"==""  set "EXTRA=!EXTRA! -token !Q!!TOKEN!!Q!"
if not "!PORT!"==""   set "EXTRA=!EXTRA! -addr !Q!:!PORT!!Q!"

echo 正在启动服务，启动完成后浏览器将自动打开审阅页...
medreview.exe -vres 720 -ires 1600 -open -gpuqc !GPUQC!!EXTRA!

echo.
echo 服务已停止。按任意键关闭本窗口。
pause >nul
