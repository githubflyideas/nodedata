# nodedata Changelog

## [v3.1.1] — 2026-09-11 · 首屏"关键指标对比"；留证按类别限流

### 首屏：关键指标对比
人话名称 + 当前值 + 1h / 6h / 12h / 1d / 3d / 7d 前同一时刻的值 + 变化百分比，按 CPU / 内存 / 磁盘 / 网络分组；
多块盘、多张网卡时逐设备各一行。历史值服务端精确取（24h 内原始层，更早 5 分钟长期层），
没有历史返回 null、页面显示"—"（不当作 0）；本机没有的指标整行不显示；
红色只标"往坏方向变化 ≥20%"（可用内存往下为坏）；当前与全部历史列都为 0 的逐设备行不显示
（实机上 vdb/vdc/vdd 长期空闲，四行 0 把表撑满）。`GET /api/compare`。
z 热力图与诊断链移到下方，回答"变化是否异常、谁干的"。

### 事故留证
- 限流由"全局 1 分钟"改为"同一类别 1 分钟"。实机注入发现：IO 故障先触发一份 CPU 证据（写入本身吃 CPU），
  全局限流挡掉了紧随其后的 IO 结论，真正的 IO 现场没留下；内存证据被推迟到检查窗口之外；
- 证据里空字段输出 `[]` 而不是 `null`；
- faultlab 核对证据时同时比对类别与 PID（此前只比 PID，把 CPU 类证据误算成 IO 场景通过）。

## [v3.1.0] — 2026-09-11 · 归因、留证、语料库（物理机 / 虚机）

### L1：采集维度与数值修正
磁盘与网络采集此前有多处**数值错误**（不是风格问题）：
- `/proc/diskstats` 字段错位一格：`disk.inflight` 实际是"累计 IO 毫秒"单调计数器；`disk.util` 读的是加权时间，
  且上一轮值从不保存 —— 几乎从不输出；整机 util 各盘相加，两块盘各 60% 就超过上限被丢弃；
- "名字以数字结尾就是分区"：`nvme0n1`、`dm-0`、`md0` 全被跳过，**NVMe 服务器没有任何磁盘指标**；
- `disk.merged` 用本次合并数减上次的 IO 数；
- `net.tx_drop` 实际是**接收包数**（`nums[11%10]`）；
- 只读前 8 个网卡，bond、成员口、VLAN 全部相加，绑定网卡的物理机流量翻倍甚至三倍。

现在：
- 每块整盘出 util / await_r / await_w / 读写 IOPS / 读写吞吐 / 队列（`disk.util@nvme0n1`）；整机汇总不重复计 dm/md，
  整机 util 取最忙那块盘，统一 0–100；
- 每个网卡出 rx / tx / 丢包 / 错包（`net.rx_drop@eth1`）；整机只汇总有 `/sys/class/net/<if>/device` 的网卡；
- 每个进程新增块设备读写（`/proc/PID/io`）、主缺页、状态（D）、1 小时 RSS 增长；
  快照 = CPU 前 10 ∪ 读写前 10 ∪ RSS 增长前 5 ∪ RSS 最大前 5 ∪ D 状态 ∪ 自身；新增 `proc.io.<名字>` 序列。

### L4：因果归因（图 2 那样的结论由 nodedata 自己给出）
- 偏离按症状归成 CPU / IO / 内存 / 网络，每类一条结论：标题、责任方、下一步命令；
- 责任方排序：自身序列在"领头指标偏离的档位"上也偏离 > 在变化开始后才出现 > 此刻用得最多（注明"未必是原因"）。
  必须按档位比较：失控的新进程跑 5 分钟后自己的 L1 就绪且平稳，只看"有无历史"会让归因翻回常年最大的进程；
- 虚机 `cpu.steal` 领头 → 宿主机；`slab` 领头 → 内核；网络只到接口；责任方是自己时明说；
- **结论要求偏离持续 3 个采样点（15 秒）**。约 360 个 z 格下单点 |z|≥3 纯靠噪声几乎每次评估都有一个：
  回放里出现过三次假的"IO 劣化：disk.await_r 3.0σ"。L3 热力图照样显示单点尖峰。

### 事故留证
后台每 15 秒跑 L4（不依赖页面）；带责任方的结论出现时写 `<data-dir>/incidents/<时间>-<类别>.json`：
结论与诊断链、|z|≥2 的指标、进程快照、责任进程线程级 CPU 与内核栈、D 状态进程及其栈、最近 100 条内核日志、
原始 /proc 文件。同一责任方 30 分钟一次，保留 100 份；不采集命令行（防密码泄露）。`/api/incidents`，页面可"立即留证"。

