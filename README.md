# nodedata — 单机偏离度监控

一个静态二进制，读 `/proc`，无外部依赖（不需要 ClickHouse / Caddy / 数据库）。
页面已内置在二进制里；只需要分发 `nodedata` 这一个文件。

## 运行：一行命令

临时跑（带资源硬上限，停掉或重启机器即消失）：

```bash
sudo systemd-run --unit=nodedata -p CPUQuota=20% -p MemoryMax=300M -p Nice=10 -p IOSchedulingClass=idle -p Restart=always --setenv=GOMAXPROCS=1 --setenv=GOMEMLIMIT=200MiB /usr/local/bin/nodedata serve --port 8888 --data-dir /var/lib/nodedata
```

看状态 / 日志 / 停止：

```bash
systemctl status nodedata ; journalctl -u nodedata -f ; sudo systemctl stop nodedata
```

常驻（开机自启，参数与上面那一行完全相同）：

```bash
sudo ./install.sh ./nodedata          # 或 sudo PORT=9000 ./install.sh ./nodedata
```

不要 systemd、只在前台看一眼：

```bash
GOMAXPROCS=1 ./nodedata serve --data-dir ./data
```

### 为什么是这些限制

| 设置 | 理由 |
|---|---|
| `CPUQuota=20%` | 正常开销约 2% 核（按实测外推：85 条序列、1000 个进程）；20% 是给页面访问留的余量，同时是硬上限 —— v3.0.8 那种 bug 再出现也只能用到 0.2 核而不是 1.5 核 |
| `GOMAXPROCS=1` | Go 1.24 及以前不感知 cgroup CPU 配额，多核机上会开满 P，突发后被 CFS 节流卡住 |
| `MemoryMax=300M` + `GOMEMLIMIT=200MiB` | 满 24h 原始层 + 56 天长期层、85 条序列约 72MiB；GOMEMLIMIT 让 GC 在内核 OOM 之前先收紧 |
| `Nice=10`、`IOSchedulingClass=idle` | 和业务抢资源时让路 |

cgroup v1 的机器（CentOS 7/8 默认）上 `MemoryMax` 不生效，改用 `-p MemoryLimit=300M`。

## 参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `--port` | `8888` | 监听 `0.0.0.0:<port>`，无鉴权 —— 页面会显示进程名与 PID，请用防火墙限制来源 |
| `--data-dir` | `./data` | 转储 `*.json`、`baseline.json`、`history/` |
| `--history-dir` | `<data-dir>/history` | 长期层落盘目录 |
| `--interval` | `5s` | 采集与 L0 巡检周期 |
| `--dump-interval` | `30s` | 页面数据转储周期 |
| `--proc` / `--sys` | `/proc` / `/sys` | 测试用的替代根 |
| `--web-root` | 空 | 从磁盘读 `index.html` 覆盖内置页面（前端开发用） |

## 数据怎么存

- **原始层**：每个采集周期一点，内存里保留 24 小时。L1–L5（5 分钟 ~ 1.5 小时）用它。
- **长期层**：每 5 分钟从原始点里抽一个（不求平均，保证 Δ 的分布不变），保留 56 天，
  追加写到 `history/YYYY-MM-DD.jsonl`（每天约 0.7MB，56 天约 40MB），重启时读回。
  L6–L14（3 小时 ~ 28 天）和 7d/30d 窗口用它。
- 为什么 56 天：σ 用最近 28 天的同时段 Δ 估计，L14 的每个 Δ 又要往回够 28 天。
  首次部署后 L9 约 1 天就绪、L14 约 29 天就绪，满 56 天后 L14 的参照才完整。

## API

| 路径 | 说明 |
|---|---|
| `/` | 页面 |
| `/data/{1h,6h,24h,7d,30d}.json` | 窗口数据（含 `procs` 进程快照） |
| `/data/health.json` | 采集器自检，含 `self.cpu_pct`、`self.rss_mb` |
| `/api/check` | L0 绝对判定 |
| `/api/diagnosis?z=3` | L4 诊断链 |
| `/api/baseline` | GET / POST / DELETE 人工基线 |

## 编译

```bash
CGO_ENABLED=0 go build -ldflags "-X main.version=$(git describe --tags --always)" -o nodedata ./cmd/nodedata
```
