# nodedata — 整机偏离度快照

**单二进制 + ClickHouse 的轻量级主机性能偏离度监控工具。**

直接读取 `/proc`，零外部命令，14档滞后差分偏离度。

## 快速部署

### 0. 解压

```bash
tar xzf nodedata-v0.1.0-linux-amd64.tar.gz
cd nodedata-v0.1.0-linux-amd64
```

解压后目录结构：

```
nodedata          # 采集器二进制（静态，无依赖）
index.html        # 前端单页面
schema.sql        # ClickHouse 建表 SQL
Caddyfile         # Caddy 配置（监听 :8888，反代内置 HTTP）
README.md         # 本文件
```

> **caddy / clickhouse 不包含在压缩包内。** 需要自行下载并放到 PATH 或当前目录。
> - Caddy：https://caddyserver.com/docs/install
> - ClickHouse：https://clickhouse.com/docs/en/install

---

### 1. ClickHouse 建库

```bash
clickhouse-client --query "CREATE DATABASE IF NOT EXISTS nodedata"
clickhouse-client --database nodedata < schema.sql
```

### 2. 启动采集器

```bash
./nodedata \
  --web=.          \
  --dsn="clickhouse://localhost:9000/nodedata" \
  --interval=30s   \
  --addr=127.0.0.1:9701 \
  --wal-dir=/var/lib/nodedata
```

启动后会：
- 每 30 秒采集一次 `/proc`，写入 ClickHouse
- 同时将最近 1h/6h/24h/7d/30d 数据序列化为 `data/*.json`
- 在 `127.0.0.1:9701` 提供 HTTP API

### 3. 用 Caddy 对外暴露（可选）

```bash
# 让 Caddy 在 :8888 同时伺服 index.html 和 /api/*
NODEDATA_WEB=$(pwd) caddy run --config Caddyfile
```

浏览器访问 `http://<host>:8888` 即可看到热力图。

**不想装 Caddy？** 直接访问 `http://127.0.0.1:9701` 也可以（内置静态服务）。

---

## 配置参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--interval` | `30s` | 采样周期，范围 [10s, 300s] |
| `--web` | `.` | 前端文件根目录（含 index.html） |
| `--dsn` | `clickhouse://localhost:9000/nodedata` | ClickHouse 连接串 |
| `--addr` | `127.0.0.1:9701` | 内置 HTTP 监听地址 |
| `--wal-dir` | `.` | WAL 目录（断网时缓冲写入） |
| `--host` | `os.Hostname()` | 上报主机名 |
| `--no-serve` | `false` | 只写文件，不启 HTTP 服务 |

---

## API

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/heatmap?from=&to=` | 时间段热力图数据 |
| GET | `/api/detail?ts=` | 单时刻偏离排名 |
| GET | `/api/raw?metric=&from=&to=` | 单指标原始序列 |

---

## 编译

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o nodedata ./cmd/nodedata/
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o nodedata ./cmd/nodedata/
```

---

## 已知限制

- 滞后差分无法检测"一直是坏的"恒定故障（由 PSI 绝对阈值补偿）
- 未启用 taskstats 时短命进程不可见
- caddy / clickhouse 需单独安装，不打包进二进制