### 故障语料库
- 回放（CI 每次跑）：8 个场景 8/8 通过；安静的一天 5 小时、601 次评估**零误报**；
- `cmd/faultlab`：实验机上真实注入 cpu / io / mem / net 故障，检验类别与 PID，报告耗时；
- `.github/workflows/ci.yml`：vet、race、回放语料库、性能护栏；手动触发时在 runner 上跑 faultlab。

### 已知问题（本版未改）
- **故障持续时基线被污染**：σ 每 5 分钟重算，包含正在发生的故障；稳健尺度用 10–90 分位差，
  故障占到一个小时桶约 10% 以上时短档 σ 被撑大、短档先被掩盖（长档仍能检出，结论不丢）。
  正确做法是结论活跃期间冻结相关指标的基线，并设最长冻结时间以便学习真实的新常态。

## [v3.0.10] — 2026-09-11

### 新增：长期层 —— L9–L14 与 7d/30d 窗口第一次真正可用
此前只有内存里 24 小时的原始点，重启即清空，ClickHouse 代码从未接入：L9–L14（1~28 天）
**永远是斜纹**，7d/30d 按钮实际只有 ≤24h 数据。
- 每 5 分钟从原始点里**抽**一个点（不求平均：抽出的仍是原始点，Δ 与原始数据同分布；
  平均会压小 σ、放大 z），保留 **56 天**，追加写 `<data-dir>/history/YYYY-MM-DD.jsonl`；
  启动时读回。85 条序列 56 天实测：磁盘 37MB，读回 1.05s；内存（含 24h 原始层）约 72MiB；
- 为什么 56 天而不是 28 天：σ 用最近 28 天的同时段 Δ 估计，而 L14 的每个 Δ 要往回够 28 天。
  只留 35 天时 L14 的参照只有 7 天，持续 3 天的劣化占参照 43%，中位数被拖走，
  劣化自己成了"正常"（测试里 L14 z=1.0，漏报）；56 天时同一场景 z=5.8；
- σ 取哪一层按规则而非拍脑袋：配 v(t−H) 的容差 ≥ 长期层步长一半时（L6=3h 起）用长期层，
  否则（L1–L5）用原始层。L6–L8 因此也不再因重启清零；
- 新增 `--history-dir`；
- 新增 `TestLongLagsSurviveRestart`：60 天历史落盘 → 重启只从磁盘装回 → L9–L14 立即就绪、
  正常指标不报、"平稳后最近 3 天每天涨 5%"的指标在 L10–L14 上被抓到；
  `TestHistoryTruncatedLineAndRetention`：崩溃留下的半行跳过、NaN 不落盘、过期日文件删除。

z 的语义提醒：它比的是"这次的 Δ"与"同 lag 的 Δ 平时多大"。一个**一直**匀速上涨的指标
（每天都涨 1%）7 天 Δ 平时就是 +7%，z≈0 —— 长 lag 抓的是"行为变了"，不是"一直在涨"。
容量类问题（磁盘按固定速率写满）归 L0 的绝对判定。

### 移除：`nodedata check` 子命令
L0 在 `serve` 里每个采集周期都跑，结果在首页与 `/api/check`，独立子命令只剩误用
（`install.sh` 装的服务跑的就是它，跑完即退）。`check.PrintResults` 一并删除。

### 部署：一个文件、一行命令
- 页面用 `go:embed` 编进二进制，只需分发 `nodedata`；`--web-root` 仅用于前端开发覆盖；
- README 给出带资源上限的 `systemd-run` 一行命令；`install.sh` 重写为约 40 行，
  写出同样限制的常驻 unit（`CPUQuota=20%`、`MemoryMax=300M`、`GOMAXPROCS=1`、
  `GOMEMLIMIT=200MiB`、`Nice=10`、`IOSchedulingClass=idle`）；
- `--data-dir` 默认 `./data`，不再依赖 web 根目录推断；
- 删除 `setup.sh`（下载 ClickHouse、传 `serve` 根本不认的 `--dsn`）与 `Caddyfile`
  （反代到没人监听的 9701）；README 按现状重写。

### 安全：不再对外暴露当前目录
`/` 原来是 `http.FileServer(http.Dir(webRoot))`：带目录列表、监听 0.0.0.0，
webRoot 找不到 index.html 时回落到**当前目录** —— 在 /root 下启动即可从网络浏览 `/root/.ssh`。
现在 `/` 只返回首页，其他路径 404（`/l0.html` 301 到 `/`）。

