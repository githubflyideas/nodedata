# nodedata Changelog

## [v3.0.8] — 2026-09-09

### 修复：L3 偏离度表整片顶格（算法错误）

页面上几乎每个指标每一档都是 ±6.00，等于什么都没说。根因是两个：

1. **z 少了位置项。** 公式写的是 `z = Δ / σ`，其中 `Δ = v(t) − v(t−H)`，
   等于假设"Δ 的正常值是 0"。这个假设对任何有稳定漂移的指标都不成立：
   网卡累计字节数、`disk.inflight`、page cache、slab 的 Δ 恒为一个大正数，
   于是 z 永远顶在 +6。现在改成标准化残差
   `z = (Δ − Center) / σ`，Center 是同时段 Δ 的历史中位数，也就是"平时就该涨这么多"。
2. **σ 的下限是编的。** 旧式 `σ = max(1.4826·MAD, 0.02·median|Δ|, N<20 ? 1e-6 : 1/6)`：
   `0.02·median|Δ|` 是 Δ 的量级而不是它的离散程度，代进去 z 恒等于 50；
   `1/6` 这个硬地板让整数型指标（进程 CPU jiffies 在 0/1 之间跳）动一格就恰好是 6.00。
   现在尺度用三个在正态下都收敛到 σ 的稳健估计取最大者
   （1.4826·MAD、四分位差/1.349、十分位差/2.563），完全退化时（速率恒定的计数器、
   恒 0 指标）落到「分辨率」与「量级的 5%」取大者，并统一以分辨率为地板 ——
   整数指标动一格 ⇒ z≈1，动三格 ⇒ z≈3。取三者最大是为了兜住多峰分布
   （周期性抖动、定时任务、锯齿型缓存），MAD 只看中位数附近那一团，会把另一个峰当异常。

### 修复：σ 与 Δ 的配对容差不一致
`ComputeSigma` 里配 `v(t−H)` 传的容差是 `H` 本身 —— 对 L14 来说"28 天内任何一点
都算 28 天前的值"，而 `Z` 用的是固定 30 秒。σ 是松配对的尺度、Δ 是紧配对的量，
两者不是同一个分布，比出来的 z 无意义。现在两边共用
`LagTolerance(H) = clamp(H/50, 30s, 30min)`。

### 变更：样本不够就不出 z
`Ready` 从"历史跨度够"改成"历史跨度够**且**同时段差分样本 ≥ 8"，不够就画斜纹。
另外样本量不足时 z 按 `N/(N+8)` 向 0 收缩（N=8 ×0.5，N=20 ×0.71，N=200 ×0.96），
避免刚启动几分钟就给出满量程结论。

### 新增回归测试
- `internal/deviation/saturation_test.go`：恒速计数器正常时 |z|<1、速率×10 时 z≥4；
  整数指标跳一格 |z|≤2；恒 0 指标 z=0 但跳到 50 要报；样本不足返回 NaN；小样本收缩。
- `cmd/nodedata/heatmap_z_test.go`：13 个"长得像真实节点"的合成指标 + 1 个注入异常，
  跑完整的 Series → RefreshSigma → Build 链路，要求正常指标红格比例 < 2%
  且注入的异常必须被抓到（实测 0.05%，异常抓到 41 格）。

## [v3.0.7] — 2026-09-09

### 修复：进程运行约 5 分钟后必崩（严重）
`internal/deviation.SigmaTable` 内部是裸 map，由 sigmaLoop（5 分钟一次整表重写）
写入，同时被转储循环（30 秒）和 HTTP 处理器读取。Go 的 map 不是并发安全的，
第一次 RefreshSigma 落在读取上就会触发
`fatal error: concurrent map read and map write` —— 这是 runtime 级错误，
recover 不住，进程直接消失。v3.0.5 及更早版本都有这个问题，**必须升级**。
- 改为 `sync.RWMutex` 保护，map 的值改成数组指针，避免每次写入拷贝 14×24 的 σ 数组；
- 索引越界不再 panic，回落到 `LagSigma{Sigma:1e-6}`；
- 新增 `internal/deviation/race_test.go` 与 `cmd/nodedata/heatmap_race_test.go`，
  在 `go test -race` 下把 sigmaLoop 与 Build/DumpAll 并发跑；把锁去掉后这两个
  测试确实会报 DATA RACE，也就是说它们能拦住这个 bug 再次出现。

### 修复：累计型计数器造成的永久性误报
conntrack 丢弃、listen 溢出、OOM 击杀、内核 taint 都是自开机起单调累加/置位的，
按绝对值判定会导致"三个月前抖过一次"永远挂着告警。现在一律只判增量：
- 有人工基线时只对超过基线的新增量告警，没有基线时以本进程首次观测值为起点；
- 计数器回绕/重启（增量为负）自动重新起算；
- taint 是位图，只对基线之后新出现的位告警，已有位仅作说明；
- 累计值照实显示，不再单独构成告警（OOM 历史保留 1 级提示）。

### 新增：人工基线
- `POST /api/baseline` 把当前状态设为标准状态，`GET` 查看，`DELETE` 清除；
- 落盘在 `<data-dir>/baseline.json`，原子写入，重启后自动加载；
- 首页"绝对判定"下方有"把当前状态设为基线 / 清除基线"按钮。

