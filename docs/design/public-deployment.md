# 安全挂公网的部署方案

> 适用版本：v1.3.1+（含前后端分离开关与内置认证）
> 前置认知：从 v1.3.0 起后端自带账号认证（`-public -auth` / `-auth-file` / `-setup`，见 3.5），**不是**只能靠反向代理挡。但认证只解决"谁能进来"，传输加密、防爆破、限流仍应由反向代理兜一层；`-public` 模式启动时也会强制检查这一点。反过来，**不开认证就把服务暴露到公网 = 把素材库交给全互联网**，这条底线没变。

---

## 1. 威胁模型（先认清边界）

| 资产 | 暴露方式 | 现状 |
| --- | --- | --- |
| 原始素材（图片/视频） | `GET /api/media`、`/api/vtrans` | **无鉴权**，只要连得上就能读 |
| 审阅标记 | `POST /api/review*` | 无鉴权（靠认领锁） |
| 打包下载 / 导出 | `/api/zip`、`/api/export`、`/api/copy` | 下载口令（Bearer / cookie / `?t=`） |
| 目录枚举 / 改根目录 | `/api/browse`、`/api/scan` | 仅回环，或 `-allow-remote-admin` + 管理口令 |

结论：**公网部署必须有一层身份认证**——要么用本项目内置的 `-public` 认证（见 3.5），
要么交给前置反向代理（第 3/5 节）。仅靠"下载口令"保护不了浏览与标记。

---

## 2. 推荐架构

```
Internet
   │  HTTPS (443)
   ▼
[Nginx / Caddy]  ← TLS 终止 + 身份认证（Basic Auth 或 OAuth2-Proxy）
   │  HTTP（内网 / docker network）
   ▼
[medreview -api-only -cors https://review.example.com]
   │                      │
   ├─ /media（只读挂载）   └─ /data（db/cache/log，持久卷）
```

要点：

1. 后端**只监听内网**（`-addr 127.0.0.1:8080` 或 docker 内网），不映射公网端口。
2. 前端可以与后端同域（Nginx 同时托管 `web/`），这样**同源、无 CORS**，最省心。
3. 若前端必须放别处（CDN/对象存储），再启用 `-cors` 白名单。

---

## 3. Nginx 配置（同域托管前端 + 反代 API）

> **可直接用的完整配置**：仓库里 [`packaging/nginx/medreview.conf`](../../packaging/nginx/medreview.conf)
> 已经包含 SSE 专用 `location = /api/events`、大文件下载、两种部署形态，改三处即可上机。

```nginx
server {
    listen 443 ssl http2;
    server_name review.example.com;

    ssl_certificate     /etc/letsencrypt/live/review.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/review.example.com/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;

    # —— 身份认证（二选一）——
    # A. 简单：Basic Auth（htpasswd -c /etc/nginx/.htpasswd alice）
    auth_basic           "medreview";
    auth_basic_user_file /etc/nginx/.htpasswd;
    # B. 推荐：OAuth2-Proxy / Authelia（见第 5 节），把上面两行换成 auth_request

    client_max_body_size 0;              # 不限制上传（导出/导入大文件）
    proxy_read_timeout   3600s;          # 大 zip 打包 + SSE 长连接

    # 前端静态资源
    root /var/www/medreview;             # web/ 目录内容
    index home.html;

    # API 反代
    location /api/ {
        proxy_pass         http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header   Host              $host;
        proxy_set_header   X-Real-IP         $remote_addr;
        proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;

        # SSE（/api/events）必需
        proxy_buffering    off;
        proxy_cache        off;
        chunked_transfer_encoding on;
    }
}

server {
    listen 80;
    server_name review.example.com;
    return 301 https://$host$request_uri;
}
```

> 后端启动参数（同域形态）：
> `medreview -api-only -addr 127.0.0.1:8080 -root /media -db /data/medreview.db -cache /data/cache -log /data/medreview.log`
> 同域不用配 `-cors`。

---

## 3.5 内置认证模式（不必上反向代理）

v1.3 起自带一层"够用"的认证，适合"就要一个能挂公网的独立服务"：

```bash
medreview -public \
  -auth alice:'你的密码' \
  -session-key "$(openssl rand -hex 32)" \
  -audit-log /data/audit.jsonl \
  -tls-cert /etc/letsencrypt/live/review.example.com/fullchain.pem \
  -tls-key  /etc/letsencrypt/live/review.example.com/privkey.pem \
  -root /media -db /data/medreview.db
```

