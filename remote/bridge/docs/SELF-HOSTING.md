# 自托管 Salcara Hub 部署指南

Salcara Bridge 支持两种独立运行模式，不依赖官方云端中转站：

---

## 方案一：自托管 Hub 服务器

适合场景：家庭局域网、私有 VPS、企业内网——任何需要完整 Hub 功能但不想使用官方服务的情况。

### 编译

```bash
cd bridge
go build -o salcara-hub ./tools/salcara-hub
```

### 启动

```bash
# 本机验证（仅回环地址）
./salcara-hub -addr 127.0.0.1:8765

# 生产环境（HTTPS）
./salcara-hub -addr :8765 -tls-cert /path/to/cert.pem -tls-key /path/to/key.pem

# 受限模式（只允许指定 API 域名）
./salcara-hub -addr :8765 -mode restricted -bases "https://api.openai.com,https://api.anthropic.com" -tls-cert /path/to/cert.pem -tls-key /path/to/key.pem
```

### 在 Bridge 里配置

1. 打开 Salcara Bridge →「手机远程」。远程站点必须使用 HTTPS；HTTP 仅允许本机回环地址。
2. 为自托管 Hub 配置可信 HTTPS 域名，或将它放在正确配置的 HTTPS 反向代理之后。
3. 输入 HTTPS 地址并连接。Bridge 和手机对该站点各自保存独立设备凭据。
4. 电脑显示在线后生成二维码，再用手机扫描绑定。

Bridge 内置的 Hub 管理页可一键启动本机 Hub，但只绑定 `127.0.0.1` / `::1`。它不提供可直接扫码的局域网 HTTP 服务。需要手机访问时，必须由可信 HTTPS 入口转发到本机 Hub。

### 反向代理示例（Nginx）

```nginx
server {
    listen 443 ssl;
    server_name hub.example.com;

    ssl_certificate /etc/letsencrypt/live/hub.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/hub.example.com/privkey.pem;

    location /salcara-hub/ {
        proxy_pass http://127.0.0.1:8765;
        proxy_http_version 1.1;
        # SSE 需要关闭缓冲
        proxy_buffering off;
        proxy_cache off;
        proxy_set_header Connection '';
        proxy_set_header X-Real-IP $remote_addr;
        chunked_transfer_encoding on;
    }
}
```

### Docker

```dockerfile
FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY . .
RUN go build -o salcara-hub ./tools/salcara-hub

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root
COPY --from=builder /app/salcara-hub .
EXPOSE 8765
ENTRYPOINT ["./salcara-hub", "-addr", ":8765"]
```

```bash
docker build -t salcara-hub .
# 若由宿主机上的 HTTPS 反向代理转发，只绑定宿主机回环地址。
docker run -d -p 127.0.0.1:8765:8765 --name salcara-hub salcara-hub
```

---

## 局域网 / VPN 直连

Bridge 提供独立的 TLS 手机直连端口。用户在电脑端打开“手机远程”的直连入口后，Bridge 只在当前可用且已保存的局域网或 VPN 地址上监听，并生成带有 WSS 地址、一次性配对码和证书 SHA-256 指纹的二维码。手机扫码后会先校验证书指纹和主机名，再通过一次性配对码完成绑定；后续重连使用手机端安全存储的设备凭据。

直连通道和 Hub 配对是两套独立授权。直连命令经过明确的命令白名单、会话面和本机 Agent 授权检查；会话事件按会话绑定到发起该命令的手机连接，不能因为重连、撤销或 Bridge 重启而转入 Hub。撤销会立即断开当前连接、清理未使用的配对码，并保留 Hub 的手机配对不变。直连失败不会静默回退到 Hub。

当前直连地址依赖手机可达的局域网或 VPN 路径，不提供自动 STUN/ICE/TURN 公网穿透。若使用会中继流量的 VPN 或其他网络服务，流量是否经过该服务由其网络拓扑决定；Salcara 的“严格直连”路径本身不会替换为 Hub 或隐式中继。直连服务默认关闭，必须由用户主动生成直连二维码后才会启动。

---

## API 端点参考

### Hub 服务端

```
GET  /salcara-hub/v1/ping              — 发现/健康检查
POST /salcara-hub/v1/bridge/register   — Bridge 注册
GET  /salcara-hub/v1/bridge/stream     — Bridge SSE 命令流
POST /salcara-hub/v1/bridge/event      — Bridge 推送事件
POST /salcara-hub/v1/pair              — QR 配对
POST /salcara-hub/v1/pair/revoke       — 解除配对
GET  /salcara-hub/v1/phone/stream      — 手机 SSE 事件流
POST /salcara-hub/v1/phone/command     — 手机提交命令
POST /salcara-hub/v1/qr                — 生成配对二维码
GET  /healthz                          — 健康检查（设备数、连接数）
```

直连 WebSocket 端点为 `GET /direct/ws`，健康检查为 `GET /direct/ping`。端点只接受 TLS，禁止 URL 查询参数和明文 `ws://`；它不是 Hub API，也不接受 Hub 的二维码或设备凭据。手机端应使用应用内直连扫码入口保存的二维码数据，不要手工拼接地址或把 Hub 配对码用于直连。
