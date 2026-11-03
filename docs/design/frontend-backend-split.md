# 前后端分离方案（medreview）

> 目标：后端可以跑在任何一台机器上（Windows / Linux / macOS / ARM），前端可以跑在任何系统的浏览器里；同一套代码同时支持"同源一体"和"跨源分离"两种部署。
>
> 状态：方案稿，未实施。日期：2026-11-03

---

## 1. 目标与非目标

### 目标

| # | 目标 | 判定标准 |
| --- | --- | --- |
| 1 | 后端 headless、零 GUI 依赖 | 同一份二进制在 Windows/Linux/macOS 上启动即用（已有） |
| 2 | 前端纯静态、零构建链 | `web/` 目录可直接放到任意静态服务器 / 对象存储 / 本地文件打开 |
| 3 | 一份 API 契约 | 前端只依赖 `/api/*` 的 HTTP 语义，不依赖同源 |
| 4 | 部署形态可选 | ①一体 ②跨源静态 ③反向代理同源，三种都能跑 |
| 5 | 不回归 | 现有 8 套 harness（386 项）保持全绿 |

### 非目标

- 不引入 React/Vue/Vite 等构建链（保持原生 JS、零构建、可直接改）
- 不做账号体系 / 多租户（仍是"受信任网络"工具，安全模型见第 5 节）
- 不拆 Go 包结构（72 个文件同包是刻意选择，拆包是另一件事）

---

## 2. 现状差距（事实）

| 项 | 现状 | 分离后需要 |
| --- | --- | --- |
| API 地址 | `web/app.js` 有 `API_BASE`，但只用于 demo / `file://` / 预览面板；**大量 `/api/` 直接硬编码**（app.js 45 处、mobile.html 21 处、standalone.html 44 处） | 全部走统一封装 `U()`，`API_BASE` 来自配置 |
| CORS | 只放行本机来源（`localOrigin`），这是**有意的安全设计** | 可配置白名单，默认仍关闭跨源 |
| 静态资源 | `go:embed web` + `http.FileServer`，只能同源 | 支持 `-api-only`（不挂静态）与 `-web-root <dir>`（外置前端） |
| 管理接口 | `/api/browse`、`/api/scan` 仅回环 | 跨机前端用不了；需显式开关 + 独立管理口令 |
| 下载鉴权 | `?t=` query 或 `mr_dl` cookie | 跨源 cookie 受 SameSite 限制 → 统一支持 `Authorization: Bearer` |
| SSE | 同源 `EventSource('/api/events')` | 跨源需 CORS 预检放行 + 心跳保活 |
| 打包 | 一个二进制含全部 | 拆成 `server` 与 `web` 两个产物（一体化仍保留） |

---

## 3. 三种部署形态（同一份代码）

```
A. 一体（现状）
   浏览器 ──► medreview 二进制（API + 内嵌 web）         同源，零配置

B. 分离·静态托管（本次目标）
   浏览器 ──► 静态站点（web/ 放 Nginx / OSS / file://）
        └────► medreview 服务（只出 API）                 跨源，CORS 白名单

C. 分离·反向代理（推荐的"跨机"姿势）
   浏览器 ──► Nginx（同源 / 和 /api 都代理）
                 └────► medreview 服务（内网任意机器）     同源，免 CORS，可加 Basic Auth
```

> 结论：**C 是生产推荐**（安全、免 CORS、可加 TLS/鉴权）；B 用于"前端放 CDN/自己电脑、后端放 NAS"这类场景。

---

## 4. 技术改动清单

### 4.1 后端（P0）

| 改动 | 说明 |
| --- | --- |
| `-api-only` | 不注册静态路由；`/` 返回 204 或简单 JSON 提示 |
| `-web-root <dir>` | 优先用磁盘目录替代 `go:embed`（前端热更新不用重编二进制） |
| `-cors <origins>` | 逗号分隔白名单，如 `http://192.168.1.9:5000,https://review.example.com`；空=维持现状（仅本机）；`*` 允许但启动时打印大字警告 |
| `Authorization: Bearer <token>` | 与 `?t=` 等价；跨源场景首选（不依赖 cookie） |
| `/api/health` | 无需鉴权，返回 `{ok,version,api:1}`，供前端探测与版本校验 |
| `/api/v1/*` 别名 | 旧路径保留（内部转发），为未来破坏性变更留余地；响应加 `X-Medreview-Api: 1` |
| `-allow-remote-admin` | 默认关。开启后 `/api/browse`、`/api/scan` 允许非回环，但必须提供**独立管理口令**（`-admin-token`），且与下载口令分离 |
| SSE 心跳 | 每 25s 发 `: ping\n\n` 注释帧，防反代/浏览器空闲断连 |

### 4.2 前端（P0）

| 改动 | 说明 |
| --- | --- |
| `web/config.js` | `window.MEDREVIEW_CONFIG = { apiBase: "", token: "" }`；**优先级**：`?api=` 查询参数 → `localStorage` → `config.js` → 空（同源） |
| 统一封装 | 所有请求/资源走 `U(p)`、`api(p, opts)`；目标是 `grep -c '"\/api\/'` 为 0（除 `U()` 内部） |
| 设置入口 | 页面右上角"服务器"菜单：填地址 → 探测 `/api/health` → 校验 `api:1` → 存 localStorage 并重载 |
| 资源 URL | `<img>`/`<video>`/下载链接一律 `U()`；跨源时沿用现有 blob 抓取路径（CSP/并发闸门逻辑不变） |
| SSE 跨源 | `new EventSource(U('/api/events'))`；`onerror` 指数退避（1s→30s），恢复后提示一次 |
| 部署自检 | 页面启动时显示一行状态：`同源` / `跨源 → http://nas:8080` / `连接失败` |

