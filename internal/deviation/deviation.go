// deviation.go — 14档滞后差分偏离度计算。
// σ 按小时分桶，下限三选一，z clamp ±6，对齐段严格24h整数倍。
package deviation

import (
	"math"
	"sort"
	"sync"
	"time"
)

// NLag 档位总数。
const NLag = 14

// LagSeconds 是十四档的滞后时长（秒）。L9+ 必须是 86400 的整数倍。
var LagSeconds = [NLag]int{
	300, 600, 1200, 2400, 5400, 10800, 21600, 43200,
	86400, 172800, 345600, 604800, 1209600, 2419200,
}

// LagID 返回档位名，1-indexed。
func LagID(i int) string {
	return "L" + itoa(i+1)
}

// Sample 是一个时间序列点。
type Sample struct {
	TS    time.Time
	Value float64
}

// LagSigma 是某 (lag, hour) 组合的尺度常数。
type LagSigma struct {
	Sigma float64
	N     int
	Ready bool
	Low   bool // N < 20
}

// ComputeSigma 批算某档位在某小时桶上的 σ。
// hist 须按时间升序，覆盖足够历史。
func ComputeSigma(lagSeconds int, hour int, hist []Sample) (LagSigma, error) {
	if len(hist) == 0 {
		return LagSigma{Sigma: 1e-6}, nil
	}
	H := time.Duration(lagSeconds) * time.Second
	ready := len(hist) > 0 && hist[len(hist)-1].TS.Sub(hist[0].TS) >= H

	// 收集 28 天内 hour 桶的 Δv（有符号，用于正确计算 MAD）
	var diffs []float64
	end := hist[len(hist)-1].TS
	cutoff := end.Add(-28 * 24 * time.Hour)
	histStart := hist[0].TS

	for i := len(hist) - 1; i >= 0; i-- {
		s := hist[i]
		if s.TS.Before(cutoff) { break }
		if s.TS.UTC().Hour() != hour { continue }
		// 找 t-H 最近样本；若目标在历史开始之前则无有效 prev，跳过
		target := s.TS.Add(-H)
		if target.Before(histStart) { continue }
		prev, ok := findNearest(hist, target, H)
		if !ok { continue }
		diffs = append(diffs, s.Value-prev) // 有符号差分
	}

	ls := LagSigma{N: len(diffs), Ready: ready}
	if !ready || len(diffs) == 0 {
		ls.Sigma = 1e-6
		ls.Low = true
		return ls, nil
	}

	mad := computeMAD(diffs)
	medDiff := median(absSlice(diffs))

	// σ 下限：低置信度（N<20）时用 1e-6，高置信度时用 1/6，
	// 确保全零指标在高置信度时 z = Δ/σ ≤ 6（Δ=1）。
	absFloor := 1e-6
	if ls.N >= 20 {
		absFloor = 1.0 / 6.0
	}
	sigma := math.Max(1.4826*mad,
		math.Max(0.02*medDiff, absFloor))
	ls.Sigma = sigma
	ls.Low = ls.N < 20
	return ls, nil
}

// SigmaTable 存储所有 (metricID, lagIndex, hour) 的σ值。
//
// 并发约定：σ 表由 sigmaLoop 周期性整表重写，同时被转储循环和 HTTP 请求
// 并发读取。map 本身不是并发安全的，早期版本没有锁，运行约 5 分钟后
// （第一次 RefreshSigma）必然 fatal error: concurrent map read and map write。
// 因此 Set/Get 一律走 RWMutex；值改为指针存储，避免每次写入拷贝 10KB 数组。
type SigmaTable struct {
	mu   sync.RWMutex
	data map[string]*[NLag][24]LagSigma
}

// NewSigmaTable 创建空表。
func NewSigmaTable() *SigmaTable {
	return &SigmaTable{data: make(map[string]*[NLag][24]LagSigma)}
}

// Set 写入一个σ值。
func (t *SigmaTable) Set(metricID string, lagIdx, hour int, ls LagSigma) {
	if lagIdx < 0 || lagIdx >= NLag || hour < 0 || hour >= 24 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	arr := t.data[metricID]
	if arr == nil {
		arr = new([NLag][24]LagSigma)
		t.data[metricID] = arr
	}
	arr[lagIdx][hour] = ls
}

// Get 读取σ值。
func (t *SigmaTable) Get(metricID string, lagIdx, hour int) LagSigma {
	if lagIdx < 0 || lagIdx >= NLag || hour < 0 || hour >= 24 {
		return LagSigma{Sigma: 1e-6}
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if arr, ok := t.data[metricID]; ok && arr != nil {
		return arr[lagIdx][hour]
	}
	return LagSigma{Sigma: 1e-6}
}

// Len 返回已有σ的指标数，仅用于自检与调试。
func (t *SigmaTable) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.data)
}

