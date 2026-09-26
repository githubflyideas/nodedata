# nodedata Design Decisions

## Zero-Allocation Requirement

**Why**: Single-host performance deviation monitor cannot afford GC pauses during sampling.

**How implemented**:
1. **readFileNT()**: syscall.RawSyscall6 + pre-cached null-terminated path buffers
   - Eliminates os.Open() file descriptor heap allocations (9 calls/round)
   - Saves ~18 allocs per collection cycle

2. **parseFloatBytes()**: Custom ASCII parser without string()
   - Avoids strconv.ParseFloat temporary string allocations

3. **splitFieldsBuf**: Reused buffer across collections
   - No slice header allocations per field

4. **Pre-allocated result slice**: make([]Sample, 0, 128)
   - Prevents append-driven reallocations

**Validation**: benchmark with `go test -bench . -benchmem` shows ≤20 allocs/round

## 9-Lag Structure (v5.18.0)

**One set of time points for the whole page** (`cmd/nodedata/timepoints.go`):
5m · 10m · 30m · 1h · 6h · 12h · 1d · 3d · 7d · 14d.
The comparison table (raw values) and the services table (present or not) use all ten.
The z-score table uses the first nine (`deviation.LagSeconds`); a test pins the two lists together.

**Why 7d is the longest lag**: a z at lag H needs 2×H of history — reach back H, then
estimate how large an H-change normally is. Retention is 14 days, so the longest lag is 7 days.
Tables that show raw values or presence need no statistics and can go to 14 days.

**Why these nine and not the old ten** (5m 10m 20m 40m 1.5h 3h 6h 12h 1d 7d):
- The old set was irregular (×2, ×2, ×2, ×2.25, ×2, ×2, ×2, ×2, ×7) and matched neither of the
  other two tables on the page.
- Short lags do not detect faster. A step change is visible at every lag immediately (now vs
  t−H differs for all H). Lags differ only in how long a change still counts as "new", and in
  that slow drift needs a lag long enough to accumulate. 20m/40m added columns, not detection speed.
- Onset precision ("started 40m ago" vs "1.5h ago") was never reliable: while a fault persists,
  its own samples inflate σ and mask the shorter lags.

**Table**:
```
L1: 300s     5m   — just now
L2: 600s     10m  — a moment ago
L3: 1800s    30m  — half an hour ago
L4: 3600s    1h   — an hour ago
L5: 21600s   6h   — earlier today
L6: 43200s   12h  — half a day ago
L7: 86400s   1d   — same time yesterday ★
L8: 259200s  3d   — a few days ago
L9: 604800s  7d   — same time last week ★
```

**Breadth** ("how many lags are abnormal") now tops out at 9. The "critical" threshold stays at
5 lags — same proportion as 5 of the old 10. Fault corpus: 8/8 after the change.

## Sigma Computation (v0.2.0 Fixed)

### Problem: v0.1.0 used absolute diffs + wrong floor

**Example**: CPU load [0, 0, 100, 0, 0, ...] every hour
- Mean = 20
- Mean(|diff|) vs actual changes → computed sigma=0.7413
- But z(100) = (100-0)/0.7413 > 6 threshold → false alert on normal pattern

### Solution: v0.2.0 uses median-based MAD

1. **Signed differences** (not absolute):
   - Preserves direction of change
   - Allows proper statistical properties

2. **Median (not mean)** as baseline:
   - Robust to outliers
   - Matches distribution better

3. **MAD (median absolute deviation)**:
   - sigma = 1.4826 × MAD
   - Correct constant from statistics

4. **Intelligent floor**:
   - When N ≥ 20: floor = median(values) / 6
   - For load=100, median ≈ 100/6 ≈ 16.67
   - Prevents z-score > 6 explosion

5. **Absolute minimum**:
   - 1e-6 safety floor for truly flat metrics

## Z-Score Tolerance

**Why**: Idle system still triggers occasional spikes (randomness).

**Solution**: Make tolerance adaptive to sampling frequency

```
tolerance = 1 / √(samples_per_hour)

1s interval:  samples_per_hour = 3600 → tolerance ≈ 0.017 (strict)
30s interval: samples_per_hour = 120  → tolerance ≈ 0.091 (lenient)
300s interval: samples_per_hour = 12  → tolerance ≈ 0.289 (very lenient)
```

Only report z > tolerance, avoiding false positives on noisy systems.

## Deviation Detection: Signed Differences

**Original bug** (v0.1.0):
```go
diff := math.Abs(v2 - v1)  // Lost sign → all positive
```

