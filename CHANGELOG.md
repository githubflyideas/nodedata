# nodedata Changelog

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