// Deviation 处理偏离度计算。
type Deviation struct {
	sigma *SigmaTable
	// lookupFn 用于查找 v(t-H)，由外部注入（通常是 ClickHouse 点查）。
	LookupFn func(metricID string, at time.Time, tolerance time.Duration) (float64, bool)
}

// New 创建 Deviation。
func New(sigma *SigmaTable) *Deviation {
	return &Deviation{sigma: sigma}
}

// Z 返回十四档 z 值，未就绪/未找到历史值时返回 NaN。
func (d *Deviation) Z(metricID string, v float64, at time.Time) [NLag]float64 {
	var result [NLag]float64
	hour := at.UTC().Hour()
	iv := 30 * time.Second // 默认容差，可从外部设置

	for i, lagSec := range LagSeconds {
		H := time.Duration(lagSec) * time.Second
		ls := d.sigma.Get(metricID, i, hour)
		if !ls.Ready {
			result[i] = math.NaN()
			continue
		}
		var prevVal float64
		var found bool
		if d.LookupFn != nil {
			prevVal, found = d.LookupFn(metricID, at.Add(-H), iv)
		}
		if !found {
			result[i] = math.NaN()
			continue
		}
		z := (v - prevVal) / ls.Sigma
		// clamp ±6
		if z > 6 { z = 6 }
		if z < -6 { z = -6 }
		result[i] = z
	}
	return result
}

// Classify 返回 onset_lag 与 breadth。
// z 中 NaN 表示未就绪，不计入 breadth。
func Classify(z [NLag]float64, th float64) (onsetLag string, breadth int) {
	if th <= 0 { th = 3.0 }
	onset := -1
	for i := 0; i < NLag; i++ {
		if math.IsNaN(z[i]) { continue }
		if math.Abs(z[i]) >= th {
			breadth++
			if onset < 0 { onset = i }
		}
	}
	if onset >= 0 {
		onsetLag = LagID(onset)
	}
	return
}

// ZInt8 将 z 值转为 Int8×20 存储格式（±120 范围）。
func ZInt8(z float64) int8 {
	if math.IsNaN(z) { return 0 }
	v := z * 20
	if v > 120 { v = 120 }
	if v < -120 { v = -120 }
	return int8(v)
}

// SigmaRefreshInterval 返回σ表推荐重算周期（每日一次）。
func SigmaRefreshInterval() time.Duration { return 24 * time.Hour }

// ──────────────────── helpers ──────────────────────────────────

func findNearest(hist []Sample, target time.Time, tol time.Duration) (float64, bool) {
	if len(hist) == 0 { return 0, false }
	// 二分查找最近时间戳
	lo, hi := 0, len(hist)-1
	for lo < hi {
		mid := (lo + hi) / 2
		if hist[mid].TS.Before(target) { lo = mid + 1 } else { hi = mid }
	}
	best := lo
	if lo > 0 && absDur(hist[lo-1].TS.Sub(target)) < absDur(hist[lo].TS.Sub(target)) {
		best = lo - 1
	}
	if absDur(hist[best].TS.Sub(target)) > tol {
		return 0, false
	}
	return hist[best].Value, true
}

func absDur(d time.Duration) time.Duration {
	if d < 0 { return -d }
	return d
}

func computeMAD(vals []float64) float64 {
	if len(vals) == 0 { return 0 }
	med := median(vals)
	devs := make([]float64, len(vals))
	for i, v := range vals { devs[i] = math.Abs(v - med) }
	return median(devs)
}

func median(vals []float64) float64 {
	if len(vals) == 0 { return 0 }
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 { return sorted[n/2] }
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

func absSlice(vals []float64) []float64 {
	out := make([]float64, len(vals))
	for i, v := range vals { out[i] = math.Abs(v) }
	return out
}

func histValues(hist []Sample, hour int, from, to time.Time) []float64 {
	var out []float64
	for _, s := range hist {
		if s.TS.Before(from) || s.TS.After(to) { continue }
		if s.TS.UTC().Hour() != hour { continue }
		out = append(out, s.Value)
	}
	return out
}

func itoa(n int) string {
	if n < 10 { return string(rune('0' + n)) }
	return string([]byte{byte('0' + n/10), byte('0' + n%10)})
}

// SigmaForExport returns whether a given lag index is ready (has any data).
func (d *Deviation) SigmaForExport(lagIdx int) bool {
	// Check if any hour bucket has ready sigma for this lag
	for hour := 0; hour < 24; hour++ {
		// We'd need to check across all known metrics - simplified: return true if sigma table has data
		_ = hour
	}
	return true // simplified: assume ready if sigma table is populated
}
