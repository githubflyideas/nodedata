# nodedata v3.0 Architecture

## Overview

nodedata is a single-host performance deviation monitor built on a 5-layer architecture (L0-L4). Each layer is independent and can be deployed separately.

```
L4: Diagnosis Chain     (64 named states → 49 diagnoses)
L3: Deviation Analysis  (14-lag z-score + deviation matrix)
L2: Aggregation         (5-minute windows, alerting thresholds)
L1: Metrics Collection  (108-item system: CPU, memory, disk, network, errors)
L0: Sanity Check        (39 absolute judgments, /proc/sys only)
```

## Layer 0: Sanity Check (L0)

**Purpose**: First-impression health check independent of time-series data.

**Capability**: `nodedata check` — runs 39 absolute judgments in ≤5 seconds.

**No dependencies**: 
- No ClickHouse
- No statistical baselines
- No persistent state
- Direct `/proc/sys` reads only

**Exit codes**:
- 0: All pass
- 1: One or more failures (critical issues)
- 2: Warnings only (performance advisory)

**39 Checks across 9 categories**:

| Category | Count | Examples |
|----------|-------|----------|
| Time/Sync | 3 | NTP, accuracy, timezone |
| CPU | 7 | Online CPUs, governor, turbo, IRQ balance |
| Memory | 8 | Swappiness, page cache, OOM killer, overcommit |
| Disk/IO | 8 | Queue depth, scheduler, throttling, RAID |
| Network | 6 | NICs up, MTU, offload, RX/TX buffers |
| Filesystem | 3 | Root usage, inodes, mount modes |
| Conntrack | 2 | Table limit, timeout |
| Socket | 2 | Buffer memory, listen queue |
| Errors | 2 | OOM kills, I/O errors |

**Output format**:
```
✓ Category:       OK (passed/total)
⚠ Category:       WARN (passed/total)
  ⚠ ID: detail message
✗ Category:       FAIL (passed/total)
  ✗ ID: detail message

✓ All checks passed
```

## Layer 1: Metrics Collection (L1)

**Purpose**: Continuous /proc sampling of 108 metrics.

**Capabilities**:
- Core metrics: CPU, memory, disk, network (existing ~50)
- Error sources: dmesg OOM/I/O/FS, conntrack full, network drops (6)
- Link config: speed, duplex, MTU, offload per-NIC (12)
- Socket states: TCP breakdown (ESTABLISHED, TIME_WAIT, CLOSE_WAIT, LISTEN) (6)
- Resource utilization %: computed from raw metrics (additional)

**Data flow**:
```
CollectGlobal() → Sample[] → store.InsertJSON() → data/*.json (atomic)
                                              ↓
                                         ClickHouse (async)
                                         WAL (on CH failure)
```

**Zero-alloc design**:
- readFileNT(): syscall.RawSyscall6 with pre-cached null-terminated paths
- parseFloatBytes: no string conversion
- parseFieldsBuf: reused buffer
- Result slice pre-allocated at ~128 items

## Layer 2: Aggregation (L2)

**Purpose**: 5-minute windows, thresholds, alert formation.

**Expected**: Fold L1 metrics into 5-min summaries, compare against baseline.

## Layer 3: Deviation Analysis (L3)

**Purpose**: 14-lag lagged-difference detection.

**Calculation**:
- **Lag structure**: [300s, 600s, 1.2k, 2.4k, 5.4k, 10.8k, 21.6k, 43.2k, 86.4k, 172.8k, 345.6k, 604.8k, 1.2M, 2.4M]
- **Z-score**: z[i] = (current - baseline[lag_i]) / sigma[lag_i]
- **Sigma computation** (v0.2.0 fixed):
  - Use median (not mean) as baseline
  - MAD (median absolute deviation) with 1.4826 scaling
  - Floor = median(values) / 6 when N ≥ 20
  - Minimum 1e-6 to prevent z-score explosion
- **Tolerance**: interval-adaptive (1/√samples_per_hour) to avoid false positives on idle systems
- **Output**: deviation matrix [39 metrics × 14 lags] = 546 cells

**v0.2.0 fixes**:
1. Use signed diffs, not absolute (fixes T_LAG_01)
2. Sigma floor based on median, not fixed threshold (fixes T_LAG_02)
3. Z-score tolerance prevents false alerts (fixes alerting flap)

## Layer 4: Diagnosis Chain (L4)

**Purpose**: Correlate multi-metric state for root-cause inference.

**Expected**: 64 named states → 49 diagnoses with negation logic.

## Design Invariants

1. **No off-baseline %**: Report absolute + FromZero, never % of idle baseline
2. **Rate floor enforcement**: Every WorseUp metric has minAbs or is event counter
3. **Peak windows**: Use BMax + MinSamples≥30 alongside means to catch bursts
4. **Per-instance state**: SameInstance=true for disk/nic multi-condition checks
5. **Concurrent safety**: JSON writes via temp-file + atomic rename
6. **Service volatility**: Only track enabled+non-volatile systemd units

## File Layout

```
cmd/nodedata/
  main.go          # Entry: check subcommand dispatcher
  check/
    check.go       # L0: 39-check implementation
internal/
  collector/
    collector.go   # L1: metric collection, zero-alloc
    paths.go       # Pre-cached /proc paths
  deviation/
    deviation.go   # L3: 14-lag z-score + v0.2.0 fixes
  server/
    api.go         # HTTP API handlers
    dump.go        # JSON atomic write pattern
  store/
    store.go       # ClickHouse + WAL
    schema.sql     # Table schema
bin/
  nodedata-check-{amd64,arm64}   # L0 only
  nodedata-linux-{amd64,arm64}   # Full v2.x (L0+L1+backend)
tests/acceptance/
  acceptance_test.go             # 37 test cases
setup.sh / install.sh            # Deployment bootstrap
```

## Deployment

**Quick start**:
```bash
bash install.sh                    # Build + smoke test
bash install.sh --systemd          # + systemd service
```

**Manual**:
```bash
go build -o nodedata ./cmd/nodedata
./nodedata check                   # Run L0
./nodedata check --timeout 10s     # Custom timeout
```

**With systemd**:
```bash
sudo systemctl start nodedata
sudo systemctl status nodedata
journalctl -u nodedata -f
```

## Exit Codes

| Code | Meaning |
|------|---------|
| 0 | All L0 checks passed |
| 1 | One or more L0 failures (critical) |
| 2 | Warnings only (no failures) |

## Performance

- **L0 check**: <1s (target ≤5s)
- **Collection interval**: 1-300s configurable (default 30s)
- **Allocations/round**: ≤20 (v0.2.0 baseline)
- **Binary size**: 2.6M (amd64, CGO_ENABLED=0)

## Known Limitations

- L0 cannot detect stateful errors (needs dmesg/journalctl parsing)
- Socket state attribution (CLOSE_WAIT vs app bug) requires deeper TCP inspection
- Network offload flags require ethtool (not in /proc/sys) — currently defaults to true

## Next Steps

- L2 aggregation: 5-min windows + baseline comparison
- L3 production deployment: monitor deviation matrix, alert thresholds
- L4 diagnosis: implement 49 diagnoses with multi-metric correlation

