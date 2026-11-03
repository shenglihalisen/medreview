#!/usr/bin/env bash
# medreview Linux / macOS 启动器：问素材目录、口令、端口、超管数量，然后启动。
# 超管密码由程序自己的 -setup 向导询问（不回显），日常启动不需要任何认证参数。
set -euo pipefail
cd "$(dirname "$0")"

BIN=""
for f in medreview medreview-linux-amd64 medreview-darwin-arm64 medreview-darwin-amd64; do
  [ -x "./$f" ] && BIN="./$f" && break
done
if [ -z "$BIN" ]; then
  echo "找不到可执行的 medreview（chmod +x 后再试）" >&2
  exit 1
fi

printf '素材目录 / media root（回车=当前目录）: '
read -r ROOT
[ -z "$ROOT" ] && ROOT="$PWD"

printf '下载口令 / download token（回车=每次随机）: '
read -r TOKEN

printf '端口 / port（回车=8080）: '
read -r PORT
[ -z "$PORT" ] && PORT=8080

printf '超管数量 / admin accounts（0=不启用登录，回车=0）: '
read -r ADMINS
[ -z "$ADMINS" ] && ADMINS=0

if [ "$ADMINS" != "0" ]; then
  export MEDREVIEW_ADMINS="$ADMINS"
  "$BIN" -setup -db "$PWD/medreview.db"
fi

ARGS=(-root "$ROOT" -addr ":$PORT" -db "$PWD/medreview.db" -cache "$PWD/cache" -log "$PWD/medreview.log")
[ -n "$TOKEN" ] && ARGS+=(-token "$TOKEN")

exec "$BIN" "${ARGS[@]}"
