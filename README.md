# doh

> 一个轻量的 DNS over HTTPS (DoH) 服务

## 特性

- 基于 Fiber v3
- 支持 `GET /dns-query?dns=...` 和 `POST /dns-query`
- 支持上游并发探测、健康检查和失败回退
- 支持正向缓存和基于 `SOA` 的负缓存
- 默认关闭调试接口，按环境变量显式开启

## 运行

### 本地运行

```bash
go run .
```

服务默认监听 `:65001`。

### 构建

```bash
task build
```

### Docker

```bash
docker build -t ysicing/doh .
docker run --rm -p 65001:65001 ysicing/doh
```

## 使用

```bash
q www.google.com @http://127.0.0.1:65001/dns-query | nali
```

## 配置

支持的环境变量：

- `DOH_UPSTREAMS`: 上游 DNS 列表，格式为逗号分隔的 `host:port|priority`
- `DOH_DEBUG_ENABLED`: 设为 `1` 时开启 `/debug/*`
- `DOH_DEBUG_TOKEN`: 调试接口鉴权 token；设置后需要 `Authorization: Bearer <token>` 或 `X-Debug-Token`
- `NODE_IP`: 用于 `/version` 和调试输出中的节点标识

`DOH_UPSTREAMS` 示例：

```bash
DOH_UPSTREAMS="8.8.8.8:53|99,1.1.1.1:53|98,[2606:4700:4700::1111]:53|98"
```

## 接口

- `GET /dns-query`
- `POST /dns-query`
- `GET /live`
- `GET /ready`
- `GET /version`
- `GET /metrics`

调试接口默认关闭，开启后可用：

- `GET /debug/status`
- `GET /debug/cache`
- `POST /debug/check`
- `POST /debug/reset`

## 行为说明

- 仅接受 `application/dns-message` 的 DoH 请求/响应协商
- 缓存键区分 `QCLASS`、`QTYPE` 和 DNSSEC `DO` 位
- 正向缓存按上游真实 TTL 生效，不再拉长短 TTL
- 负缓存仅缓存 `NXDOMAIN` 和带 `SOA` 的空答案
- 调试接口未开启时返回 `404`

## 验证

```bash
go test ./...
go build ./...
```
