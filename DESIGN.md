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

## 14-Lag Structure

**Why this structure**:
- 300s–2.4M seconds covers 5min to 28-day ranges
- L9+ must be 86.4k (24h) multiples for day-over-day comparison
- 14 lags provide dense coverage without explosion

**Table**:
```
L1: 300s (5min)      — micro transient
L2: 600s (10min)     — transient
L3: 1.2k (20min)     — short anomaly
L4: 2.4k (40min)     — medium anomaly
L5: 5.4k (90min)     — long anomaly
L6: 10.8k (3hr)      — daily macro 1
L7: 21.6k (6hr)      — daily macro 2
L8: 43.2k (12hr)     — daily macro 3
L9: 86.4k (24hr)     — day-over-day ★
L10: 172.8k (2d)     — 2-day pattern
L11: 345.6k (4d)     — 4-day pattern
L12: 604.8k (1w)     — weekly ★
L13: 1.2M (2w)       — 2-week pattern
L14: 2.4M (4w)       — monthly ★
```

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

