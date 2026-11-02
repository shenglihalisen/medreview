@echo off
rem medreview daily maintenance: go vet + go test + build + 8 acceptance harnesses
rem Pure ASCII on purpose (GBK codepage safe). Run by double-click or: cmd /c maint.bat
setlocal EnableDelayedExpansion
cd /d "%~dp0"

set "GO=D:\tools\go\go\bin\go.exe"
set "EXE=medreview.exe"
set "LOG=_verify\maint.log"
set "FAILS="

echo ============================================ >  "%LOG%"
echo medreview maintenance >  "%LOG%"
echo started: %DATE% %TIME% >> "%LOG%"
echo ============================================ >> "%LOG%"

rem 0. precheck python + Pillow (the harness needs Pillow to read image EXIF/size)
python -c "import PIL" >nul 2>&1
if errorlevel 1 (
  echo [precheck] Pillow missing, installing from _verify\requirements.txt
  python -m pip install -r _verify\requirements.txt >> "%LOG%" 2>&1
  python -c "import PIL" >nul 2>&1
  if errorlevel 1 (
    echo [precheck] FATAL: Pillow still missing after install. Aborting.
    exit /b 1
  )
)
echo [precheck] python + Pillow OK

rem 1. go vet
echo [1/4] go vet ./...
"%GO%" vet ./... >> "%LOG%" 2>&1
if errorlevel 1 ( echo [1/4] go vet: FAIL & set "FAILS=!FAILS! vet" ) else ( echo [1/4] go vet: PASS )

rem 2. go test (unit + regression + security)
echo [2/4] go test ./...
"%GO%" test ./... >> "%LOG%" 2>&1
if errorlevel 1 ( echo [2/4] go test: FAIL & set "FAILS=!FAILS! test" ) else ( echo [2/4] go test: PASS )

rem 3. build (harness uses the freshly built exe in this dir)
echo [3/4] go build -o %EXE% .
"%GO%" build -o %EXE% . >> "%LOG%" 2>&1
if errorlevel 1 (
  echo [3/4] build: FAIL
  set "FAILS=!FAILS! build"
  goto :summary
) else ( echo [3/4] build: PASS )

rem 4. acceptance harness (8 suites, each self-contained: own material/port/db/cache)
echo [4/4] acceptance harness (8 suites)
set "SUITES=check_mobile_shell check_keep_scope check_image_preview check_download_and_720p check_qc check_startup_clean check_startup_modes check_token_random"
for %%s in (%SUITES%) do (
  echo   running %%s ...
  python -u _verify\%%s.py %EXE% >> "%LOG%" 2>&1
  if errorlevel 1 ( echo   %%s: FAIL & set "FAILS=!FAILS! %%s" ) else ( echo   %%s: PASS )
)

:summary
rem cleanup regenerable harness artifacts (keep this run's maint log)
move /y "_verify\maint.log" "_verify\maint.log.keep" >nul 2>&1
for /d %%d in (_verify\*_trash_*) do rmdir /s /q "%%d"
del /q _verify\*.log >nul 2>&1
move /y "_verify\maint.log.keep" "_verify\maint.log" >nul 2>&1
echo ============================================ >> "%LOG%"
echo SUMMARY (empty FAILS = all pass):!FAILS! >> "%LOG%"
echo ============================================ >> "%LOG%"
if "!FAILS!"=="" ( echo MAINTENANCE OK & exit /b 0 ) else ( echo MAINTENANCE FAILED:!FAILS! & exit /b 1 )
