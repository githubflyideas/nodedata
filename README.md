# nodedata — 整机偏离度快照

单二进制，读 `/proc`，14档偏离度，写 ClickHouse，前端热力图。

---

## 一键部署

```bash
tar xzf nodedata-v0.1.0-linux-amd64.tar.gz
cd nodedata-v0.1.0-linux-amd64
bash setup.sh
```

`setup.sh` 会自动完成：ClickHouse 下载 → 建库建表 → 启动 nodedata → 打印状态。

---

## 手动部署

### 1. 解压

```bash
tar xzf nodedata-v0.1.0-linux-amd64.tar.gz
cd nodedata-v0.1.0-linux-amd64
```

包内文件：

```
nodedata      — 采集器（静态二进制，无任何依赖）
index.html    — 前端热力图页面
schema.sql    — ClickHouse 建表 SQL
Caddyfile     — Caddy 反代配置（监听 :8888）
setup.sh      — 一键安装脚本
README.md     — 本文件
```

---

### 2. 准备 ClickHouse

**没有 ClickHouse？** 用官方单文件版（无需 root，无需安装）：

```bash
curl -L https://clickhouse.com/ | sh          # 下载 clickhouse 单文件
./clickhouse server --daemon                  # 后台启动，默认 TCP 端口 9000
```

启动后等约 3 秒，用以下命令确认连通：

```bash
./clickhouse client --query "SELECT 1"        # 输出 1 即正常
```

**想用其他端口（如 19000）？**

```bash
./clickhouse server --daemon -- --tcp_port=19000
# 启动 nodedata 时对应修改 --dsn
./nodedata --dsn="clickhouse://localhost:19000/nodedata" ...
```

---

### 3. 建库建表

```bash
./clickhouse client --query "CREATE DATABASE IF NOT EXISTS nodedata"
./clickhouse client --database nodedata < schema.sql
```

---

### 4. 启动 nodedata

```bash
./nodedata --web=. --wal-dir=.
```

nodedata 启动时会**主动检测并打印状态**，例如：

```
────────────────────────────────────────────────────────
  nodedata — 整机偏离度快照
────────────────────────────────────────────────────────
  主机名      : myserver
  采集器地址  : http://127.0.0.1:9701
  前端目录    : .
  ClickHouse  : clickhouse://localhost:9000/nodedata
────────────────────────────────────────────────────────

[ClickHouse] ✓ 连接正常 (clickhouse://localhost:9000/nodedata)
[Caddy]      ℹ caddy 未运行（可选）
             → 若需对外暴露：NODEDATA_WEB=$(pwd) caddy run --config Caddyfile
[nodedata]   HTTP 服务启动 → http://127.0.0.1:9701
[nodedata]   前端页面      → http://127.0.0.1:9701/
```

ClickHouse 连不上时不会崩溃，数据写本地 WAL，恢复后自动重放：

```
[ClickHouse] ✗ 无法连接: dial tcp 127.0.0.1:9000: connection refused
[ClickHouse]   → 数据将写入本地 WAL，ClickHouse 恢复后自动重放
[ClickHouse]   → 检查 ClickHouse 是否启动：./clickhouse server &
[ClickHouse]   → 或指定其他端口：--dsn=clickhouse://localhost:PORT/nodedata
```

---

### 5. 对外暴露（可选：Caddy）

不需要 Caddy 也能用，直接访问内置服务即可：

```
http://127.0.0.1:9701        # 内置 HTTP，含前端和 API
```

如果要在 `:8888` 对外暴露（通过 Caddy 加 gzip、缓存等）：

```bash
# 需要先安装 Caddy: https://caddyserver.com/docs/install
NODEDATA_WEB=$(pwd) caddy run --config Caddyfile &
```

Caddy 启动后 nodedata 日志会变为：

```
[Caddy] ✓ 端口 8888 已监听，前端可访问 http://本机IP:8888
```

---

## 全部参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--dsn` | `clickhouse://localhost:9000/nodedata` | ClickHouse 连接串，**改端口在这里** |
| `--interval` | `30s` | 采样周期，范围 [10s, 300s] |
| `--web` | `.` | 前端文件根目录（含 index.html） |
| `--addr` | `127.0.0.1:9701` | nodedata 自身 HTTP 地址 |
| `--wal-dir` | `.` | WAL 目录（ClickHouse 断连时缓冲写入） |
| `--host` | os.Hostname() | 上报主机名 |
| `--no-serve` | false | 只写文件，不启 HTTP |

---

## API

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/heatmap?from=&to=` | 时间段热力图数据 |
| GET | `/api/detail?ts=` | 单时刻偏离排名 |
| GET | `/api/raw?metric=&from=&to=` | 单指标原始序列 |

---

## 从源码编译

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o nodedata ./cmd/nodedata/
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o nodedata ./cmd/nodedata/
```

---

## 已知限制

- 未启用 taskstats 时短命进程不可见（不影响主要指标）
- caddy / clickhouse 需单独安装，不打包进二进制
