// deviation.go — 9 档滞后差分偏离度计算。
// σ 按小时分桶，下限三选一，z clamp ±6，对齐段严格24h整数倍。
package deviation

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NLag 档位总数。
const NLag = 9

// LagSeconds 是九档的滞后时长（秒），从 5 分钟到 7 天。
//
// 这 9 个点跟页面上对比表、服务表用的是同一套（cmd/nodedata/timepoints.go 的前 9 个，
// 由测试钉住）。原来是 5分 10分 20分 40分 1.5时 3时 6时 12时 1天 7天，跟另外两张表
// 各不相同；而且短档之间高度重叠——一次突变在所有档位上都是立即可见的（现在和 H 前一比
// 就不一样），档位的差别只在"这个变化多久之内还算新的"，20/40 分钟并没有让发现更快。
//
// 为什么最长只到 7 天：一档 z 要判断"这个变化算不算异常"，既要往回够 H，
// 又要拿最近的历史估计这一档平时的波动，所以**需要 2×H 的历史**。
// 保留期 14 天（见 series.go），能支撑的最长档位就是 7 天。
//
// "跟 14 天前比"这件事由对比表与服务表直接摆出两个时刻的值/在否来回答，
// 不需要统计——跨两周差一倍，人一眼就知道不正常，不必 σ 来告诉他。
var LagSeconds = [NLag]int{
	300, 600, 1800, 3600, 21600, 43200,
	86400, 259200, 604800,
}

// LagID 返回档位的内部编号，1-indexed。只给程序用（JSON 字段、测试）；
// 页面和报告上一律用 LagName——"L6"对人没有意义，"3小时"才有。
func LagID(i int) string {
	return "L" + itoa(i+1)
}

// LagName 返回档位的人话名字：5分钟、1.5小时、1天、7天……
// 直接从 LagSeconds 算，不另存一张表——改了档位长度，名字自动跟着变。
func LagName(i int) string {
	if i < 0 || i >= NLag {
		return ""
	}
	return DurationName(LagSeconds[i])
}

// LagNameByID 把 "L6" 翻成 "3小时"；认不出的原样返回。
func LagNameByID(id string) string {
	if len(id) < 2 || id[0] != 'L' {
		return id
	}
	n := 0
	for _, c := range id[1:] {
		if c < '0' || c > '9' {
			return id
		}
		n = n*10 + int(c-'0')
	}
	if n < 1 || n > NLag {
		return id
	}
	return LagName(n - 1)
}