## [v3.0.9] — 2026-09-11

### 修复：nodedata 自身吃掉 ~1.5 个核（严重，外部工具抓到 `nodedata-linux-` 150%）
根因是 L3 的 `Series.Lookup` 对 17280 点的缓冲区做**线性扫描**，而 `Build` 对窗口里
每个点、每个就绪 lag 都要 Lookup 一次。成本随缓冲区长度线性上涨，所以启动时正常、
跑满 24 小时后最重。实测（20 个指标、满 24h 缓冲）：

| | v3.0.8 | v3.0.9 |
|---|---|---|
| 一轮转储（5 个窗口） | 34.6 s | 0.08 s |
| σ 全表重算 | 2.7 s | 0.49 s |

转储周期是 30 s，旧实现一轮要 35 s，转储 goroutine 永不空闲（≈1 核）；页面每 5 s 调一次
`/api/diagnosis`，它每次又完整 Build 一个 1h 窗口，叠加后到 1.5 核。
- `Series.Lookup` / `Range` 改二分；拒收时间倒退的点以保证有序前提；
- `/api/diagnosis` 只算每个指标最后一个点（`HeatmapBuilder.Latest`）；
- σ 重算由每指标 14×24=336 遍全量扫描改为每 lag 一遍（双指针配 v(t−H)），
  每个桶只排一次序。结果与旧实现逐位一致（`TestComputeSigmaLagMatchesLegacy`）；
- 采集器路径审计 `openedPaths` 每次读都 append 且从不清理（一天 ~170 万条，
  内存与 GC 扫描随运行时长上涨），改为按 `/proc/<pid>/...` 归一的去重集合；
- 新增 `cmd/nodedata/perf_test.go` 回归护栏：满缓冲一轮转储 > 2 s 即失败
  （把旧 `series.go` 换回去，该测试以 34.6 s 失败）。

### 修复：L3 里找不到对应进程
旧的进程采集每轮从全部 PID 里**随机抽 100 个**，只输出 top-5 的 comm：
- 进程要好几轮才被抽中一次，增量是"距上次被抽中以来"的累计值，首次抽中时 prev=0，
  直接把进程一辈子的 CPU 当 5 秒增量报（`proc.cpu.systemd` 的偏离就是这么来的）；
- 序列时有时无，同时段差分攒不够，z 基本是斜纹；同名多进程同一时刻互相覆盖；
- 按空格切 `/proc/PID/stat`，comm 带空格时 utime/stime 取错字段；
- `kworker/u4:2-events_unbound` 每个变体一条新序列，序列数无界增长；
- 结果里没有 PID，从表格无法走到具体进程 —— nodedata 自己占 150% 也看不到自己。

现在：每轮扫全部 PID（一个 PID 一次 read，1000 进程 < 10 ms）；用 starttime 识别 PID 复用，
首次见到只记基线；以最后一个 `)` 为界解析 comm；内核线程名归一（`kworker/…` → `kworker`）；
CPU 达到 1% 核的名字进入跟踪集合（上限 32），之后每轮都出点（空闲出 0），空闲 1 小时退出；
单位统一为"占一个核的百分比"，与 `cpu.user` 相同。
- 转储 JSON 新增 `procs`：此刻 CPU 前 10 的进程（PID、comm、CPU、RSS），**nodedata 自身永远在列**；
- `health.json` 新增 `self.cpu_pct`、`self.rss_mb`、`collector.procs_scan_ms`；
- 24 小时无新点的序列连同 σ 一起回收（`Series.Prune`）。

### 页面
- L3 表所有列可点击排序（数值列先降序，文本列先升序，z 列按 |z|，空值垫底），
  排序状态在 5 秒自动刷新后保持；
- L3 表下方新增"此刻谁在用 CPU"：PID、进程、CPU、RSS 与下一步命令（点击复制），
  `proc.cpu.<名字>` 行直接标出对应 PID；
- 修正降采样：旧实现从第一个点起步长取点，24h 窗口里表格的"当前值"最多落后 6 分钟，
  现在保证最后一个点是最新点。

### 已知问题（v3.0.10 已修）
- ~~缓冲区只在内存里存 24 小时，L9–L14 永远不会就绪~~
- ~~`install.sh` 装的 systemd 单元跑的是 `nodedata check`~~

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