### 修复：`-proc` 对 L0 无效
`-proc` 之前只传给了 L1 采集器，L0 仍在读真 `/proc`。现在 `runServe` 会调用
`check.SetRoots`，`check` 子命令也支持 `-proc / -sys / -data-dir`。

### 变更：首页 L0 不再折叠
41 项判定全部平铺展开，异常项排在前面，保留"只看异常项"开关。

## [v3.0.0-L0] — 2026-09-08

### Overview
L0 Sanity Check release: 39 absolute judgments independent of time-series infrastructure.
First stable layer of v3.0 architecture (L0-L4).

### New Features

#### L0: Sanity Check Subcommand
- `nodedata check [--timeout 5s]` runs 39 absolute judgments in ≤5 seconds
- No ClickHouse, no baselines, direct /proc/sys reads only
- Exit codes: 0=pass, 1=fail, 2=warn

**39 Checks**:
- Time/Sync (3): NTP, accuracy, timezone
- CPU (7): online CPUs, governor, turbo, IRQ balance, preemption, sched, RT
- Memory (8): fragmentation, swappiness, cache, compaction, OOM, zone reclaim, overcommit, dirty pages
- Disk/IO (8): queue depth, scheduler, throttling, IOPS, writeback, trim, RAID, journaling
- Network (6): NICs, MTU, offload, RX/TX buffers
- Filesystem (3): capacity, inodes, mount modes
- Conntrack (2): limit, timeout
- Socket (2): buffer memory, listen queue
- Errors (2): OOM kills, I/O errors

#### L1 Expansion Methods
- `CollectErrorSources()`: dmesg OOM, I/O errors, FS errors, conntrack full, network drops
- `CollectLinkConfig()`: per-NIC speed, duplex, MTU, offload flags
- `CollectSocketStates()`: TCP state breakdown (ESTABLISHED, TIME_WAIT, CLOSE_WAIT, LISTEN)

#### Deviation Layer (L3) Fixes (v0.2.0 sync)
- `ComputeSigmaV2()`: median-based MAD instead of mean baseline
- Proper sigma floor: median(values) / 6 when N ≥ 20
- `ZWithTolerance()`: interval-adaptive tolerance to prevent false positives
- Signed differences in MAD calculation (fixes T_LAG_01)

#### Deployment
- `install.sh v0.2.0`: dependency check, smoke test, systemd integration
- Support for --systemd, --port, --interval, --ch-port parameters
- Go 1.21+ requirement validation

### Fixed

- **T_LAG_01**: Sigma calculation was using absolute diffs instead of signed
  - Result: z-scores were 2x larger than should be
  - Fix: Use signed differences for proper MAD

- **T_LAG_02**: Sigma floor too small (1e-6) for zero-valued metrics
  - Result: z-scores explode when sigma near zero
  - Fix: Use median(values)/6 floor when N≥20

- **Z-score false positives**: Idle systems trigger spikes on noise
  - Result: False alerts on completely normal systems
  - Fix: Z-score tolerance inversely proportional to sampling frequency

### Documentation

- `ARCHITECTURE.md`: 5-layer overview, L0-L1 detail, design invariants
- `DESIGN.md`: zero-allocation strategy, 14-lag rationale, sigma reasoning
- `install.sh`: improved inline comments and error messages

### Technical

**Binaries**:
- `nodedata-check-amd64`, `nodedata-check-arm64`: L0 only (2.6M each)
- `nodedata-linux-amd64`, `nodedata-linux-arm64`: Full v2.x (L0+L1+backend)

**Code Quality**:
- 37 existing acceptance tests still passing (1 SKIP: T_LAG_09)
- Zero-alloc validation: ≤20 allocs per collection round
- Build: CGO_ENABLED=0, pure Go, static binary

### Performance

- L0 check: <1s (target ≤5s)
- Collection: ≤20 allocs/round (v0.2.0 baseline maintained)
- Binary size: 2.6M (amd64)

### Breaking Changes

- L0 now returns exit code 2 for warnings (was not applicable before)
- Sigma computation changed: old baseline calculations no longer apply

### Known Limitations

- L0 error checks require dmesg/journalctl (not available in all containers)
- Socket CLOSE_WAIT attribution still heuristic (needs tcp_diag for certainty)
- Network offload flags default to true (require ethtool -k for accuracy)
- L2 aggregation not yet implemented (planned for v3.1)
- L3 production deployment pending (deviation matrix operational, alerting TBD)

### Upgrade Notes

From v2.x:
- L0 check is opt-in: `nodedata check` does not interfere with existing `nodedata run`
- Deviation layer improvements are backward-compatible but may change alert thresholds
- Consider re-tuning per-metric floors after upgrade

### Contributors

- nodedata core team (v3.0 architecture design)
- v0.2.0 PRD from customer feedback (sigma fixes, deploy clarity)

### Next Steps

- v3.1: L2 aggregation (5-min windows, baseline comparison)
- v3.2: L4 diagnosis chain (64 states → 49 diagnoses)
- v3.3: Production hardening + monitoring dashboard

---

## [v2.x] — Previous

(See git history for v2.0.0 and earlier releases)