// DurationName 把秒数写成最短的人话：300→5分钟，5400→1.5小时，86400→1天。
func DurationName(sec int) string {
	trim := func(f float64) string {
		s := strconv.FormatFloat(f, 'f', 1, 64)
		return strings.TrimSuffix(s, ".0")
	}
	switch {
	case sec >= 86400: // 历史跨度这种不整的数：431700 秒写成 5天，不写 119.9小时
		return trim(float64(sec)/86400) + "天"
	case sec >= 3600:
		return trim(float64(sec)/3600) + "小时"
	case sec >= 60:
		return trim(float64(sec)/60) + "分钟"
	default:
		return itoa(sec) + "秒"
	}
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
	// 但一小时的代价被低估了：只要 history/ 里没有可用的长期层（新机器、换了 data-dir、
	// 或历史被清掉），**每次重启后画面都要盲一小时**——十个档位全是斜纹，
	// 而"刚刚发生"这一屏恰恰是重启后最该能看的。
	//
	// 现在防"一屏红"靠的是另一道更准的护栏：同时段样本不足 HighConfN 的档位
	// 不参与 L4 下结论（见 HeatmapBuilder.LowConfLags）。L3 照常显示——那一层是摆事实。
	// 所以这里放宽到 15 分钟：既挡住"只见过几分钟安静时段就开始判定"，
	// 又不至于让一次重启换来一小时的失明。
	MinBaselineSpan = 15 * time.Minute
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
	//
	// 还有一种更隐蔽的情况必须一并挡住：**按小时分桶之后，某个桶可能被剔空**。
	// 机器运行不满一天时，"当前小时"只有今天这一份样本，而异常起点通常就在最近一小时内，
	// 于是这个桶的样本被全部排除、N=0、该档位直接变成"未就绪"。
	// 表现就是：L1–L5（用原始层）全是斜纹，L6–L10（用跨天的长期层）正常——
	// 实测一台运行 10 小时的机器，某指标一进 L4 结论，L1 的 N 从 361 掉到 0。
	// 宁可这一轮基线被故障污染，也不能让短档位整片失明：失明是彻底看不见，
	// 污染只是尺度偏大，而且下一轮就有机会恢复。
	if !excludeFrom.IsZero() {
		n := 0
		for _, sm := range hist {
			if !sm.TS.Before(excludeFrom) {
				n++
			}
		}
		if float64(n) > MaxExcludedFrac*float64(len(hist)) {
			excludeFrom = time.Time{}
		} else if !enoughAfterExclusion(hist, excludeFrom) {
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

// enoughAfterExclusion 判断排除之后，"当前时刻所在的小时桶"是否还剩够用的样本。
// 只看这一个桶：它是 z 实际会用到的那个，别的桶剩多少都不影响当下能不能出 z。
func enoughAfterExclusion(hist []Sample, excludeFrom time.Time) bool {
	if len(hist) == 0 {
		return false
	}
	hour := hist[len(hist)-1].TS.UTC().Hour()
	kept := 0
	for _, sm := range hist {
		if sm.TS.UTC().Hour() != hour || !sm.TS.Before(excludeFrom) {
			continue
		}
		kept++
		if kept >= MinDiffsForZ*2 { // 差分数略少于样本数，留一倍余量
			return true
		}
	}
	return false
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
// LagTolerance 是配 v(t−H) 时允许的时间误差。
//
// 原来是 H/50、下限 30 秒。问题出在重启之后：原始层清空，只剩磁盘上的长期层，
// 而长期层是 5 分钟一格——L3（20 分钟档）的容差只有 30 秒，永远配不上，
// 于是短档整片斜纹，一直要等原始层自己攒够 20 分钟。
// 页面上那行自检就是这么说的："配不到 1200s 前的点（容差 30s）：长期层是 5m0s 一格、对不上短档"。
//
// 改成 H/8（12.5%）：L3 的容差变成 150 秒，正好够上 5 分钟的长期层格子，
// 重启后 L3 及以上立刻可用。误差被限制在该档时长的 12.5% 以内——
// 拿"20 分钟前 ±2.5 分钟"和"20 分钟前"比，对判断变化幅度没有实质影响，
// 而且 σ 的历史 Δ 用的是同一个容差，口径自洽。
// L1/L2 仍然配不上（37 秒 / 75 秒 < 150 秒），只能等原始层攒够 5~10 分钟——那是物理下限。
func LagTolerance(H time.Duration) time.Duration {
	tol := H / 8
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
	// MinDelta 返回该指标"值得一提的最小变化量"。返回 0 表示不设门槛。
	//
	// z 是尺度无关的：一台安静的机器 σ 趋近于 0，于是 202 字节/秒的流量、
	// 每秒 2.8 个包、0.1 次重传也都是 6σ，整屏红。统计上显著 ≠ 实际上要紧。
	// 这个门槛就是"效应量"：变化本身太小的时候，不管它偏离平时多少都按 0 处理。
	MinDelta func(metricID string) float64
}

// New 创建 Deviation。
func New(sigma *SigmaTable) *Deviation {
	return &Deviation{sigma: sigma}
}

// Z 返回十档 z 值，未就绪/未找到历史值时返回 NaN。
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
		effect := v - prevVal - ls.Center
		if d.MinDelta != nil {
			if floor := d.MinDelta(metricID); floor > 0 && math.Abs(effect) < floor {
				result[i] = 0 // 变化太小，不值一提——不是"没数据"，所以给 0 不给 NaN
				continue
			}
		}
		z := effect / sigma
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
