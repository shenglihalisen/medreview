# medreview

[**简体中文**](#简体中文) | [English](#english)

---

# 简体中文

Windows 本地运行的照片 / 视频人工筛选工具。多人按目录并行标记「保留 / 不保留」，审完一键打包或导出。**源文件全程只读**：程序只读取素材，不会移动、修改或删除任何原文件。

## 目录

- [1. 功能](#1-功能)
- [2. 快速开始](#2-快速开始)
- [3. 架构与源码结构](#3-架构与源码结构)
- [4. 构建](#4-构建)
- [5. 运行参数](#5-运行参数)
- [6. HTTP 接口](#6-http-接口)
- [7. 数据模型](#7-数据模型)
- [8. 验收](#8-验收)
- [9. 安全模型与风险](#9-安全模型与风险)
- [10. 已知限制](#10-已知限制)
- [11. QC 质量检测](#11-qc-质量检测)

## 1. 功能

| 能力 | 说明 |
| --- | --- |
| 目录树浏览 | 按服务端磁盘目录树选择素材目录，支持「含父目录」浏览（父层直属文件一并显示，但标记与打包口径不变） |
| 人工标记 | 单文件标记、按屏幕可见区批量标记、**按目录整体标记**（本目录全部保留 / 全部不保留 / 清空本目录标记） |
| 目录认领 | 按目录加锁：被认领后他人只读，**锁仅作用于本层，不向下递归** |
| 视频转码 | 浏览器无法直接播放的编码（HEVC/H.265 等）由内嵌 ffmpeg 转码为 H.264 并缓存，边转边显示进度 |
| 图片预览 | 大图走低清转码（较小边默认封顶 1600），灯箱可看原图；处理 EXIF 方向 |
| 下载 / 导出 | 三种形态：浏览器打包下载（zip）、导出到本机目录、导出保留文件路径清单 |
| 导出清单 | 导出 / 打包时生成 CSV 明细（目录、文件名、是否保留、是否已导出、大小），Excel 可直接打开 |
| 多端 | 电脑浏览器与手机（同 Wi-Fi）均可打开；手机端为移动端布局 |
| 控制台 | 黑窗口（控制台版）或 WebView2 本地窗口（wails 版）实时显示地址、口令与日志 |
| QC 质量检测 | 自动检出损坏 / 重复 / 空镜 / 镜头脏点 / 模糊 / 欠曝 / 过曝 / 噪点 / 视频抖动，卡片徽章 + 灯箱红框标注，**只标注、绝不修改保留 / 不保留结论** |

## 2. 快速开始

### 2.1 使用便携包（推荐）

便携包包含三项：`medreview.exe`、`start.bat`、`说明.txt`。

```
双击 start.bat
  1) 素材目录：可把文件夹拖入窗口、粘贴完整路径，或直接回车沿用上次的目录
  2) 下载口令：可自定义（如 1234），直接回车则每次随机生成
  3) 监听端口：直接回车使用 8080；端口被占用时改用其他端口（如 8081）

也可以：把素材文件夹直接拖到 start.bat 图标上 —— 直接用该目录启动，不再询问。
```

启动后浏览器自动打开**审阅页**（蓝色「审阅模式 · 只读标记」徽章）；需要下载时在页面内切换到**下载页**（绿色「下载模式」徽章）。关掉黑窗口即停止服务。

运行环境：Windows 10/11 64 位（中文系统，启动器提示为 GBK 编码）；无需安装任何依赖。

### 2.2 从源码构建

前置：

1. Go 1.26 及以上（不使用 CGO，无需 gcc）。
2. 需要转码能力时，把 `ffmpeg.exe` 与 `ffprobe.exe` 放入 `tools/ffmpeg/`（构建时被 `go:embed` 打进二进制；缺失则编出的程序没有转码与低清预览能力）。

```bash
# 控制台版（黑窗口）
go build -o medreview.exe .

# WebView2 本地窗口版（无黑窗，需要 desktop 与 production 两个 tag）
go build -tags "wails,desktop,production" -ldflags "-H=windowsgui" -o medreview_ui.exe .

# 纯 Win32 本地窗口版（无黑窗，目标机无 WebView2 时的兜底）
go build -tags gui -ldflags "-H=windowsgui" -o medreview_ui.exe .
```

> **tag 铁律**：wails 版必须同时带 `desktop` 与 `production`。只写 `wails` 时，wails 会编入「未使用正确构建标签」的运行时报错弹窗，且桌面前端为空壳，窗口永远出不来。

### 2.3 Linux（headless）

仓库可直接构建 Linux 版本（无 GUI），两种形态：

```bash
./build-linux.sh            # 单文件版（约 252 MB）：内嵌 ffmpeg / ffprobe / ImageMagick
./build-linux.sh --slim     # 精简版（约 12 MB）：工具外置，fnOS 包用这个
```

- **单文件版（默认）**：构建前把 Linux 版 `ffmpeg`、`ffprobe` 放进 `tools/ffmpeg-linux/`，把静态 `magick` 放进 `tools/imagemagick-linux/`（两者都不入库）。首次运行会把工具释放到 `~/.cache/medreview/`（按文件大小比对，内置版本更新自动重释），之后二进制可拷到任何 Linux 机器直接跑。
- **精简版（`--slim`）**：不内嵌，运行时在程序目录 `tools/ffmpeg/`、`tools/imagemagick/` 或 `PATH` 里找工具；体积小，适合随包分发（fnOS 原生包即此形态）。
- **RAW / HEIC**：依赖 ImageMagick 的 `magick`；单文件版已内嵌，精简版需自备（`PATH` 或 `tools/imagemagick/`）。
- **系统服务**：`packaging/linux/medreview.service` 是可直接改用的 systemd 模板（改路径、用户、素材目录即可）。
- **硬件加速**：QC 解码的 `auto` 候选为 `cuda → qsv → vaapi`；`-qcdec` 也接受 `vaapi`（设备节点默认自动找 `/dev/dri/renderD128…131`，可用 `MEDREVIEW_VAAPI_DEVICE` 覆盖）。编码仍为 `nvenc` / `qsv` / CPU 三档，VAAPI 编码尚未接入。
- **打开浏览器**：有桌面环境时用 `xdg-open`；无桌面（服务器）时忽略即可，地址会打印在日志里。

### 2.4 macOS（headless）

Go 原生支持 darwin，仓库可直接交叉编译（Apple Silicon / Intel 均可）：

```bash
./build-macos.sh              # 当前机器架构
./build-macos.sh arm64        # 或 amd64
./build-macos.sh universal    # lipo 合并成通用二进制（需在 macOS 上执行）
```

- **内嵌 ffmpeg**：构建前把 macOS 版 `ffmpeg` / `ffprobe` 放进 `tools/ffmpeg-macos-arm64/` 或 `tools/ffmpeg-macos-amd64/`（按目标架构，均不入库）；运行时首次释放到 `~/Library/Caches/medreview/`。
- **ImageMagick**：macOS 没有官方静态单文件版，**不内嵌**——`brew install imagemagick` 提供 `magick` 即可（用于 RAW / HEIC）。
- **未签名**：首次启动会被 Gatekeeper 拦下，在「系统设置 → 隐私与安全性」里允许，或执行 `xattr -d com.apple.quarantine ./medreview`。
- **硬件加速**：macOS 暂未接入（VideoToolbox 未支持），一律 CPU。
- **无桌面窗口**：headless 形态，启动后从日志里取访问地址。

### 2.5 前后端分离 / 容器

```bash
# 后端只出 API（前端可部署在任意机器）
medreview -api-only -addr 0.0.0.0:8080 -root /media -db /data/medreview.db

# 前端：把 web/ 放到任意静态服务器；页面里点「服务器地址…」填后端地址，
# 或首次用 ?api=http://nas:8080 打开（地址会记在 localStorage）
```

- **CORS 默认策略**：放行本机来源 + 私有网段（10/8、172.16/12、192.168/16）；公网域名必须 `-cors https://your.domain` 显式列出，`-cors-lan=false` 可关掉私网放行。
- **同源部署**（推荐）：反向代理把 `/` 和 `/api` 都指向同一个后端，免 CORS，还能在代理层加认证。
- **远端管理**：异机选目录/重扫需要 `-allow-remote-admin -admin-token <随机串>`，默认关闭。
- **跨源下载**：用 `Authorization: Bearer <下载口令>`（跨源 cookie 不可靠）。
- **Docker**：`docker build -t medreview .`，素材挂到 `/media`（只读）、数据挂 `/data`。
- **公网模式（内置认证）**：`-public -auth 用户名:密码` 启用登录页 + 签名会话 + 登录限流 + CSRF 校验 + 审计日志（JSONL）；配合 `-tls-cert/-tls-key` 可直接跑 HTTPS。不需要反代也能挡住匿名访问。
- **账号怎么设**：单账号 `-auth 用户名:密码` 即可；多账号用 `medreview -hash-pass -`（从标准输入读密码，不进 shell 历史）生成 bcrypt 哈希，写进 `users.json`：`{"users":{"alice":"$2a$10$..."}}`，启动加 `-auth-file users.json`。改账号需重启；固定 `-session-key` 可让重启后不用重新登录。
- ⚠️ **公网部署前必读** [`docs/design/public-deployment.md`](docs/design/public-deployment.md)：媒体接口本身无鉴权，必须有认证层（内置 `-public` 或反向代理）。

## 3. 架构与源码结构

单进程 Go 服务 + 原生前端，无外部服务依赖（数据库为嵌入式 SQLite，通过纯 Go 驱动 `modernc.org/sqlite`）。

```
main.go              入口：解析参数 → 建 app → 绑定端口 → 启动扫描 → 按构建模式进入控制台/窗口
app.go               运行时核心：bind / printBanner / startScan / serve / SetRoot / SetToken / Rebind / Status / 退出清缓存
api.go               HTTP 路由与全部业务处理（标记、认领、媒体、下载导出、目录浏览）
store.go             数据访问：文件 / 目录 / 标记 / 认领 / 统计；absPath = root + relPath
scanner.go           素材目录扫描：以 rel_path 为唯一键增量入库，按 size+mtime 检测内容变更并作废旧结论
db.go                SQLite 建表与连接
video.go             ffmpeg 转码：探测 → 转码 → 缓存 → 预热，支持协同取消
image.go             图片低清预览：转码缓存、EXIF 方向、目录预热
copy.go              导出到本机目录（含导出清单写入）
zip.go               打包下载（zip 内附带导出清单，运行目录另落一份）
manifest.go          导出清单 CSV 生成（UTF-8 BOM、Excel 公式注入转义）
events.go            SSE 事件广播（扫描进度、标记变化、认领变化、整目录标记）
lan.go               本机局域网 IPv4 枚举（带缓存，含伪网卡过滤）
ffmpeg_embed.go      内嵌 ffmpeg / ffprobe 的释放逻辑（Windows）
console.go           日志环形缓冲，供本地窗口读取
gui_win32.go         纯 Win32 本地窗口（构建 tag: gui || wails）
gui_wails.go         WebView2 本地窗口（构建 tag: wails）
mode_*.go            构建模式常量（console / gui / wails）
web/                 审阅页与下载页前端（go:embed 进二进制）
ui/                  WebView2 本地控制台窗口界面（go:embed 进二进制）
tools/               build_single.py（单文件化）、make_lnk.py（生成快捷方式）；tools/ffmpeg*/ 为内嵌源（不入库）
local/               本机调试脚本与一次性产物（不入库）
_verify/             验收脚本（见第 8 节）
build-linux.sh       Linux 构建脚本（默认单文件版；--slim 精简版）
build-macos.sh       macOS 构建脚本（arm64 / amd64 / universal）
tools/ffmpeg-linux/  Linux 版 ffmpeg/ffprobe（内嵌源，不入库）
tools/imagemagick-linux/  Linux 版静态 magick（内嵌源，不入库）
packaging/linux/     systemd 服务模板
```

前端改动后必须重新构建：`web/` 与 `ui/` 均在编译期嵌入二进制。

## 4. 构建

三种构建互斥，由构建标签决定 `buildMode`：

| 构建 | 控制台形态 | 备注 |
| --- | --- | --- |
| 默认 | 黑窗口 | 日常使用 |
| `-tags "wails,desktop,production"` | WebView2 本地窗口 | 窗口内容为 HTML/CSS，可就地改素材根目录 / 端口 / 口令 |
| `-tags gui` | 纯 Win32 控件窗口 | WebView2 不可用时的兜底 |

ffmpeg 内嵌机制（Windows）：`tools/ffmpeg/` 下的 `ffmpeg.exe`、`ffprobe.exe` 通过 `go:embed` 编入二进制；磁盘上 `tools/ffmpeg/` 存在时优先使用磁盘版本，否则首次启动释放到 `%LocalAppData%\medreview\tools\ffmpeg\`（按文件大小比对决定是否重新释放）。因此二进制体积约 220 MB 属预期。

## 5. 运行参数

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-root` | 上次记录 | 素材根目录；位置参数（拖到图标上）亦可 |
| `-addr` | `:8080` | 监听地址，如 `:8080`、`0.0.0.0:8080` |
| `-token` | 每次随机 | 下载口令，24 位十六进制（12 字节熵） |
| `-open-dl` | false | 关闭下载权限校验（任何页面都能打包下载） |
| `-db` | `medreview.db` | 数据库路径 |
| `-cache` | `cache` | 转码缓存目录 |
| `-vres` | 720 | 转码规格：较小边封顶像素，0 为保持原分辨率 |
| `-ires` | 1600 | 图片预览规格：较小边封顶像素，0 为一律原图 |
| `-vjobs` | 2 | 视频预热并发数 |
| `-vkeep` | false | 保留上一轮转码产物（默认每次启动清理） |
| `-open` | false | 启动后自动打开审阅页 |
| `-log` | 自动选择 | 日志文件路径；依次尝试当前目录 → 程序目录 → 临时目录 |

退出行为：正常退出（Ctrl+C、关闭窗口、点停止服务）清空转码与预览缓存；被强制结束（直接关闭黑窗口、任务管理器结束）不执行清理，残留由下次启动兜底清理。

## 6. HTTP 接口

| 方法与路径 | 权限 | 说明 |
| --- | --- | --- |
| `GET /api/status` | 无 | 扫描进度与统计 |
| `GET /api/folders`、`GET /api/folder` | 无 | 目录树与目录信息 |
| `GET /api/files` | 无 | 目录内文件列表（分页、筛选） |
| `GET /api/media` | 无；带 `dl=1` 时需下载权限且该文件标记为保留 | 图片 / 视频内容 |
| `GET /api/vtrans`、`/api/vtrans-status`、`/api/vtrans-warmup` | 无 | 转码产物与转码状态 |
| `POST /api/review`、`/api/review/batch` | 无（受目录认领锁约束） | 标记 |
| `POST /api/review/clear-folder` | 无（受认领锁约束） | 清空本层目录标记 |
| `POST /api/review/folder-decision` | 无（受认领锁约束） | 本层目录整体标为保留 / 不保留 |
| `POST /api/claim`、`/api/claim/release` | 无 | 认领 / 释放目录 |
| `GET /api/users` | 无 | 近期使用过的批注名（进程内存，重启失效） |
| `GET /api/events` | 无 | SSE 事件流 |
| `GET /api/browse` | **仅本机回环** | 枚举本机磁盘目录（用于选择素材根目录） |
| `POST /api/scan` | **仅本机回环** | 重新指定素材根目录并扫描 |
| `GET /api/zip` | 下载权限 | 打包下载保留文件 |
| `GET /api/export` | 下载权限 | 保留文件的本机绝对路径清单 |
| `POST /api/copy` | 下载权限 | 导出保留文件到本机目录 |
| `GET /api/browse-dest` | 下载权限 | 导出目标目录浏览与新建 |

「下载权限」＝ URL 中 `?t=<口令>` 或 Cookie `mr_dl` 与当前口令一致，或使用 `-open-dl` 关闭校验。

## 7. 数据模型

- `file`：以 `rel_path`（相对素材根目录）为唯一键；`present` 表示本轮扫描是否存在。
- `folder`：以 `rel_path` 合并；父子关系通过 `parent_id`。
- `review`：`file_id` 主键；`decision` 取 `0` 未定 / `1` 保留 / `2` 不保留，并记录 `reviewer` 与时间。
- `claim`：`folder_id` 主键，记录认领人。
- `meta`：仅存 `root_path`（上次素材根目录，绝对路径）与扫描完成时间。

扫描语义：新一轮扫描先把全部文件置为 `present=0`；命中相同 `rel_path` 时按 `size`+`mtime` 比对，**仅当内容变化才作废该文件的既有结论**，未变化则保留标记。因此素材目录整体迁移到新路径后，只要文件本身未变，标记会自动接上。

## 8. 验收

`_verify/` 下 7 套脚本，合计 350+ 项断言，直接对构建产物（exe）运行：自造素材、自起自停服务、失败即非 0 退出。

```
check_mobile_shell.py      手机适配、认领、门禁、局域网、悬停预览、画质开关、缩放
check_keep_scope.py        标记与打包口径、可见范围、整目录标记、认领拦截、接口鉴权
check_image_preview.py     图片预览、EXIF 方向、缓存、改档、目录预热（需 Pillow）
check_download_and_720p.py 转码规格与四个下载出口的字节一致性
check_startup_modes.py     启动方式、构建模式、接口存在性与权限
check_startup_clean.py     启动与退出的缓存清理行为
check_token_random.py      下载口令随机性、启动器不写死口令
```

另有 `go test -run 'TestSetRoot|TestSetToken|TestRebind'`，覆盖运行时修改根目录 / 口令 / 端口三条路径。

## 9. 安全模型与风险

**本项目定位为「受信任局域网内的本机工具」，不具备面向不可信网络的安全能力。部署前请阅读本节。**

### 9.1 认证与授权现状

| 项目 | 状态 |
| --- | --- |
| 查看素材（`/api/media`、`/api/vtrans`） | **无鉴权**。任何能访问服务端口的设备可读取原始素材 |
| 标记、认领、清空目录标记 | **无鉴权**。仅受目录认领锁约束（认领可被其他人再次覆盖） |
| 下载 / 打包 / 导出 | 需下载口令（默认每次随机，24 位十六进制） |
| 枚举磁盘目录、改素材根目录 | 仅允许本机回环地址（`127.0.0.1` / `::1`），局域网调用返回 403 |
| 跨站请求 | CORS 仅放行本机来源（回环、localhost、本机局域网 IP）；非浏览器客户端不受 CORS 约束 |

### 9.2 风险清单

| 风险 | 等级 | 说明与建议 |
| --- | --- | --- |
| 素材可被局域网任意设备读取 | **高** | 审阅页必须能看图才能标记，因此媒体接口无鉴权。同一 Wi-Fi 内任何人可浏览、下载全部素材。**仅在受信任网络中使用**，不要把服务端口暴露到公网、公司大网或不可信 Wi-Fi |
| 标记可被任意设备修改 | 中 | 无鉴权写入，恶意或误操作可篡改结论。建议用于可信团队；需要强约束时应置于独立网段 |
| 下载口令泄露面 | 中 | 口令出现在 URL 查询串、`urls.txt`、运行日志与浏览器历史。已设置 `Referer-Policy: no-referrer` 防止跳转外泄；口令每次启动随机，重启即失效 |
| 明文传输 | 中 | 服务为 HTTP，无 TLS。局域网内口令与素材内容均明文传输 |
| ffmpeg 解码不受信任素材 | 中 | 启动与换目录后会自动预热（解码）素材。ffmpeg 历史上存在编解码器 CVE。**只审阅可信来源的素材** |
| 本地数据库未加密 | 中 | `medreview.db` 明文保存审阅结论与批注名，应视为敏感数据，与程序一同保管，不要随意外传 |
| 导出可写入服务器任意目录 | 低 | 持有下载口令者可指定任意目标目录让服务写入保留文件（已禁止写入素材目录内部及磁盘根目录）。写入内容为已标记素材，执行风险有限 |
| 二进制未签名 | 低 | 首次运行可能触发 Windows SmartScreen「已保护你的电脑」，需手动选择「仍要运行」。缺少代码签名证书所致，非程序异常 |
| 日志含本机信息 | 低 | 运行日志含素材绝对路径、局域网地址与下载口令。日志落在程序运行目录，不要随意外发 |

### 9.3 已实施的安全措施

- 素材文件全程只读：不写入、不移动、不删除原文件。
- 导出目标不得位于素材目录内部，避免自我嵌套复制。
- 导出清单 CSV 对以 `= + - @` 开头的单元格做转义，防止 Excel 公式注入。
- 导出 / 打包拒绝包含 `..` 段的相对路径（纵深防御）。
- 目录枚举与重扫接口限制为本机回环地址。
- 下载、打包、导出、导出目标浏览四类出口统一校验下载口令。
- 响应头固定 `X-Content-Type-Options: nosniff` 与 `Referer-Policy: no-referrer`。
- 随机数源失败时拒绝启动，不降级为固定口令。

## 10. 已知限制

- 官方分发目标为 Windows（ffmpeg 内嵌与快捷方式生成依赖 Windows）。
- Linux 为自建 headless 形态（见 2.3）：单文件版内嵌工具，精简版需自行提供 `tools/ffmpeg/`。
- macOS 同为自建 headless（见 2.4）：内嵌 ffmpeg（ImageMagick 需 brew），无桌面窗口、无硬件加速。
- WebView2 版在部分环境会出现渲染进程崩溃循环；不稳定时请使用控制台版。
- 转码缓存关闭程序时清空，下次启动需重新转码（`--vkeep` 可保留）。
- 单实例使用同一端口；端口被占用时启动失败并给出提示。
- 素材目录内的软链接、稀疏文件不在支持范围内。

## 11. QC 质量检测

质量自动检测（QC）是审阅工作流里的**可视化标注层**：在扫描 / 转码时并行跑一组检测器，把结果以卡片徽章与灯箱红框呈现，**只标注、绝不修改 `review.decision`**（保留 / 不保留始终由人工决定）。

### 11.1 检测器

| 检测器 | 说明 | 输出 |
| --- | --- | --- |
| 损坏 corrupted | 文件损坏、无法解码、截断 | 布尔标记 |
| 重复 duplicate | 内容哈希 + 时间 / 设备 + 像素二次确认，过滤误判 | 哈希值 |
| 空镜 blank | 纯色、全黑、全白、大面积单一颜色 | 置信度 |
| 镜头脏点 lens-dirt | 镜头污渍 / 灰尘斑点；**灯箱中以半透明红框标注位置** | 置信度 + 包围盒 |
| 模糊 blur | 对焦不准、运动模糊 | 置信度（拉普拉斯标准差） |
| 欠曝 / 过曝 exposure | 严重偏暗或偏亮 | 欠曝 / 过曝标记 + 平均亮度 + 近黑 / 近白像素占比 |
| 噪点 noise | 高 ISO 噪点严重 | 置信度（平坦块噪声底线） |
| 抖动 shake | **视频**帧间整体平移的往复震荡（防抖检测） | 振幅 RMS（1px ≈ 画面 1.5%） |

> **校准说明**：上述阈值**尚未用真实素材校准**。当前策略遵循「宁漏勿滥」，并预留 2–3× 余量，避免误伤正常照片。正式发布前需用真实素材集校准后再上线。

### 11.2 结果展示

- **卡片徽章**：缩略图右上角的 `.qc-stack` 叠放徽章，每个检出的问题一个标签（损坏 / 重复 / 空镜 / 脏污 / 模糊 / 曝光 / 噪点）。
- **灯箱红框**：灯箱大图里，镜头脏点以半透明红框叠加在脏点实际位置；按 **H 键**可隐藏 / 恢复红框（偏好会被记住）。
- **问题清单 CSV**：下载页「问题清单」按钮导出 `qc-report.csv`，含损坏 / 重复 / 空镜 / 镜头脏污 / 模糊 / 欠曝 / 过曝 / 噪点的文件清单（含本目录及子目录），Excel 可直接打开。

### 11.3 性能与硬件

- 视频帧由 ffmpeg 抽取（图片取单帧），检测算法在 CPU 上运行。
- 视频解码可走硬件加速（`cuda` / `qsv` / `d3d11va`）做 NVDEC 抽帧，失败自动回退软解；检测数值不受影响。硬件探测结论跨启动缓存。
- 源文件全程只读，QC 不写入、不移动、不删除任何原文件。

## 许可证

AGPL-3.0（GNU Affero General Public License v3.0），见 [LICENSE](./LICENSE)。

第三方组件：构建时通过 `go:embed` 打包的 `ffmpeg` / `ffprobe`（[FFmpeg](https://ffmpeg.org/)，依其自身许可证发布，本项目不随仓库分发，需自行准备）。

---

# English

A local, Windows-based manual photo/video triage tool. Multiple people mark files "keep / reject" in parallel per folder, then export or package the results in one click. **Source files are read-only throughout**: the program only reads media, and never moves, modifies, or deletes any original file.

## Contents

- [1. Features](#1-features)
- [2. Quick start](#2-quick-start)
- [3. Architecture and source layout](#3-architecture-and-source-layout)
- [4. Build](#4-build)
- [5. Runtime flags](#5-runtime-flags)
- [6. HTTP API](#6-http-api)
- [7. Data model](#7-data-model)
- [8. Acceptance tests](#8-acceptance-tests)
- [9. Security model and risks](#9-security-model-and-risks)
- [10. Known limitations](#10-known-limitations)
- [11. QC (quality checks)](#11-qc-quality-checks)

## 1. Features

| Capability | Description |
| --- | --- |
| Folder tree | Pick a media folder from the server-side tree; optional "include parent" browsing (files directly in the parent are listed too, while marking and packaging rules stay unchanged) |
| Manual marking | Mark a single file, batch-mark the visible area, **or mark the whole folder** (all keep / all reject / clear this folder's marks) |
| Folder claiming | Per-folder lock: once claimed, others are read-only. **The lock applies to this folder only and does not recurse** |
| Video transcoding | Formats browsers cannot play (HEVC/H.265 etc.) are transcoded to H.264 by the embedded ffmpeg and cached, with live progress |
| Image preview | Large images use a low-res transcode (smaller side capped at 1600 by default); the lightbox can show the original; EXIF orientation is handled |
| Download / export | Three modes: browser ZIP download, export to a local directory, export the list of kept file paths |
| Export manifest | A CSV manifest (folder, filename, kept, exported, size) is generated on export/zip; opens directly in Excel |
| Multi-device | Desktop browsers and phones on the same Wi-Fi; the phone layout is mobile-native |
| Console | Live address, token, and logs in a console window or a WebView2 desktop window (wails build) |
| QC | Automatically detects corrupted / duplicate / blank / lens-dirt / blur / under- and over-exposure / noise / video shake, shown as card badges and lightbox boxes. **Annotation only — never changes keep/reject decisions** |

## 2. Quick start

### 2.1 Using the portable package (recommended)

The portable package contains three files: `medreview.exe`, `start.bat`, `说明.txt`.

```
Double-click start.bat
  1) Media folder: drag a folder into the window, paste a full path, or press Enter to reuse the last one
  2) Download token: custom (e.g. 1234), or press Enter for a random token each start
  3) Port: press Enter for 8080; if it is taken, use another port (e.g. 8081)

Alternatively: drag a media folder onto the start.bat icon to start with that folder directly.
```

The browser opens the **review page** automatically (blue "review mode · read-only marking" badge). Switch to the **download page** in-page when you need downloads (green "download mode" badge). Closing the console window stops the service.

Runtime: Windows 10/11 x64 (Chinese systems; the launcher prompts are GBK-encoded). No dependencies required.

### 2.2 Building from source

Prerequisites:

1. Go 1.26+ (no CGO, no gcc needed).
2. For transcoding, put `ffmpeg.exe` and `ffprobe.exe` into `tools/ffmpeg/` (they are embedded into the binary via `go:embed`; without them the build has no transcoding or low-res preview).

```bash
# Console build (black window)
go build -o medreview.exe .

# WebView2 desktop window build (no console; requires both desktop and production tags)
go build -tags "wails,desktop,production" -ldflags "-H=windowsgui" -o medreview_ui.exe .

# Pure Win32 window build (no console; fallback when WebView2 is unavailable)
go build -tags gui -ldflags "-H=windowsgui" -o medreview_ui.exe .
```

> **Tag rule**: the wails build must include both `desktop` and `production`. With only `wails`, the binary embeds the "incorrect build tags" runtime error dialog and the desktop frontend is an empty shell — the window never appears.

### 2.3 Linux (headless)

The repository builds a GUI-less Linux binary in two flavours:

```bash
./build-linux.sh            # single-file build (~252 MB): embeds ffmpeg / ffprobe / ImageMagick
./build-linux.sh --slim     # slim build (~12 MB): tools are external; used by the fnOS package
```

- **Single-file (default)**: before building, put the Linux `ffmpeg` and `ffprobe` into `tools/ffmpeg-linux/`, and a static `magick` into `tools/imagemagick-linux/` (neither is tracked). On first run the tools are extracted to `~/.cache/medreview/` (size-compared, re-extracted when the embedded version changes), so the binary can be copied to any Linux machine and just run.
- **Slim (`--slim`)**: nothing embedded; tools are looked up in `tools/ffmpeg/`, `tools/imagemagick/` next to the binary, then `PATH`. Small, good for packaging (the fnOS native package uses this).
- **RAW / HEIC**: needs ImageMagick's `magick`; the single-file build embeds it, the slim build requires your own (`PATH` or `tools/imagemagick/`).
- **Service**: `packaging/linux/medreview.service` is a ready-to-adapt systemd unit (adjust paths, user, media root).
- **Hardware acceleration**: QC decoding tries `cuda → qsv → vaapi` in `auto` mode; `-qcdec` also accepts `vaapi` (the device node is auto-detected at `/dev/dri/renderD128…131`, override with `MEDREVIEW_VAAPI_DEVICE`). Encoding still offers `nvenc` / `qsv` / CPU only; VAAPI encoding is not wired up yet.
- **Browser**: `xdg-open` is used when a desktop session exists; on headless servers the address is printed to the log instead.

### 2.4 macOS (headless)

Go supports darwin natively; the repository cross-compiles for both Apple Silicon and Intel:

```bash
./build-macos.sh              # host architecture
./build-macos.sh arm64        # or amd64
./build-macos.sh universal    # lipo both into one universal binary (run on macOS)
```

- **Embedded ffmpeg**: before building, put the macOS `ffmpeg` / `ffprobe` into `tools/ffmpeg-macos-arm64/` or `tools/ffmpeg-macos-amd64/` (matching the target arch; neither is tracked). First run extracts them to `~/Library/Caches/medreview/`.
- **ImageMagick**: there is no official static single-file macOS build, so it is **not embedded** - `brew install imagemagick` provides `magick` for RAW / HEIC.
- **Unsigned**: the first launch is blocked by Gatekeeper. Allow it in System Settings > Privacy & Security, or run `xattr -d com.apple.quarantine ./medreview`.
- **Hardware acceleration**: not wired up for macOS (VideoToolbox unsupported); CPU only.
- **No desktop window**: headless; read the address from the log.

### 2.5 Split frontend/backend and containers

```bash
# API-only backend (the frontend can live on any machine)
medreview -api-only -addr 0.0.0.0:8080 -root /media -db /data/medreview.db

# Frontend: serve web/ from any static host; click "Server address..." in the UI,
# or open once with ?api=http://nas:8080 (the address is remembered in localStorage).
```

- **Default CORS policy**: local origins plus private ranges (10/8, 172.16/12, 192.168/16). Public domains must be listed explicitly with `-cors https://your.domain`; `-cors-lan=false` disables the private-range allowance.
- **Same-origin (recommended)**: put a reverse proxy in front, routing both `/` and `/api` to the backend - no CORS, and you can add authentication at the proxy.
- **Remote admin**: choosing folders / rescanning from another machine needs `-allow-remote-admin -admin-token <random>`; off by default.
- **Cross-origin downloads**: use `Authorization: Bearer <download-token>` (cookies are unreliable cross-origin).
- **Docker**: `docker build -t medreview .`; mount media read-only at `/media` and data at `/data`.
- **Public mode (built-in auth)**: `-public -auth user:password` enables a login page, signed sessions, login rate limiting, CSRF checks, and a JSONL audit log; add `-tls-cert/-tls-key` to serve HTTPS directly. Anonymous access is blocked without a reverse proxy.
- **Accounts**: a single account is just `-auth user:password`. For multiple accounts, run `medreview -hash-pass -` (reads the password from stdin so it stays out of shell history), put the bcrypt hashes into `users.json` as `{"users":{"alice":"$2a$10$..."}}`, and start with `-auth-file users.json`. Changes require a restart; pin `-session-key` to keep sessions valid across restarts.
- ⚠️ **Read [`docs/design/public-deployment.md`](docs/design/public-deployment.md) before exposing it to the internet**: media endpoints are unauthenticated by design, so some auth layer (built-in `-public` or a reverse proxy) is mandatory.

## 3. Architecture and source layout

A single-process Go service with a native frontend, no external services (SQLite embedded via the pure-Go driver `modernc.org/sqlite`).

```
main.go              Entry: parse flags → build app → bind port → start scan → enter console/window per build mode
app.go               Runtime core: bind / printBanner / startScan / serve / SetRoot / SetToken / Rebind / Status / cache cleanup on exit
api.go               HTTP routes and all handlers (marking, claims, media, download/export, folder browsing)
store.go             Data access: files / folders / reviews / claims / stats; absPath = root + relPath
scanner.go           Media scan: incremental upsert keyed by rel_path; invalidates stale decisions when size+mtime change
db.go                SQLite schema and connection
video.go             ffmpeg transcoding: probe → transcode → cache → warm-up, cooperative cancellation
image.go             Low-res image preview: transcode cache, EXIF orientation, per-folder warm-up
copy.go              Export to a local directory (writes the manifest)
zip.go               ZIP download (manifest included in the archive and written next to it)
manifest.go          Export manifest CSV (UTF-8 BOM, Excel formula-injection escaping)
events.go            SSE broadcasting (scan progress, decision changes, claim changes, folder-wide marking)
lan.go               Local LAN IPv4 enumeration (cached, filters phantom adapters)
ffmpeg_embed.go      Embedded ffmpeg / ffprobe extraction (Windows)
console.go           Ring-buffer logs for the desktop window
gui_win32.go         Pure Win32 desktop window (build tag: gui || wails)
gui_wails.go         WebView2 desktop window (build tag: wails)
mode_*.go            Build-mode constants (console / gui / wails)
web/                 Review and download page frontends (go:embed into the binary)
ui/                  WebView2 desktop console UI (go:embed into the binary)
tools/               build_single.py (single-file build), make_lnk.py (shortcut creation); tools/ffmpeg*/ are embed sources (not tracked)
local/               local debug scripts and scratch artifacts (not tracked)
_verify/             Acceptance scripts (see section 8)
build-linux.sh       Linux build script (single-file by default; --slim for a small build)
build-macos.sh       macOS build script (arm64 / amd64 / universal)
tools/ffmpeg-linux/  Linux ffmpeg/ffprobe (embed source, not tracked)
tools/imagemagick-linux/  Linux static magick (embed source, not tracked)
packaging/linux/     systemd unit template
```

Frontend changes require a rebuild: both `web/` and `ui/` are embedded at compile time.

## 4. Build

The three builds are mutually exclusive; build tags select `buildMode`:

| Build | Console | Notes |
| --- | --- | --- |
| Default | Console window | Daily use |
| `-tags "wails,desktop,production"` | WebView2 desktop window | HTML/CSS UI; can change media root / port / token in place |
| `-tags gui` | Pure Win32 controls | Fallback when WebView2 is unavailable |

ffmpeg embedding (Windows): `ffmpeg.exe` and `ffprobe.exe` under `tools/ffmpeg/` are compiled into the binary via `go:embed`. If `tools/ffmpeg/` exists on disk it takes precedence; otherwise the tools are extracted to `%LocalAppData%\medreview\tools\ffmpeg\` on first start (re-extracted when file sizes differ). A binary size of ~220 MB is therefore expected.

## 5. Runtime flags

| Flag | Default | Description |
| --- | --- | --- |
| `-root` | Last used | Media root folder; a positional argument (drag onto the icon) also works |
| `-addr` | `:8080` | Listen address, e.g. `:8080`, `0.0.0.0:8080` |
| `-token` | Random per start | Download token, 24 hex chars (12 bytes of entropy) |
| `-open-dl` | false | Disable download authorization (any page can zip/download) |
| `-db` | `medreview.db` | Database path |
| `-cache` | `cache` | Transcode cache directory |
| `-vres` | 720 | Transcode size: cap on the smaller side; 0 keeps the original resolution |
| `-ires` | 1600 | Image preview size: cap on the smaller side; 0 always uses originals |
| `-vjobs` | 2 | Concurrent video warm-up jobs |
| `-vkeep` | false | Keep the previous run's transcode outputs (cleared on every start by default) |
| `-open` | false | Open the review page automatically after start |
| `-log` | Auto | Log file path; tries the current directory → program directory → temp directory |

Exit behavior: a normal exit (Ctrl+C, closing the window, stop button) clears transcode and preview caches; a forced kill skips cleanup, and leftovers are cleaned on the next start.

## 6. HTTP API

| Method and path | Auth | Description |
| --- | --- | --- |
| `GET /api/status` | None | Scan progress and stats |
| `GET /api/folders`, `GET /api/folder` | None | Folder tree and folder info |
| `GET /api/files` | None | Files in a folder (paged, filtered) |
| `GET /api/media` | None; with `dl=1` requires download auth and a kept file | Image / video content |
| `GET /api/vtrans`, `/api/vtrans-status`, `/api/vtrans-warmup` | None | Transcode outputs and status |
| `POST /api/review`, `/api/review/batch` | None (subject to folder claim lock) | Marking |
| `POST /api/review/clear-folder` | None (subject to claim lock) | Clear marks in this folder |
| `POST /api/review/folder-decision` | None (subject to claim lock) | Mark the whole folder keep / reject |
| `POST /api/claim`, `/api/claim/release` | None | Claim / release a folder |
| `GET /api/users` | None | Recently used reviewer names (in-memory, lost on restart) |
| `GET /api/events` | None | SSE event stream |
| `GET /api/browse` | **Loopback only** | Enumerate local disk folders (for choosing the media root) |
| `POST /api/scan` | **Loopback only** | Set a new media root and rescan |
| `GET /api/zip` | Download auth | ZIP the kept files |
| `GET /api/export` | Download auth | Absolute paths of kept files |
| `POST /api/copy` | Download auth | Copy kept files to a local directory |
| `GET /api/browse-dest` | Download auth | Browse / create export destination folders |

"Download auth" = `?t=<token>` in the URL or the `mr_dl` cookie matches the current token, or authorization is disabled with `-open-dl`.

## 7. Data model

- `file`: unique key `rel_path` (relative to the media root); `present` marks whether the file exists in this scan.
- `folder`: merged by `rel_path`; parent/child via `parent_id`.
- `review`: primary key `file_id`; `decision` is `0` undecided / `1` keep / `2` reject, plus `reviewer` and timestamp.
- `claim`: primary key `folder_id`, records the claimer.
- `meta`: only `root_path` (last media root, absolute) and the scan completion time.

Scan semantics: each new scan first sets every file to `present=0`; when the same `rel_path` is found, `size`+`mtime` are compared and **existing decisions are invalidated only when the content changed**. Moving the media folder to a new path therefore keeps decisions as long as files are unchanged.

## 8. Acceptance tests

Seven scripts under `_verify/`, 350+ assertions in total, run against the built executable: they create their own media, start/stop the service themselves, and exit non-zero on failure.

```
check_mobile_shell.py      Mobile layout, claims, name gate, LAN, hover preview, quality toggle, zoom
check_keep_scope.py        Marking/packaging scope, visible range, folder-wide marking, claim blocking, API auth
check_image_preview.py     Image preview, EXIF orientation, cache, file change, folder warm-up (needs Pillow)
check_download_and_720p.py Transcode sizes and byte-identity across the four download paths
check_startup_modes.py     Startup modes, build modes, endpoint presence and permissions
check_startup_clean.py     Cache cleanup on startup and exit
check_token_random.py      Token randomness; launcher never hardcodes a token
```

There is also `go test -run 'TestSetRoot|TestSetToken|TestRebind'`, covering runtime changes of root / token / port.

## 9. Security model and risks

**This project is a "local tool for a trusted LAN" and has no defenses for untrusted networks. Read this section before deploying.**

### 9.1 Authentication and authorization today

| Item | Status |
| --- | --- |
| Viewing media (`/api/media`, `/api/vtrans`) | **No auth.** Any device that can reach the port can read raw media |
| Marking, claiming, clearing folder marks | **No auth.** Only the folder claim lock applies (and claims can be overridden by others) |
| Download / ZIP / export | Requires the download token (random per start, 24 hex chars) |
| Enumerating folders, changing the media root | Loopback only (`127.0.0.1` / `::1`); LAN calls return 403 |
| Cross-site requests | CORS allows local origins only (loopback, localhost, local LAN IPs); non-browser clients are not constrained by CORS |

### 9.2 Risk list

| Risk | Level | Notes and advice |
| --- | --- | --- |
| Media readable by any device on the LAN | **High** | The review page must be able to show images to mark them, so media endpoints have no auth. Anyone on the same Wi-Fi can browse and download all media. **Use only on a trusted network**; never expose the port to the internet, a corporate WAN, or untrusted Wi-Fi |
| Marks modifiable by any device | Medium | Unauthenticated writes; malicious or accidental changes can alter decisions. Use with a trusted team; isolate on a dedicated subnet when stronger guarantees are needed |
| Download token exposure | Medium | The token appears in URL query strings, `urls.txt`, runtime logs, and browser history. `Referer-Policy: no-referrer` prevents referrer leakage; the token is random per start and dies on restart |
| Plaintext transport | Medium | HTTP only, no TLS. Tokens and media travel in cleartext on the LAN |
| ffmpeg decoding untrusted media | Medium | Warm-up (decoding) runs automatically on start and folder changes. ffmpeg has a history of codec CVEs. **Only review media from trusted sources** |
| Unencrypted local database | Medium | `medreview.db` stores decisions and reviewer names in plaintext; treat it as sensitive, keep it with the program, and do not share it |
| Export can write anywhere on the server | Low | A token holder can point the export at any destination directory (writes inside the media folder and to disk roots are rejected). Only already-marked media is written, so impact is limited |
| Unsigned binary | Low | First run may trigger Windows SmartScreen ("Windows protected your PC"); choose "Run anyway". This is due to the missing code-signing certificate, not a defect |
| Logs contain local details | Low | Runtime logs include absolute media paths, LAN addresses, and the download token. Logs stay in the run directory; do not share them casually |

### 9.3 Measures already in place

- Media files are read-only throughout: never written, moved, or deleted.
- Export destinations cannot be inside the media folder (prevents recursive self-copying).
- Manifest CSVs escape cells starting with `= + - @` to prevent Excel formula injection.
- Export/ZIP reject relative paths containing `..` (defense in depth).
- Folder enumeration and rescan endpoints are restricted to loopback.
- Download, ZIP, export, and destination browsing all validate the download token.
- Responses always carry `X-Content-Type-Options: nosniff` and `Referer-Policy: no-referrer`.
- Startup is refused when the random source fails; it never falls back to a fixed token.

## 10. Known limitations

- Windows is the official distribution target (embedded ffmpeg and shortcut creation are Windows-specific).
- Linux is supported as a self-built headless binary (see 2.3): the single-file build embeds the tools, the slim build needs `tools/ffmpeg/` alongside.
- macOS is also a self-built headless target (see 2.4): ffmpeg is embedded (ImageMagick via brew), no desktop window, CPU only.
- The WebView2 build may hit renderer crash loops in some environments; use the console build when unstable.
- The transcode cache is cleared when the program exits; the next start transcodes again (`--vkeep` keeps it).
- A single instance per port; if the port is taken, startup fails with a message.
- Symlinks and sparse files inside the media folder are unsupported.

## 11. QC (quality checks)

Quality checks (QC) are the **visual annotation layer** of the review workflow: a set of detectors runs alongside scanning/transcoding and surfaces results as card badges and lightbox boxes. **It only annotates and never touches `review.decision`** (keep/reject is always a human decision).

### 11.1 Detectors

| Detector | Description | Output |
| --- | --- | --- |
| corrupted | Unreadable, truncated, or damaged files | Boolean flag |
| duplicate | Content hash + time/device + pixel confirmation to filter false positives | Hash value |
| blank | Solid color, all black/white, or large uniform areas | Confidence |
| lens-dirt | Lens smudges / dust spots; **shown as translucent boxes at the actual positions in the lightbox** | Confidence + bounding boxes |
| blur | Missed focus, motion blur | Confidence (Laplacian standard deviation) |
| under/over-exposure | Severely dark or bright images | Flag + mean luma + near-black/near-white pixel ratio |
| noise | Severe high-ISO noise | Confidence (flat-block noise floor) |
| shake | **Video** back-and-forth global motion between frames (stabilization check) | RMS amplitude (1 px ≈ 1.5% of the frame) |

> **Calibration note**: these thresholds are **not yet calibrated against real-world footage**. The current policy is "prefer misses over false positives", with 2–3× headroom so normal photos are not flagged. Calibrate on a real media set before a formal release.

### 11.2 Presentation

- **Card badges**: a `.qc-stack` in the top-right corner of each thumbnail, one label per detected issue (corrupted / duplicate / blank / dirt / blur / exposure / noise).
- **Lightbox boxes**: in the lightbox, lens-dirt is drawn as translucent boxes over the actual spots; press **H** to hide/show them (the preference is remembered).
- **Issue list CSV**: the "issue list" button on the download page exports `qc-report.csv` listing corrupted / duplicate / blank / lens-dirt / blur / under- and over-exposed files (this folder and subfolders); opens directly in Excel.

### 11.3 Performance and hardware

- Frames are extracted by ffmpeg (a single frame for stills); detection runs on the CPU.
- Video decoding can use hardware acceleration (`cuda` / `qsv` / `d3d11va`) for NVDEC frame extraction, falling back to software decoding; detection values are unaffected. Hardware probing results are cached across starts.
- Source files stay read-only; QC never writes, moves, or deletes originals.

## License

AGPL-3.0 (GNU Affero General Public License v3.0). See [LICENSE](./LICENSE).

Third-party components: `ffmpeg` / `ffprobe` ([FFmpeg](https://ffmpeg.org/)) embedded at build time via `go:embed`. They are distributed under their own licenses and are not shipped with this repository — provide them yourself.
