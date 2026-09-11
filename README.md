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

开机自启：把下面这段存成 `/etc/systemd/system/nodedata.service`，然后
`systemctl daemon-reload && systemctl enable --now nodedata`。
（仓库里只有 Go 与 HTML，不提供安装脚本；这段与上面那一行命令的限制完全相同。）

```ini
[Unit]
Description=nodedata — single-host deviation monitor
After=network.target

[Service]
ExecStart=/usr/local/bin/nodedata serve --port 8888 --data-dir /var/lib/nodedata
Restart=always
RestartSec=5
CPUQuota=20%
MemoryMax=300M
Nice=10
IOSchedulingClass=idle
Environment=GOMAXPROCS=1
Environment=GOMEMLIMIT=200MiB

[Install]
WantedBy=multi-user.target
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
- **采集维度**：整机 + 每块整盘（`disk.util@nvme0n1` 等）+ 每个网卡（`net.rx_drop@eth1` 等）
  + 每个进程（CPU、块设备读写、主缺页、RSS 与 1 小时增长、状态）。整机磁盘汇总不重复计 dm/md，
  整机网络只汇总物理/virtio 网卡（不重复计 bond 与 VLAN）。
- **长期层**：每 5 分钟从原始点里抽一个（不求平均，保证 Δ 的分布不变），保留 56 天，
  追加写到 `history/YYYY-MM-DD.jsonl`（每天约 0.7MB，56 天约 40MB），重启时读回。
  L6–L14（3 小时 ~ 28 天）和 7d/30d 窗口用它。
- 为什么 56 天：σ 用最近 28 天的同时段 Δ 估计，L14 的每个 Δ 又要往回够 28 天。
  首次部署后 L9 约 1 天就绪、L14 约 29 天就绪，满 56 天后 L14 的参照才完整。

## 它会告诉你什么

L4 把偏离按症状归成 CPU / IO / 内存 / 网络四类，每类一条结论，写明**责任方**和**下一步命令**：

```
CPU 劣化：cpu.user 上升 6.0σ — 责任方 burner（PID 4242）
  责任方 burner PID 4242  占 150% 核，占整机忙碌的 41%；在这次变化开始之后才出现，是首要嫌疑
  下一步 top -H -p 4242 · pidstat -u -t -p 4242 1 5 · cat /proc/4242/status · perf top -p 4242
```

- 责任方排序：自身序列也偏离的进程 > 变化开始后才出现的进程 > 此刻用得最多的进程（会注明"未必是原因"）。
  常年 200% 的数据库不会因为一直最大就被指认。
- IO 同时给出设备（按盘的 util/延迟）和进程（/proc/PID/io 块层读写），并列出 D 状态进程。
- 内存看 1 小时 RSS 增长、主缺页；slab 领头时指认内核而不是进程。
- 虚机上 `cpu.steal` 领头时指认宿主机，不冤枉本机进程。
- 网络只到接口（按进程拆流量需要 eBPF，未做）。
- 责任方是 nodedata 自己时会明说。
- 结论要求偏离在最近 3 个采样点（15 秒）持续，单点噪声不下结论。

## 事故留证

后台每 15 秒跑一次 L4（不需要打开页面）。出现带责任方的结论时，把现场存到
`<data-dir>/incidents/<时间>-<类别>.json`：结论与完整诊断链、|z|≥2 的指标、进程快照、
责任进程的线程级 CPU（1 秒采样）与 wchan/内核栈、全部 D 状态进程及其栈、最近 100 条内核日志、
原始 pressure/meminfo/vmstat/diskstats/net 文件。同一责任方 30 分钟内只留一次，保留最近 100 份。
不采集进程命令行（参数里常有密码）。页面上可以"立即留证"。

## 故障语料库

- **回放**（每次 CI）：`go test -run TestFaultCorpus -v ./cmd/nodedata`。8 个物理机/虚机场景
  （新失控进程、老进程变异、nodedata 自身、单盘写入、3 小时内存泄漏、网卡丢包、虚机 steal、安静的一天），
  断言类别与责任方，报告出结论耗时和误报率。
- **真实注入**（实验机）：`./faultlab --url http://127.0.0.1:8888 --scenarios cpu,io,mem`，
  nodedata 需先运行约 10 分钟。网卡丢包：`sudo ./faultlab --scenarios net --iface eth0`（需要 tc 和该口上的 TCP 流量）。

## API

| 路径 | 说明 |
|---|---|
| `/` | 页面 |
| `/data/{1h,6h,24h,7d,30d}.json` | 窗口数据（含 `procs` 进程快照） |
| `/data/health.json` | 采集器自检，含 `self.cpu_pct`、`self.rss_mb` |
| `/api/check` | L0 绝对判定 |
| `/api/diagnosis?z=3` | L4 诊断链 |
| `/api/baseline` | GET / POST / DELETE 人工基线 |
| `/api/incidents` | GET 留证列表；POST 立即留证 |
| `/api/incidents/<id>` | 一份证据 |

## 编译

```bash
CGO_ENABLED=0 go build -ldflags "-X main.version=$(git describe --tags --always)" -o nodedata ./cmd/nodedata
```