**Fixed** (v0.2.0):
```go
diff := v2 - v1  // Keep sign, compute MAD on signed diffs
```

**Why it matters**:
- Load oscillation [0, 100, 0, 100, ...] should not look smooth
- Signed diffs reveal true pattern: [-100, +100, -100, +100, ...]
- MAD on signed diffs captures real volatility

## LagReady Threshold

**Requirement**: 300 samples minimum before deviation detection
- At 1s interval: 300s = 5 minutes
- Ensures sigma stable before comparison

**Why not 30?** Rule of thumb: MAD unstable with N<20; use N≥300 for production

## Concurrent Safety

**Pattern**: Write via temp-file + atomic rename
```go
tmp := filepath.Join(dir, ".tmp")
ioutil.WriteFile(tmp, data, 0644)
os.Rename(tmp, target)  // Atomic on POSIX
```

**Why**: JSON dumps run async while reader polls data/*.json
- Prevents reader from seeing partial JSON
- No locks needed (OS guarantees rename atomicity)

## Error Source Tracking

**Categories**:
1. **Critical**: OOM kills, I/O errors, FS errors → stop immediately
2. **High**: Conntrack full, network drops → degradation imminent
3. **Medium**: Scheduler warnings, soft lockups → performance impact

**Severity logic**:
- Count + parse from dmesg/journalctl (last ~50KB = ~1 hour)
- Report count + last timestamp
- Alert if critical error count increases

## Socket State Attribution

**Challenge**: CLOSE_WAIT = app bug OR network cleanup?

**Solution**: Breakdown by:
- Active listeners (LISTEN): service count
- Established (ESTABLISHED): active connections
- Transient (TIME_WAIT): cleanup in progress
- Problematic (CLOSE_WAIT): potential resource leak

**Action**: Alert if CLOSE_WAIT > 100 or >10% of total

## Next Refinements

1. **L2 aggregation**: 5-min window folding → trend
2. **Baseline freshness**: Sigma recompute on schedule, decay old data
3. **Multi-metric correlation**: L4 diagnoses checking L1+L3 state
4. **Configuration**: Per-metric tolerance tuning

---

# 附录：运行细节（v5.25.1 从 README 移来，README 只保留怎么用）

#### 为什么是这些限制

| 设置 | 理由 |
|---|---|
| `CPUQuota=20%` | 正常开销约 2% 核（按实测外推：85 条序列、1000 个进程）；20% 是给页面访问留的余量，同时是硬上限 —— v3.0.8 那种 bug 再出现也只能用到 0.2 核而不是 1.5 核 |
| `GOMAXPROCS=1` | Go 1.24 及以前不感知 cgroup CPU 配额，多核机上会开满 P，突发后被 CFS 节流卡住 |
| `MemoryMax=300M` + `GOMEMLIMIT=200MiB` | 满 24h 原始层 + 14 天长期层、85 条序列约 72MiB；GOMEMLIMIT 让 GC 在内核 OOM 之前先收紧 |
| `Nice=10`、`IOSchedulingClass=idle` | 和业务抢资源时让路 |

cgroup v1 的机器（CentOS 7/8 默认）上 `MemoryMax` 不生效，改用 `-p MemoryLimit=300M`。

### 数据怎么存

- **原始层**：每个采集周期一点，内存里保留 24 小时。5 分钟到 1 小时这几档用它。
- **采集维度**：CPU / 内存 / 磁盘（含容量）/ 网络（含 UDP 缓冲区溢出）/ 套接字（TCP 状态分布、conntrack 水位）
  + 每块整盘（`disk.util@nvme0n1` 等）+ 每个网卡（`net.rx_drop@eth1` 等）
  + 每个进程（CPU、块设备读写、主缺页、RSS 与 1 小时增长、状态）。整机磁盘汇总不重复计 dm/md，
  整机网络只汇总物理/virtio 网卡（不重复计 bond 与 VLAN）。
- **长期层**：每 5 分钟从原始点里抽一个（不求平均，保证 Δ 的分布不变），保留 14 天，
  追加写到 `history/YYYY-MM-DD.jsonl`（每天约 0.7MB，14 天约 9MB），重启时读回。
  6 小时到 7 天这几档、以及对比表和服务表的"14天前"一列用它。
- **一套时间点，全页面共用**：5分钟 · 10分钟 · 30分钟 · 1小时 · 6小时 · 12小时 · 1天 · 3天 · 7天 · 14天。
  对比表（原始数值）和服务表（在不在）用全部 10 个；变化幅度用前 9 个——它要判断"这个变化平时有多大"，
  一档需要 2 倍的历史（往回够 H，再估计这一档平时的波动），14 天保留期最多撑到 7 天。
  首次部署后，"和 1 天前比"约 2 天后可用，"和 7 天前比"约 14 天后可用。

### 它会告诉你什么

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

### 事故留证

后台每 15 秒跑一次 L4（不需要打开页面）。出现带责任方的结论时，把现场存到
`<data-dir>/incidents/<时间>-<类别>.json`：结论与完整诊断链、|z|≥2 的指标、进程快照、
责任进程的线程级 CPU（1 秒采样）与 wchan/内核栈、全部 D 状态进程及其栈、最近 100 条内核日志、
原始 pressure/meminfo/vmstat/diskstats/net 文件。同一责任方 30 分钟内只留一次，保留最近 100 份。
不采集进程命令行（参数里常有密码）。页面上可以"立即留证"。

### 故障语料库

- **回放**（每次 CI）：`go test -run TestFaultCorpus -v ./cmd/nodedata`。8 个物理机/虚机场景
  （新失控进程、老进程变异、nodedata 自身、单盘写入、3 小时内存泄漏、网卡丢包、虚机 steal、安静的一天），
  断言类别与责任方，报告出结论耗时和误报率。
- **真实注入**（实验机）：`./faultlab --url http://127.0.0.1:8888 --scenarios cpu,io,mem`，
  nodedata 需先运行约 10 分钟。网卡丢包：`sudo ./faultlab --scenarios net --iface eth0`（需要 tc 和该口上的 TCP 流量）。

### 页面

绝对判定（L0）固定在最上面，其余分四个标签页：

| 标签页 | 内容 |
|---|---|
| 关键指标 | 25 条曲线（1h/6h/24h/7d/14d 可选）+ 诊断链 + 事故留证 |
| OS 指标 | 全部采集项：CPU / 内存 / 磁盘 / 网络 / 套接字 / 进程 |
| 整体偏离度和占用 | z 热力图 + 此刻谁在用资源（带下一步命令） |
| 服务对比 | 服务清单与历史在否 |

### 服务

页面上单独一个"服务"区块，回答"这台机器上跑着什么、各自什么时候起来的、昨天那个还在不在"。

识别**只读 /proc**：可执行文件名查表 → java 命令行（区分 ELK / Kafka / Tomcat）→
监听端口反查 → 都不认就显示可执行文件名。不连服务、不读配置、不执行任何命令。
把 mysqld 改名部署也能靠 3306 认出来。内置表覆盖 Nginx / Apache / Caddy / HAProxy /
MySQL / MariaDB / PostgreSQL / MongoDB / Redis / Memcached / ClickHouse / etcd /
Elasticsearch / Kibana / Logstash / Kafka / ZooKeeper / RabbitMQ / Docker / containerd /
BIND / CoreDNS / PHP-FPM 等。

每行给出：服务名与监听端口、进程名、PID、实例数、启动时间、已运行时长、CPU、内存，
以及 1h / 6h / 12h / 1d / 3d / 7d / 14d 前在不在（`✓` 在 / `—` 不在 / `?` 当时 nodedata 没在跑 /
`重启` 那时在但之后换过一次）。**消失的服务留在表里标灰**，不会凭空不见。

服务变化只呈现、不告警：我们分不清"挂了"和"运维手动停的"，分不清就不该报警。

### 给监控系统的接口

`GET /health.txt` 一行文本，关键字打头，主机名在同一行：

```
NODEDATA host=jp02-dns-01 status=WARN cpu=182% load=9.14 mem_avail=1.2GiB disk_util=96 \
  culprit=fl-io/236 reason="IO 劣化：disk.await_w 上升 6.0σ" ts=2026-09-12T03:10:51Z
```

```bash
curl -s http://127.0.0.1:8888/health.txt | grep -q '^NODEDATA .*status=NORMAL' || 告警
```

`status` 取 L0 绝对判定与 L4 归因结论中较严重者：`DOWN`（L0 有 fail）、
`WARN`（L0 有 warn，或 L4 给出了带责任方的结论）、`NORMAL`。异常时 `reason=` 说明原因，
`culprit=` 直接给出进程名/PID，派单时不用再登机器。
自由文本里的 `NODEDATA`、`status=`、换行与引号都会被中和 —— 进程名是攻击者可控的，
否则一个叫 `NODEDATA status=NORMAL` 的进程就能让监控的 grep 在真告警时匹配成功。