行为：

| 项 | 说明 |
| --- | --- |
| 保护范围 | 所有页面与 `/api/*` 都要求登录；只放行 `/login.html`、`/api/login`、`/api/health` 和图标样式 |
| 会话 | HMAC-SHA256 签名的 Cookie（HttpOnly / SameSite=Lax；给了 TLS 就加 Secure），12 小时 |
| 登录限流 | 同一 IP 15 分钟内失败 5 次 → 429 |
| CSRF | 浏览器发起的写操作必须同源（Origin 校验；curl 等无 Origin 的客户端不受影响） |
| 权限 | 已登录 = 有下载权限、可用目录枚举/重扫（不用再配 `-admin-token`） |
| 审计 | `-audit-log` 追加 JSONL：时间、IP、用户、动作、结果、UA |

**账号怎么设**

```bash
# ① 单账号（最简单，密码只存在于进程内存）
medreview -public -auth alice:'你的密码' -session-key "$(openssl rand -hex 32)" ...

# ② 多账号：先生成 bcrypt 哈希（-hash-pass - 从 stdin 读，避免进 shell 历史）
medreview -hash-pass -
# 输入密码 → 输出形如 $2a$10$xxxx...

# 写成 users.json（只放哈希，不放明文）
cat > users.json <<'JSON'
{"users":{"alice":"$2a$10$...","bob":"$2a$10$..."}}
JSON
chmod 600 users.json

medreview -public -auth-file users.json -session-key "$(openssl rand -hex 32)" ...
```

Windows 下注意引号：`.\medreview.exe -public -auth 'alice:p@ss' -session-key '0123...'`；
多账号同样先跑 `.\medreview.exe -hash-pass -`，再把哈希写进 `users.json`。

- 改账号或密码：改文件/参数后**重启**服务
- 固定 `-session-key`：重启后已登录的浏览器不会被踢；不固定则每次启动随机
- `users.json` 权限建议 600；它只含哈希，仍然不要提交到仓库

**局限（想清楚再上）**：进程内计数（重启清零）；没有 2FA / 密码找回 / 用户管理界面；
会话密钥不指定就每次启动随机（重启要重新登录）。需要这些能力时，还是上 Authelia / OAuth2-Proxy。

**强烈建议**：即使有内置认证，也把 `-public` 与 `-tls-cert/-tls-key` 一起用，别用明文 HTTP 传密码。

### 3.1 SSE 不生效的排查顺序

仪表盘是**纯推送**的（页面不轮询，只显示"已更新 X 秒前"）。数字不动时按这个顺序查：

1. **直连后端**：浏览器打开 `http://后端:8080/admin.html`，数字会动 → 问题在代理层。
2. **Nginx 是否关了缓冲**：`/api/events` 必须有 `proxy_buffering off;`、`proxy_cache off;`、`gzip off;`。
3. **HTTP 版本**：`proxy_http_version 1.1;` + `proxy_set_header Connection "";`（缺前者会退化成短连接）。
4. **超时**：`proxy_read_timeout` 默认 60s，SSE 会被周期性掐断；设 3600s。
5. **看响应头**：`curl -N -H 'Accept: text/event-stream' https://域名/api/events` 应看到
   `Content-Type: text/event-stream`、`Cache-Control: no-cache`、`X-Accel-Buffering: no`，
   并且每 25 秒有一行 `: ping`。
6. **CDN / 云 WAF**：Cloudflare 等会缓冲或超时，需要关闭对应缓冲或走直连。
7. **前端跨源**：`EventSource` 无法附带 `Authorization`，跨源部署时前端会自动降级为定时拉取；
   想用 SSE 就让前后端同源（推荐）或由代理注入认证。

页面自身也有兜底：60 秒收不到任何推送会自动重连（`EventSource` 断线重连 + 主动重建）。

## 4. 前后端不同域（前端放 CDN / 另一台机器）

```bash
medreview -api-only -addr 0.0.0.0:8080 \
  -cors https://review.example.com \
  -cors-lan=false \
  -admin-token "$(openssl rand -hex 16)" \
  -root /media -db /data/medreview.db
```

前端页面首次打开后，点工具栏「服务器地址…」填 `https://api.example.com`（或直接带 `?api=https://api.example.com` 打开一次，会记住）。

注意：

