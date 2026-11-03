# 安全挂公网的部署方案

> 适用版本：v1.2.0+（含前后端分离开关）
> 前置认知：medreview 的**审阅接口本身没有鉴权**（能看图才能标记），下载只靠口令。把它直接暴露到公网 = 把素材库交给全互联网。本文的方案核心是：**把鉴权交给反向代理，后端只监听内网**。

---

## 1. 威胁模型（先认清边界）

| 资产 | 暴露方式 | 现状 |
| --- | --- | --- |
| 原始素材（图片/视频） | `GET /api/media`、`/api/vtrans` | **无鉴权**，只要连得上就能读 |
| 审阅标记 | `POST /api/review*` | 无鉴权（靠认领锁） |
| 打包下载 / 导出 | `/api/zip`、`/api/export`、`/api/copy` | 下载口令（Bearer / cookie / `?t=`） |
| 目录枚举 / 改根目录 | `/api/browse`、`/api/scan` | 仅回环，或 `-allow-remote-admin` + 管理口令 |

结论：**公网部署必须在前置代理上加身份认证**，否则等于公开素材。仅靠"下载口令"保护不了浏览与标记。

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
    image: medreview:1.2.0
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
