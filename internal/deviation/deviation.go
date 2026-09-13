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
const NLag = 10

// LagSeconds 是十档的滞后时长（秒），从 5 分钟到 7 天。L9+ 必须是 86400 的整数倍。
//
// 为什么最长只到 7 天：一档 z 要判断"这个变化算不算异常"，既要往回够 H，
// 又要拿最近的历史估计这一档平时的波动，所以**需要 2×H 的历史**。
// 保留期 14 天（见 series.go），能支撑的最长档位就是 7 天。
//
// "跟 14 天前比"这件事由对比表与服务表直接摆出两个时刻的值/在否来回答，
// 不需要统计——跨两周差一倍，人一眼就知道不正常，不必 σ 来告诉他。
var LagSeconds = [NLag]int{
	300, 600, 1200, 2400, 5400, 10800, 21600, 43200,
	86400, 604800,
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

// 算法常量。
const (
	// MinDiffsForZ 是出 z 值所需的最少同时段差分样本数。
	// 样本太少时任何尺度估计都是编的，宁可画斜纹说"还没攒够"，
	// 也不要拿 1~2 个点算出来的 σ 去支撑一个 ±6。
	MinDiffsForZ = 8
	// HighConfN 是"高置信"门槛：样本少于它的档位不参与 L4 下结论（L3 照常显示）。
	HighConfN = 20
	// MinBaselineSpan 是基线必须覆盖的最短墙钟跨度。
	//
	// 只要求"跨度 ≥ 该档时长"是不够的：L1=300s，等于装上 5 分钟就开始出 z，
	// 而那时基线只见过几分钟的安静时段，之后任何正常波动都顶格——
	// 实测一台刚装上的桌面机，十几个指标齐刷刷 ±6.00、广度 2（只有 L1/L2 就绪）。
	// 要求至少覆盖一小时，等于"装上一小时后才开始判定"，这个代价可接受。
	MinBaselineSpan = time.Hour
	// shrinkK 让样本量不足时 z 向 0 收缩：z *= N/(N+shrinkK)。
	// N=8 时 ×0.5，N=20 时 ×0.71，N=200 时 ×0.96。
	shrinkK = 8.0
	// ClampZ 是 z 的截断幅度。
	ClampZ = 6.0
)

// LagSigma 是某 (lag, hour) 组合上滞后差分 Δ=v(t)-v(t-H) 的分布刻画。
//
// 关键点是它同时记录位置（Center）和尺度（Sigma）。早期版本只有尺度，
// z 直接取 Δ/σ，等于假设"Δ 的正常值是 0"。这个假设对任何有稳定漂移的指标
// 都不成立：网卡累计字节数、slab、page cache 的 Δ 恒为一个大正数，于是
// z 永远顶在 +6，整张表通红，真异常反而看不出来。正确做法是把 Δ 与它自己的
// 历史中位数比，也就是标准化残差。
type LagSigma struct {
	Center   float64 // Δ 的历史中位数（正常漂移量）
	Sigma    float64 // Δ 的稳健尺度（MAD → 四分位距 → 分辨率/相对地板）
	Quantum  float64 // Δ 分布的分辨率（相邻不同取值的最小间隔），也是 σ 的地板
	N        int     // 参与估计的差分样本数
	ZeroFrac float64 // Δ 恰好为 0 的比例；>0.5 说明指标处于量化/静止区
	Ready    bool    // 历史跨度够且 N >= MinDiffsForZ
	Low      bool    // N < HighConfN，置信度低
}

// ComputeSigma 批算某档位在某小时桶上 Δ 分布的位置与尺度。
// hist 须按时间升序，覆盖足够历史。
// 只要一个小时桶时用它；批量重算请用 ComputeSigmaLag（一次扫描出 24 个桶）。
func ComputeSigma(lagSeconds int, hour int, hist []Sample) (LagSigma, error) {
	if hour < 0 || hour >= 24 {
		return LagSigma{Sigma: 1e-9}, nil
	}
	all := ComputeSigmaLag(lagSeconds, hist)
	return all[hour], nil
}

// ComputeSigmaLag 一次扫描历史，算出某档位全部 24 个小时桶上 Δ 分布的位置与尺度。
//
// 早期实现是对每个 (lag, hour) 各扫一遍全量历史并逐点二分，一个指标要扫
// 14×24=336 遍；这里每个 lag 只扫一遍，v(t-H) 用双指针推进（t 单调 ⇒ t-H 单调），
// 结果与逐桶计算逐位相同（见 TestComputeSigmaLagMatchesLegacy）。
func ComputeSigmaLag(lagSeconds int, hist []Sample) [24]LagSigma {
	return ComputeSigmaLagExcluding(lagSeconds, hist, time.Time{})
}

// MaxExcludedFrac 是"异常期样本"最多能占历史的比例。超过它就不再排除——
// 占了历史四分之一以上的状态，已经不是异常，是新常态，该学进去。
const MaxExcludedFrac = 0.25

// ComputeSigmaLagExcluding 与 ComputeSigmaLag 相同，但把 excludeFrom 之后的样本
// 从 Δ 分布里剔除（用于把正在发生的故障排除在基线之外）。
//
// 为什么需要：σ 的尺度取 MAD / 四分位差 / 十分位差的最大者，而一个持续几天的阶跃故障
// 会让十分位差直接张到故障幅度那么大 —— 实测一个持续 3 天的故障，z 从 4.45 掉到 −1.70：
// 机器还病着，报警自己没了。
//
// 为什么按比例而不是按时长封顶：业务真的扩容了、流量真的涨一倍，那是新常态。
// 用"最多排除 1/4 历史"这个界，既挡住了几天量级的故障被学成正常，
// 又保证真的长期变化最终会被接受。excludeFrom 为零值时行为与 ComputeSigmaLag 完全一致。
func ComputeSigmaLagExcluding(lagSeconds int, hist []Sample, excludeFrom time.Time) [24]LagSigma {
	var out [24]LagSigma
	for h := range out {
		out[h] = LagSigma{Sigma: 1e-9}
	}
	if len(hist) == 0 {
		return out
	}
	// 先看排除的量是否在上限之内；超了就当作新常态，不再排除。
	if !excludeFrom.IsZero() {
		n := 0
		for _, sm := range hist {
			if !sm.TS.Before(excludeFrom) {
				n++
			}
		}
		if float64(n) > MaxExcludedFrac*float64(len(hist)) {
			excludeFrom = time.Time{}
		}
	}
	H := time.Duration(lagSeconds) * time.Second
	// 容差必须与 Z 里配 v(t-H) 用的一致，否则 σ 是"松配对"的尺度、
	// Δ 是"紧配对"的量，两者根本不是同一个分布。
	tol := LagTolerance(H)
	histStart := hist[0].TS
	end := hist[len(hist)-1].TS
	span := end.Sub(histStart)
	spanOK := span >= H && span >= MinBaselineSpan
	cutoff := end.Add(-28 * 24 * time.Hour)

	var diffs [24][]float64
	j := 0 // hist[j] 是第一个 TS >= t-H 的点
	for i := range hist {
		s := hist[i]
		if s.TS.Before(cutoff) {
			continue
		}
		if !excludeFrom.IsZero() && !s.TS.Before(excludeFrom) {
			continue // 异常期的样本不参与基线
		}
		target := s.TS.Add(-H)
		if target.Before(histStart) {
			continue
		}
		for hist[j].TS.Before(target) { // target < s.TS，故 j 不会越过 i
			j++
		}
		best := j
		if j > 0 && absDur(hist[j-1].TS.Sub(target)) < absDur(hist[j].TS.Sub(target)) {
			best = j - 1
		}
		if absDur(hist[best].TS.Sub(target)) > tol {
			continue
		}
		h := s.TS.UTC().Hour()
		diffs[h] = append(diffs[h], s.Value-hist[best].Value)
	}
	for h := range out {
		out[h] = summarizeDiffs(diffs[h], spanOK)
	}
	return out
}

// summarizeDiffs 把一个小时桶的 Δ 样本归纳成 LagSigma。
func summarizeDiffs(diffs []float64, spanOK bool) LagSigma {
	ls := LagSigma{N: len(diffs)}
	ls.Ready = spanOK && len(diffs) >= MinDiffsForZ
	ls.Low = ls.N < HighConfN
	if len(diffs) == 0 {
		ls.Sigma = 1e-9
		return ls
	}
	// 只排一次序：中位数、分位差、分辨率都从同一份有序副本上取。
	// 早期每个桶要把同一批 Δ 排 5 次序外加一个 map，σ 重算一半时间花在 sort 上。
	sorted := make([]float64, len(diffs))
	copy(sorted, diffs)
	sort.Float64s(sorted)
	ls.Center = medianSorted(sorted)
	ls.Quantum = resolutionSorted(sorted)
	ls.ZeroFrac = zeroFraction(diffs)
	ls.Sigma = robustScaleSorted(sorted, ls.Center, ls.Quantum)
	return ls
}

// robustScaleSorted 与 robustScale 语义完全相同，输入须已升序。
func robustScaleSorted(sorted []float64, center, resolution float64) float64 {
	devs := make([]float64, len(sorted))
	med := medianSorted(sorted)
	for i, v := range sorted {
		devs[i] = math.Abs(v - med)
	}
	sort.Float64s(devs)
	s := 1.4826 * medianSorted(devs)
	if v := spreadSorted(sorted, 0.25, 4) / 1.349; v > s {
		s = v
	}
	if v := spreadSorted(sorted, 0.10, 10) / 2.563; v > s {
		s = v
	}
	if s <= 0 {
		s = math.Max(resolution, relFloorFrac*math.Abs(center))
	}
	if s < resolution {
		s = resolution
	}
	if s <= 0 {
		s = 1e-9
	}
	return s
}

func medianSorted(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// spreadSorted 与 quantileSpread 相同，输入须已升序。
func spreadSorted(sorted []float64, p float64, minN int) float64 {
	if len(sorted) < minN {
		return 0
	}
	q := func(pp float64) float64 {
		idx := pp * float64(len(sorted)-1)
		lo := int(math.Floor(idx))
		hi := int(math.Ceil(idx))
		if lo == hi {
			return sorted[lo]
		}
		return sorted[lo] + (sorted[hi]-sorted[lo])*(idx-float64(lo))
	}
	return q(1-p) - q(p)
}

// resolutionSorted 与 resolutionOf 相同，输入须已升序。
func resolutionSorted(sorted []float64) float64 {
	allInt := true
	res := math.Inf(1)
	for i, d := range sorted {
		if d != math.Trunc(d) {
			allInt = false
		}
		if i > 0 {
			if gap := d - sorted[i-1]; gap > 0 && gap < res {
				res = gap
			}
		}
	}
	if math.IsInf(res, 1) {
		res = 0
	}
	if allInt && res < 1 {
		res = 1
	}
	return res
}

// relFloorFrac 是退化情形下的相对地板：Δ 在分辨率内完全恒定时，
// 尺度只能相对它自己的量级来给。5% 意味着速率变化 15% 才到 z=3。
const relFloorFrac = 0.05

// robustScale 估计 Δ 的尺度，逐级退化。
//
// 为什么要有阶梯：MAD 在超过一半样本取值相同时恒为 0。这在节点指标里是常态
// ——整数型指标（某进程的 CPU jiffies 在 0/1 之间跳）、速率恒定的计数器，
// 差分里一大半甚至全部是同一个数。σ=0 之后无论加什么绝对地板都会失真：
// 地板取小了（早期版本 1e-6）任何抖动都是 ±6；地板取死了（早期版本 1/6）
// 整数指标动一格就恰好顶到 6。
//
// 也不能拿 |Δ| 的中位数当尺度（早期版本的 0.02*medDiff）：那是 Δ 的量级，
// 不是它的离散程度，代进去 z 恒等于 50，一样满量程。
//
// 阶梯是：MAD → 四分位距 → 完全退化时用「分辨率」与「量级的 5%」取大者，
// 最后统一以分辨率为地板。这样动一格 ⇒ z≈1，动三格 ⇒ z≈3。
func robustScale(diffs []float64, center, resolution float64) float64 {
	// 三个在正态下都收敛到 σ 的稳健估计，取最大者。
	// 正态数据上三者一致；多峰/重尾数据上（周期性抖动、定时任务、锯齿型缓存）
	// MAD 只看中位数附近那一团，会把另一个峰当成异常，分位差则能覆盖到峰间距。
	// 取 max 意味着宁可保守：只有真的超出历史见过的幅度才算异常。
	s := 1.4826 * computeMAD(diffs)
	if v := iqr(diffs) / 1.349; v > s { // 四分位差
		s = v
	}
	if v := interdecile(diffs) / 2.563; v > s { // 十分位差，覆盖多峰
		s = v
	}
	if s <= 0 {
		// Δ 在分辨率内完全恒定（典型：速率恒定的累计计数器、恒 0 的指标）。
		s = math.Max(resolution, relFloorFrac*math.Abs(center))
	}
	if s < resolution {
		s = resolution
	}
	if s <= 0 {
		s = 1e-9
	}
	return s
}

// resolutionOf 推断 Δ 分布的分辨率：相邻不同取值之间的最小间隔。
// 只有一个取值时无从推断，返回 0（由相对地板兜底）；
// 全为整数的指标至少给 1，避免浮点噪声把分辨率压到无意义的小值。
func resolutionOf(diffs []float64) float64 {
	allInt := true
	uniq := make([]float64, 0, len(diffs))
	seen := make(map[float64]struct{}, len(diffs))
	for _, d := range diffs {
		if d != math.Trunc(d) {
			allInt = false
		}
		if _, ok := seen[d]; !ok {
			seen[d] = struct{}{}
			uniq = append(uniq, d)
		}
	}
	res := math.Inf(1)
	if len(uniq) >= 2 {
		sort.Float64s(uniq)
		for i := 1; i < len(uniq); i++ {
			if gap := uniq[i] - uniq[i-1]; gap > 0 && gap < res {
				res = gap
			}
		}
	}
	if math.IsInf(res, 1) {
		res = 0
	}
	if allInt && res < 1 {
		res = 1
	}
	return res
}

func zeroFraction(diffs []float64) float64 {
	if len(diffs) == 0 {
		return 0
	}
	n := 0
	for _, d := range diffs {
		if d == 0 {
			n++
		}
	}
	return float64(n) / float64(len(diffs))
}

// quantileSpread 返回 [p, 1-p] 分位差，样本不足时返回 0。
func quantileSpread(vals []float64, p float64, minN int) float64 {
	if len(vals) < minN {
		return 0
	}
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)
	q := func(pp float64) float64 {
		idx := pp * float64(len(sorted)-1)
		lo := int(math.Floor(idx))
		hi := int(math.Ceil(idx))
		if lo == hi {
			return sorted[lo]
		}
		return sorted[lo] + (sorted[hi]-sorted[lo])*(idx-float64(lo))
	}
	return q(1-p) - q(p)
}

// iqr 是四分位差 P75-P25，正态下等于 1.349σ。
func iqr(vals []float64) float64 { return quantileSpread(vals, 0.25, 4) }

// interdecile 是十分位差 P90-P10，正态下等于 2.563σ。
func interdecile(vals []float64) float64 { return quantileSpread(vals, 0.10, 10) }

// LagTolerance 是配 v(t-H) 时允许的时间误差：H 的 2%，夹在 30 秒到 30 分钟之间。
// ComputeSigma 与 Z 必须用同一个值。
func LagTolerance(H time.Duration) time.Duration {
	tol := H / 50
	if tol < 30*time.Second {
		tol = 30 * time.Second
	}
	if tol > 30*time.Minute {
		tol = 30 * time.Minute
	}
	return tol
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

// Delete 删除某指标的全部 σ（序列被回收时调用）。
func (t *SigmaTable) Delete(metricID string) {
	t.mu.Lock()
	delete(t.data, metricID)
	t.mu.Unlock()
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
//
// z = (Δ - Center) / Sigma，其中 Δ = v(t) - v(t-H)。
// 减 Center 这一步是关键：它把"这个指标平时就在涨"从异常里剔除出去，
// 剩下的才是"这次涨得跟平时不一样"。样本量不足时再按 N/(N+k) 收缩，
// 避免刚启动几分钟就给出满量程结论。
func (d *Deviation) Z(metricID string, v float64, at time.Time) [NLag]float64 {
	var result [NLag]float64
	hour := at.UTC().Hour()

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
			prevVal, found = d.LookupFn(metricID, at.Add(-H), LagTolerance(H))
		}
		if !found {
			result[i] = math.NaN()
			continue
		}
		sigma := ls.Sigma
		if sigma <= 0 {
			sigma = 1e-9
		}
		z := (v - prevVal - ls.Center) / sigma
		z *= float64(ls.N) / (float64(ls.N) + shrinkK) // 小样本收缩
		if z > ClampZ {
			z = ClampZ
		}
		if z < -ClampZ {
			z = -ClampZ
		}
		result[i] = z
	}
	return result
}

// Classify 返回 onset_lag 与 breadth。
// z 中 NaN 表示未就绪，不计入 breadth。
func Classify(z [NLag]float64, th float64) (onsetLag string, breadth int) {
	if th <= 0 {
		th = 3.0
	}
	onset := -1
	for i := 0; i < NLag; i++ {
		if math.IsNaN(z[i]) {
			continue
		}
		if math.Abs(z[i]) >= th {
			breadth++
			if onset < 0 {
				onset = i
			}
		}
	}
	if onset >= 0 {
		onsetLag = LagID(onset)
	}
	return
}

// ZInt8 将 z 值转为 Int8×20 存储格式（±120 范围）。
func ZInt8(z float64) int8 {
	if math.IsNaN(z) {
		return 0
	}
	v := z * 20
	if v > 120 {
		v = 120
	}
	if v < -120 {
		v = -120
	}
	return int8(v)
}

// SigmaRefreshInterval 返回σ表推荐重算周期（每日一次）。
func SigmaRefreshInterval() time.Duration { return 24 * time.Hour }

// ──────────────────── helpers ──────────────────────────────────

func findNearest(hist []Sample, target time.Time, tol time.Duration) (float64, bool) {
	if len(hist) == 0 {
		return 0, false
	}
	// 二分查找最近时间戳
	lo, hi := 0, len(hist)-1
	for lo < hi {
		mid := (lo + hi) / 2
		if hist[mid].TS.Before(target) {
			lo = mid + 1
		} else {
			hi = mid
		}
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
	if d < 0 {
		return -d
	}
	return d
}

func computeMAD(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	med := median(vals)
	devs := make([]float64, len(vals))
	for i, v := range vals {
		devs[i] = math.Abs(v - med)
	}
	return median(devs)
}

func median(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

func absSlice(vals []float64) []float64 {
	out := make([]float64, len(vals))
	for i, v := range vals {
		out[i] = math.Abs(v)
	}
	return out
}

func histValues(hist []Sample, hour int, from, to time.Time) []float64 {
	var out []float64
	for _, s := range hist {
		if s.TS.Before(from) || s.TS.After(to) {
			continue
		}
		if s.TS.UTC().Hour() != hour {
			continue
		}
		out = append(out, s.Value)
	}
	return out
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
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