- `-cors` 必须写**完整 origin**（协议+域名+端口），不支持通配符；`-cors-lan=false` 关掉私网放行。
- 跨源下载无法用 cookie 鉴权：要么在设置页填下载口令（走 `Authorization: Bearer`），要么给后端加 `-allow-query-token` 用 `?t=`（会进浏览器历史，不推荐）。
- 更稳的做法仍是**同域反代**（第 3 节），把跨源限制绕开。

---

## 5. 身份认证选型

| 方案 | 成本 | 适合 |
| --- | --- | --- |
| **Basic Auth** | 5 分钟 | 个人/小团队，HTTPS 下可接受 |
| **OAuth2-Proxy**（Google/GitHub/企业 SSO） | 30 分钟 | 需要账号体系、审计 |
| **Authelia** | 1 小时 | 自建 2FA、多应用统一门户 |
| **WireGuard / Tailscale**（不上公网） | 20 分钟 | **最安全**：根本不暴露端口，只给设备组网 |

> 如果只是自己和家人用：**Tailscale 直连 + 不开公网** 比任何反代鉴权都安全；公网方案是给"必须用浏览器直接访问"的场景。

---

## 6. 加固清单（上线前逐项打勾）

- [ ] 后端只监听内网 / 容器内网，公网只开 443
- [ ] TLS 证书有效（Let's Encrypt 自动续期）
- [ ] 代理层有身份认证（Basic/OAuth2/Authelia 任一）
- [ ] `-cors` 只列必要域名，`-cors-lan=false`（跨域部署时）
- [ ] `-admin-token` 随机 32 hex，且不写进前端代码
- [ ] 素材目录**只读**挂载（`-v /srv/photos:/media:ro`）
- [ ] 容器以非 root 运行（镜像已建 `medreview` 用户）
- [ ] `/data` 定期备份（db 是审阅结论，丢了要重标）
- [ ] Nginx `limit_req` 限制 `/api/dl-login`（防口令爆破）
- [ ] 日志脱敏：不要公开 `medreview.log`（含下载口令与绝对路径）
- [ ] 关闭 `-open-dl`、`-allow-query-token`（除非明确需要）
- [ ] 监控：`/api/health` 探活 + 磁盘容量告警

示例限流：

```nginx
limit_req_zone $binary_remote_addr zone=dllogin:10m rate=5r/m;

location = /api/dl-login {
    limit_req zone=dllogin burst=3 nodelay;
    proxy_pass http://127.0.0.1:8080;
}
```

---

## 7. docker-compose 示例

```yaml
services:
  medreview:
    image: medreview:latest
    restart: unless-stopped
    command: ["-api-only", "-addr", "0.0.0.0:8080", "-root", "/media"]
    volumes:
      - /srv/photos:/media:ro
      - medreview-data:/data
    expose: ["8080"]           # 不映射到宿主机公网

  caddy:
    image: caddy:2
    restart: unless-stopped
    ports: ["443:443", "80:80"]
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy-data:/data
      - /var/www/medreview:/srv/web:ro
    depends_on: [medreview]

volumes:
  medreview-data:
  caddy-data:
```

Caddyfile（自动 HTTPS，比 Nginx 少一半配置）：

```
review.example.com {
    basic_auth {
        alice $2a$14$...   # caddy hash-password
    }
    root * /srv/web
    file_server
    reverse_proxy /api/* medreview:8080 {
        flush_interval -1      # SSE
    }
}
```

---

## 8. 已知限制（公网场景）

1. **审阅页无登录态**：代理层认证通过后，所有访问者共享同一份"批注名"空间（靠名字区分）。
2. **下载口令仍是单一口令**：多人使用时建议每人用"分享链接"（一次性/限次），而不是共享主口令。
3. **无审计日志**：谁在什么时候标了什么，只有 `reviewer` 名字，没有 IP/时间线审计。
4. **不适合暴露给完全不可信的公网**：即使有代理鉴权，素材读取仍无二次校验；敏感素材建议只走 VPN。

---

## 9. 最小可用路径（ checklist 版）

1. 服务器装 Docker → `docker compose up -d`
2. Caddy/Nginx 配好域名 + Let's Encrypt + Basic Auth
3. 后端 `-api-only` + `-cors https://你的域名`（同域则省略）
4. 浏览器打开 → 「服务器地址…」填后端（同域跳过）→ 选素材目录（需 `-allow-remote-admin` 或本机操作）
5. 用手机 4G 访问一次，确认 HTTPS + 认证 + 图片能看
6. 打开 `/api/health` 确认探活；把 `/data` 加进备份
