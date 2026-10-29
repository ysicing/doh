# doh

> 一个轻量的 DNS over HTTPS (DoH) 服务

## 特性

- 基于 Fiber v3
- 支持 `GET /dns-query?dns=...` 和 `POST /dns-query`
- 向可用上游并发查询，采用首个成功或 `NXDOMAIN` 响应
- 固定间隔健康检查，自动恢复故障上游
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

- `DOH_UPSTREAMS`: 上游 DNS 列表，格式为逗号分隔的 `IP:port`，IPv6 地址需加方括号
- `DOH_DEBUG_ENABLED`: 设为 `1` 时开启 `/debug/*`
- `DOH_DEBUG_TOKEN`: 调试接口必需的鉴权 token，使用 `Authorization: Bearer <token>` 或 `X-Debug-Token`
- `NODE_IP`: 用于 `/version` 和调试输出中的节点标识

`DOH_UPSTREAMS` 示例：

```bash
DOH_UPSTREAMS="8.8.8.8:53,1.1.1.1:53,[2606:4700:4700::1111]:53"
```

配置无效时启动失败，不会静默切换到公共上游。
旧配置中的 `|priority` 后缀需要移除；所有上游平等参与查询。

## 接口

- `GET /dns-query`
- `POST /dns-query`
- `GET /live`
- `GET /ready`：至少一个上游可用时返回 `200`，否则返回 `503`
- `GET /version`
- `GET /metrics`：保留 Fiber 系统监控面板

调试接口默认关闭，同时配置 `DOH_DEBUG_ENABLED=1` 和非空 `DOH_DEBUG_TOKEN` 后可用：

- `GET /debug/status`
- `GET /debug/cache`
- `POST /debug/check`
- `POST /debug/reset`

## 行为说明

- 仅接受 `application/dns-message` 的 DoH 请求/响应协商
- 只接受包含一个 Question 的普通 DNS 查询
- 每个上游每次查询一次，共用 3 秒总超时；首个有效结果返回后取消其他查询并关闭连接
- 上游返回 `SERVFAIL`、`REFUSED` 或网络错误时，等待其他上游结果；没有成功答案时透传已收到的 DNS 错误，全部发生网络错误时返回 HTTP `502`
- 连接失败或超时影响上游健康状态；正常收到的 DNS 错误响应不代表上游断连
- 每分钟检查所有上游，停止服务时取消检查；主动取消不计为上游故障
- 缓存键忽略域名大小写，区分 `QCLASS`、`QTYPE`、`DO`、`CD` 和 `RD` 位
- 带客户端 EDNS 选项或 TSIG 的消息绕过共享缓存
- 正向缓存按上游真实 TTL 生效，不再拉长短 TTL
- 负缓存仅缓存 `NXDOMAIN` 和带 `SOA` 的空答案
- 缓存最长保留 5 分钟，最多 10000 条，每条 DNS 消息长度不超过 4096 字节
- 缓存满时清理过期项；仍满则拒绝新 key，已有 key 可以更新；不再定时排序淘汰
- HTTP 出口统一处理响应大小，超过 4096 字节时按 DNS 协议截断；HTTP 响应不允许中间缓存
- 命中统计从进程启动或手动重置起累计，不再每周清零
- 调试接口未开启、没有 token 或鉴权失败时返回 `404`；代理头只用于日志和 IP 展示
- 调试状态移除优先级、权重和下次检查时间，使用 `latency_ms` 展示最近一次查询延迟
- 缓存摘要的 `capacity_usage` 表示条目容量使用率，`total_size` 表示 DNS wire 长度之和，不是堆内存占用
- 调试时间统一使用 RFC 3339，统计重置时间统一为 `last_reset_time`

## 代码结构

保持单个 Go 包，不引入额外的接口层：

- `main.go`：路由注册、启动与关闭
- `http.go`：DoH 请求、响应协商与调试鉴权
- `upstream.go`：配置解析、并发查询与健康检查
- `cache.go`：缓存准入、TTL 和统计
- `debug.go`：版本及运维接口

## 验证

```bash
GOMAXPROCS=2 go test -p=1 -parallel=2 ./...
GOMAXPROCS=2 go test -race -p=1 -parallel=2 ./...
GOMAXPROCS=2 go build -p=1 ./...
```
