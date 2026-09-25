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