### 4.3 打包与分发（P1）

- 产物拆分：`medreview-server-<os>-<arch>[.exe]`（约 250MB 含内嵌工具）+ `medreview-web-<ver>.zip`（约 200KB）
- 一体化包继续提供（现状不变），文档标明三种用法
- 可选 `Dockerfile`：`FROM debian:12-slim` + 二进制 + `-root /media -api-only`
- 前端可加 PWA manifest（顺手满足"添加到桌面"，含鸿蒙浏览器）

---

## 5. 安全模型变化（必须同步写进 README）

| 风险 | 变化 | 对策 |
| --- | --- | --- |
| 跨源后媒体接口无鉴权 | **暴露面随 CORS 放大**：任何能访问后端的浏览器/脚本都能读全部素材（现状本来就无鉴权，但仅限能连到端口的人） | 默认不放行跨源；跨机部署**必须**配反向代理鉴权（Basic Auth / VPN）或在受信任内网 |
| 下载口令跨源 | cookie 在 `SameSite=Lax` 下跨站不发送 | 跨源统一 Bearer；同源仍可用 cookie |
| 远端管理接口 | 一旦开启，远端可枚举磁盘、改素材根目录 | 默认关；独立 `-admin-token`；开启时启动日志大字警告 |
| 明文传输 | 分离后可能跨公网 | 文档强制建议 HTTPS 反代 |

> 一句话：**分离不会让工具变安全，反而更容易被误配成"把素材库挂公网"**。默认值必须保守（CORS 空、远端管理关），并把这句写进文档首页。

---

## 6. 分阶段实施

| 阶段 | 内容 | 改动面 | 验收 |
| --- | --- | --- | --- |
| **S1** | 前端 `U()/config.js` 改造 + 后端 `-api-only`/`-web-root` + `/api/health` | 中（约 110 处 URL） | 同源、`file://`、Nginx 反代三种形态可用；8 套件不回归 |
| **S2** | `-cors` 白名单 + Bearer token + 前端"服务器地址"设置页 | 小 | 新 harness 用例：跨源预检 200、非白名单 403、Bearer 下载 |
| **S3** | 打包拆分 + Dockerfile + 文档（双语 README）+ `check_split_deploy.py` | 小 | server/web 两产物在 Linux/macOS/Windows 各跑一次 |
| **S4（可选）** | Tauri/Electron 壳（前端独立桌面安装包）、PWA | 中 | 壳内 `apiBase` 可配 |

预计：S1 是主体工作量（前端 URL 收敛 + 后端两三个开关），S2 之后都是增量。

---

## 7. 验收标准（可测）

1. `web/` 三种打开方式全部可用：`file://`、`http://<static>:5000`、`http://<nas>/`（反代）
2. `grep -rn '"/api/' web/*.js web/*.html` 结果只出现在 `U()` 封装内部
3. CORS 矩阵：默认 → 跨源被挡；白名单 → 放行且 `Vary: Origin`；`*` → 放行并打印警告
4. 下载四出口（zip/export/copy/browse-dest）在 Bearer 与 query 两种鉴权下都通过
5. SSE 跨源连接 60 分钟不断（心跳验证）
6. 现有 8 套 harness 全绿 + 新增 1 套 `check_split_deploy.py`

---

## 8. 决策记录（2026-11-03，全部"要"）

| # | 决策 | 落地情况 |
| --- | --- | --- |
| 1 | 默认放行**私有网段**来源（10/8、172.16/12、192.168/16），公网域名仍须显式 `-cors` | ✅ 已实现（`-cors-lan`，默认 true） |
| 2 | 提供 **Docker 镜像** | ✅ `Dockerfile` + `.dockerignore`（slim 构建 + apt ffmpeg/ImageMagick） |
| 3 | 前端加 **PWA** | ✅ 已有 manifest/图标；本次给 review/home/download 补齐引用 |
| 4 | **Tauri 壳** | ⏳ 未实施：需要 Rust 工具链 + 各平台签名，属独立产物；等 S1-S3 稳定后单开一轮 |

## 9. 实施状态（2026-11-03）

已完成（本次提交）：

- 后端：`-api-only`、`-web-root`、`-cors`、`-cors-lan`、`-allow-remote-admin`、`-admin-token`、
  `Authorization: Bearer`、`GET /api/health`、`/api/v1/*` 别名
- 前端：`?api=` → localStorage → 同源的地址解析；工具栏「服务器地址…」；3 处裸 `/api/` 收口；
  PWA 引用补齐
- 测试：keep_scope 增加私网放行/公网拦截断言（96 项）
- 部署：`Dockerfile`、`docs/design/public-deployment.md`（公网安全方案）

未做：Tauri 壳、前端产物打包脚本（web 直接是产物，无需构建）、`/api/v1` 的响应头版本标识。
