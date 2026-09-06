# nodedata — 整机偏离度快照

**单二进制 + ClickHouse 的轻量级主机性能偏离度监控工具。**

直接读取 `/proc`，零外部命令，14档滞后差分偏离度。

## 部署

### 1. ClickHouse 建库

```bash
clickhouse-client < internal/store/schema.sql
```

### 2. 启动采集器

```bash
./nodedata \
  --web=/path/to/web \
  --dsn="clickhouse://localhost:9000/nodedata" \
  --interval=30s \
  --addr=127.0.0.1:9701 \
  --wal-dir=/var/lib/nodedata
```

### 3. 启动 Caddy（端口 8888）

```bash
NODEDATA_WEB=/path/to/web caddy run --config Caddyfile
```

**或者仅用内置 HTTP 服务（不需要 Caddy）：**
浏览器访问 `http://127.0.0.1:9701` 即可。

## 配置参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--interval` | `30s` | 采样周期 [10s, 300s] |
| `--web` | `.` | 前端文件根目录 |
| `--dsn` | `clickhouse://localhost:9000/nodedata` | ClickHouse 连接串 |
| `--addr` | `127.0.0.1:9701` | HTTP 监听地址 |
| `--wal-dir` | `.` | WAL 目录 |
| `--host` | `os.Hostname()` | 主机名 |
| `--no-serve` | false | 只写文件，不起 HTTP 服务 |

## 产物文件清单

```
nodedata          # 单一二进制
web/
  index.html      # 前端页面
  data/
    1h.json       # 静态转储（每30秒更新）
    6h.json
    24h.json
    7d.json
    30d.json
    health.json
Caddyfile         # Caddy 配置（端口8888）
clickhouse        # ClickHouse 二进制（不打包进 nodedata）
caddy             # Caddy 二进制（不打包进 nodedata）
internal/store/schema.sql  # ClickHouse 建表 SQL
```

## 编译

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o nodedata ./cmd/nodedata/
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o nodedata-arm64 ./cmd/nodedata/
```

## API

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/heatmap?from=&to=` | 任意时间段热力图 |
| GET | `/api/detail?ts=` | 单时刻偏离排名 |
| GET | `/api/raw?metric=&from=&to=` | 单指标原始序列 |

## 已知限制

- 采样间隙内的 gauge 类指标抖动不可见
- 滞后差分无法检测"一直是坏的"恒定故障（由 PSI 绝对阈值补偿）
- 未启用 taskstats 时短命进程不可见
