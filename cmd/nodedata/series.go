// series.go — 进程内时间序列缓冲区（L1 采集 → L2 转储 的中间层）。
// 不依赖 ClickHouse：保留最近 N 个采样点在内存中，供 heatmap 查询与静态转储使用。
//
// 性能约定（v3.0.9）：每条序列按时间非降序追加，Lookup / Range 一律二分。
// 早期版本 Lookup 是全缓冲线性扫描（17280 点），而 Build 对每个点、每个 lag 都要
// Lookup 一次 —— 满缓冲时单指标单窗口 ~0.37s，转储 5 个窗口 + 页面每 5s 一次
// /api/diagnosis，进程自己吃掉 1.5 个核。
//
// 内存约定（v5.20）：
//   - 原始层每点 16 字节（unix 纳秒 + float64）。原来用 time.Time 存，每点 32 字节，
//     134 条序列满 24 小时是 37MB，是整个进程最大的一块。
//   - 缓冲区定长复用：容量到顶后把最新的点挪回数组开头，原地继续写。原来是
//     buf = buf[len-N:] 再 append，每次越过容量 Go 都会新分配一个更大的数组、
//     旧的变垃圾——每条序列实际占到 1.25~2 倍，还持续制造 GC 压力。
//     挪回开头每小时一次（原始层）或每天一次（长期层），拷贝成本可以忽略。
package main

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
	"github.com/githubflyideas/nodedata/internal/deviation"
	"github.com/githubflyideas/nodedata/internal/metrics"
)

// maxPointsPerMetric 每个指标保留的最大原始点数。
// 10s 间隔 × 8640 点 ≈ 24 小时。采集周期从 5s 放宽到 10s 之后，
// 这个数也要跟着减半——否则原始层会变成 48 小时，内存白涨一倍。
const maxPointsPerMetric = 8640

// rawSlack 是原始层容量超出 maxPointsPerMetric 的余量（10 秒一点 = 1 小时）。
// 余量里的点对外不可见（读取只看最新的 maxPointsPerMetric 个），只是为了让
// "挪回开头"每小时才发生一次，而不是每来一个点就搬一次。
const rawSlack = 360

// 长期层：每 5 分钟从原始点里"抽"一个点（不求平均），保留 14 天，并落盘。
//
// 为什么抽点而不平均：z 比的是 Δ=v(t)-v(t-H) 与它的历史分布。抽出来的点仍是原始点，
// Δ 的分布与用原始数据算的完全同分布，只是样本少；平均会把 Δ 的离散度压小，σ 偏小、z 偏大。
// 为什么 14 天：对比表与服务表要能看到"14 天前"，而 14 天以前的数据基本不看。
// 它同时决定 z 的最长档位——一档需要 2×H 的历史，所以 14 天保留期对应最长 7 天档。
// 长期层用 16 字节的 cpoint（unix 秒 + float64），14 天 × 5 分钟 = 4032 点 ≈ 64KB/序列。
const (
	coarseStep      = 5 * time.Minute
	coarseRetention = 14 * 24 * time.Hour
	maxCoarsePoints = int(coarseRetention/coarseStep) + 12
	coarseSlack     = 288 // 一天：长期层每天才挪一次
)

// cpoint 是长期层的紧凑点。
type cpoint struct {
	T int64 // unix 秒
	V float64
}

func (c cpoint) point() point { return point{TS: time.Unix(c.T, 0), V: c.V} }

// rpoint 是原始层的紧凑点：16 字节。对外一律转成 point。
type rpoint struct {
	N int64 // unix 纳秒
	V float64
}

func (r rpoint) point() point { return point{TS: time.Unix(0, r.N), V: r.V} }

// point 是对外的采样点。
type point struct {
	TS time.Time
	V  float64
}

// 峰值（v5.20）：长期层每 5 分钟只抽 1 个点，一次 40 秒的打满大约 87% 会从 14 天历史里
// 消失——以展示为主的工具答不上"昨天凌晨出过事没有"是硬伤。所以每个 5 分钟桶另记一个
// "最坏值"：升高是坏事的取最大，降低是坏事的取最小（方向登记在 internal/metrics）。
//
// **稀疏**：最坏值跟桶的抽样点相差不到该指标的最小变化量时不记——机器安静时一个都不记，
// 只有出事的桶才多几个数。抽样点本身不动，z 的统计口径不受影响。
type cpeak struct {
	T int64 // 所在桶的抽样点时刻（与 cpoint.T 相同），unix 秒
	P float64
}

// Peak 是一个待落盘的峰值。
type Peak struct {
	MetricID string
	T        int64
	P        float64
}

// openBucket 记录某指标当前这个 5 分钟桶里见过的最坏值。
type openBucket struct {
	v, worst   float64
	dir, floor float64
	unix       int64
}

// Series 是按 metricID 索引的时间序列缓冲区，并发安全。
// data 是原始层（24h），coarse 是长期层（5 分钟一点，14 天），peaks 是长期层的稀疏峰值。
type Series struct {
	mu     sync.RWMutex
	data   map[string][]rpoint
	coarse map[string][]cpoint
	peaks  map[string][]cpeak
	open   map[string]*openBucket
	pend   []Peak // 已结束、尚未落盘的峰值
}

func NewSeries() *Series {
	return &Series{
		data:   make(map[string][]rpoint, 256),
		coarse: make(map[string][]cpoint, 256),
		peaks:  make(map[string][]cpeak, 64),
		open:   make(map[string]*openBucket, 256),
	}
}

const coarseStepSec = int64(coarseStep / time.Second)

func coarseBucket(unix int64) int64 { return unix / coarseStepSec }

// raw 返回某指标对外可见的原始层（最新的 maxPointsPerMetric 个点）。调用方须持锁。
func (s *Series) raw(id string) []rpoint {
	b := s.data[id]
	if len(b) > maxPointsPerMetric {
		return b[len(b)-maxPointsPerMetric:]
	}
	return b
}

// appendRaw 定长复用地追加一个原始点。
func appendRaw(buf []rpoint, p rpoint) []rpoint {
	const capMax = maxPointsPerMetric + rawSlack
	if len(buf) == cap(buf) {
		if len(buf) >= maxPointsPerMetric {
			// 到顶：把最新的 N-1 个挪回开头，原地复用，不分配
			k := copy(buf, buf[len(buf)-maxPointsPerMetric+1:])
			buf = buf[:k]
		} else {
			nc := 2 * cap(buf)
			if nc < 64 {
				nc = 64
			}
			if nc > capMax {
				nc = capMax
			}
			nb := make([]rpoint, len(buf), nc)
			copy(nb, buf)
			buf = nb
		}
	}
	return append(buf, p)
}

// Add 追加一批采样点，超出上限时丢弃最旧的点。
// 早于该序列最后一个点的样本直接丢弃（墙钟回拨），以保证二分查找的前提成立。
// 返回本批中被抽进长期层的样本（每指标每 5 分钟桶第一个点），由调用方落盘。
// 同时维护每个桶的最坏值；桶结束时值得记的峰值进 pend，由 TakePeaks 取走落盘。
func (s *Series) Add(samples []collector.Sample) []collector.Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	var kept []collector.Sample
	for _, sm := range samples {
		n := sm.TS.UnixNano()
		buf := s.data[sm.MetricID]
		if l := len(buf); l > 0 && n < buf[l-1].N {
			continue
		}
		s.data[sm.MetricID] = appendRaw(buf, rpoint{N: n, V: sm.Value})

		cb := s.coarse[sm.MetricID]
		u := sm.TS.Unix()
		if len(cb) == 0 || coarseBucket(u) > coarseBucket(cb[len(cb)-1].T) {
			s.closeBucketLocked(sm.MetricID)
			s.coarse[sm.MetricID] = appendCapped(cb, cpoint{T: u, V: sm.Value})
			kept = append(kept, sm)
			in := metrics.Lookup(sm.MetricID) // 每指标每 5 分钟查一次，不是每个点
			s.open[sm.MetricID] = &openBucket{unix: u, v: sm.Value, worst: sm.Value, dir: in.Direction(), floor: in.MinDelta}
		} else if ob := s.open[sm.MetricID]; ob != nil {
			if (ob.dir > 0 && sm.Value > ob.worst) || (ob.dir < 0 && sm.Value < ob.worst) {
				ob.worst = sm.Value
			}
		}
	}
	return kept
}

// closeBucketLocked 结束某指标当前的桶：最坏值跟抽样点差够大才记。
func (s *Series) closeBucketLocked(id string) {
	ob := s.open[id]
	if ob == nil {
		return
	}
	delete(s.open, id)
	d := ob.worst - ob.v
	if d < 0 {
		d = -d
	}
	if ob.floor <= 0 || d < ob.floor {
		return
	}
	s.peaks[id] = appendPeak(s.peaks[id], cpeak{T: ob.unix, P: ob.worst})
	s.pend = append(s.pend, Peak{MetricID: id, T: ob.unix, P: ob.worst})
}

// TakePeaks 取走已结束、尚未落盘的峰值。
func (s *Series) TakePeaks() []Peak {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pend
	s.pend = nil
	return p
}

// AddPeak 把落盘的峰值装回来（启动时用）。可以乱序：峰值行写在下一个桶之后。
func (s *Series) AddPeak(id string, t int64, v float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pk := s.peaks[id]
	i := sort.Search(len(pk), func(i int) bool { return pk[i].T >= t })
	if i < len(pk) && pk[i].T == t {
		pk[i].P = v
		return
	}
	pk = append(pk, cpeak{})
	copy(pk[i+1:], pk[i:])
	pk[i] = cpeak{T: t, P: v}
	if len(pk) > maxCoarsePoints {
		pk = pk[len(pk)-maxCoarsePoints:]
	}
	s.peaks[id] = pk
}

func appendPeak(pk []cpeak, p cpeak) []cpeak {
	if n := len(pk); n > 0 && pk[n-1].T >= p.T {
		return pk
	}
	pk = append(pk, p)
	if len(pk) > maxCoarsePoints {
		pk = pk[len(pk)-maxCoarsePoints:]
	}
	return pk
}

// AddCoarse 把落盘历史装回长期层（启动时用）。须按时间升序调用。
func (s *Series) AddCoarse(samples []collector.Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sm := range samples {
		cb := s.coarse[sm.MetricID]
		u := sm.TS.Unix()
		if n := len(cb); n > 0 && coarseBucket(u) <= coarseBucket(cb[n-1].T) {
			continue
		}
		s.coarse[sm.MetricID] = appendCapped(cb, cpoint{T: u, V: sm.Value})
	}
}

// appendCapped 定长复用地追加一个长期层点，对外可见的最多 maxCoarsePoints 个。
func appendCapped(buf []cpoint, p cpoint) []cpoint {
	const capMax = maxCoarsePoints + coarseSlack
	if len(buf) == cap(buf) {
		if len(buf) >= maxCoarsePoints {
			k := copy(buf, buf[len(buf)-maxCoarsePoints+1:])
			buf = buf[:k]
		} else {
			nc := 2 * cap(buf)
			if nc < 16 {
				nc = 16
			}
			if nc > capMax {
				nc = capMax
			}
			nb := make([]cpoint, len(buf), nc)
			copy(nb, buf)
			buf = nb
		}
	}
	return append(buf, p)
}

// coarseOf 返回某指标对外可见的长期层。调用方须持锁。
func (s *Series) coarseOf(id string) []cpoint {
	b := s.coarse[id]
	if len(b) > maxCoarsePoints {
		return b[len(b)-maxCoarsePoints:]
	}
	return b
}

// coarseSlice 取长期层 [fromU, toU]（unix 秒，闭区间）并转成 point。
func coarseSlice(buf []cpoint, fromU, toU int64) []point {
	lo := sort.Search(len(buf), func(i int) bool { return buf[i].T >= fromU })
	hi := sort.Search(len(buf), func(i int) bool { return buf[i].T > toU })
	if lo >= hi {
		return nil
	}
	out := make([]point, hi-lo)
	for i := range out {
		out[i] = buf[lo+i].point()
	}
	return out
}

// unixCeil / unixFloor 把 time.Time 夹到整秒边界，用于长期层区间比较。
func unixCeil(t time.Time) int64 {
	u := t.Unix()
	if t.After(time.Unix(u, 0)) {
		u++
	}
	return u
}

// MetricIDs 返回当前已知的全部指标 ID（两层的并集，已排序）。
func (s *Series) MetricIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.data))
	for id := range s.data {
		ids = append(ids, id)
	}
	for id := range s.coarse {
		if _, ok := s.data[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// nanoOf 把时刻换成 unix 纳秒，超出 int64 能表示的范围（约 1678–2262 年）时夹到边界。
//
// 不能直接 t.UnixNano()：调用方会用 time.Time{}（公元 1 年）和 farFuture 表示"整层"，
// UnixNano 对它们的结果是未定义的——实测溢出成一个莫名其妙的数，二分查找返回空，
// σ 于是悄悄改用长期层，5 分钟档从 242 个样本掉到 5 个。
func nanoOf(t time.Time) int64 {
	if t.Before(minNanoTime) {
		return math.MinInt64
	}
	if t.After(maxNanoTime) {
		return math.MaxInt64
	}
	return t.UnixNano()
}

var (
	minNanoTime = time.Unix(0, math.MinInt64)
	maxNanoTime = time.Unix(0, math.MaxInt64)
)

// rawRange 取原始层 [from, to] 的下标区间。
func rawRange(buf []rpoint, from, to time.Time) (lo, hi int) {
	f, t := nanoOf(from), nanoOf(to)
	lo = sort.Search(len(buf), func(i int) bool { return buf[i].N >= f })
	hi = sort.Search(len(buf), func(i int) bool { return buf[i].N > t })
	return
}

func rawPoints(buf []rpoint) []point {
	out := make([]point, len(buf))
	for i := range buf {
		out[i] = buf[i].point()
	}
	return out
}

// Range 返回 [from, to] 区间内某指标的点（副本）。
// 原始层覆盖不到的更早部分由长期层补上，所以 7d/14d 窗口是真的 7 天/14 天。
func (s *Series) Range(metricID string, from, to time.Time) []point {
	s.mu.RLock()
	defer s.mu.RUnlock()
	raw := s.raw(metricID)
	coarseTo := to.Unix()
	if len(raw) > 0 && raw[0].N < nanoOf(to) {
		coarseTo = unixCeil(time.Unix(0, raw[0].N)) - 1 // 长期层只补原始层覆盖不到的更早部分
	}
	c := coarseSlice(s.coarseOf(metricID), unixCeil(from), coarseTo)
	lo, hi := rawRange(raw, from, to)
	if lo >= hi {
		return c
	}
	return append(c, rawPoints(raw[lo:hi])...)
}

// RawRange 只取原始层。
func (s *Series) RawRange(metricID string, from, to time.Time) []point {
	s.mu.RLock()
	defer s.mu.RUnlock()
	raw := s.raw(metricID)
	lo, hi := rawRange(raw, from, to)
	if lo >= hi {
		return nil
	}
	return rawPoints(raw[lo:hi])
}

// CoarseRange 只取长期层。
func (s *Series) CoarseRange(metricID string, from, to time.Time) []point {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return coarseSlice(s.coarseOf(metricID), unixCeil(from), to.Unix())
}

// Worst 返回 [from, to] 内该指标"最坏"的值：原始层覆盖的部分看每一个原始点，
// 更早的部分看长期层的抽样点和稀疏峰值。方向登记在 internal/metrics。
// 画图时每一步只取一个点，短尖峰会被跳过；这个值给图画出"这一步里最坏到过哪"。
func (s *Series) Worst(metricID string, from, to time.Time) (float64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	dir := metrics.Lookup(metricID).Direction()
	best, ok := 0.0, false
	take := func(v float64) {
		if !ok || (dir > 0 && v > best) || (dir < 0 && v < best) {
			best, ok = v, true
		}
	}
	raw := s.raw(metricID)
	rawFrom := int64(math.MaxInt64)
	if len(raw) > 0 {
		rawFrom = raw[0].N
	}
	lo, hi := rawRange(raw, from, to)
	for i := lo; i < hi; i++ {
		take(raw[i].V)
	}
	// 原始层之前的部分：长期层
	if nanoOf(from) < rawFrom {
		fu, tu := unixCeil(from), time.Unix(0, rawFrom).Unix()-1
		if tu > to.Unix() {
			tu = to.Unix()
		}
		cb := s.coarseOf(metricID)
		clo := sort.Search(len(cb), func(i int) bool { return cb[i].T >= fu })
		for i := clo; i < len(cb) && cb[i].T <= tu; i++ {
			take(cb[i].V)
		}
		pk := s.peaks[metricID]
		plo := sort.Search(len(pk), func(i int) bool { return pk[i].T >= fu })
		for i := plo; i < len(pk) && pk[i].T <= tu; i++ {
			take(pk[i].P)
		}
	}
	return best, ok
}

// Last 返回某指标最后一个点。
func (s *Series) Last(metricID string) (point, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	buf := s.data[metricID]
	if len(buf) == 0 {
		return point{}, false
	}
	return buf[len(buf)-1].point(), true
}

// nearestRaw 在有序原始层里找离 target 最近的点。
func nearestRaw(buf []rpoint, target time.Time) (float64, time.Duration, bool) {
	n := len(buf)
	if n == 0 {
		return 0, 0, false
	}
	t := nanoOf(target)
	i := sort.Search(n, func(i int) bool { return buf[i].N >= t })
	dist := func(j int) time.Duration { return nanoDist(buf[j].N, t) }
	best := i
	if i == n || (i > 0 && dist(i-1) < dist(i)) {
		best = i - 1
	}
	return buf[best].V, dist(best), true
}

// Lookup 在两层里找离 target 最近的点，超出 tol 容差则返回 false。O(log N)。
// 签名与 deviation.Deviation.LookupFn 兼容。
func (s *Series) Lookup(metricID string, target time.Time, tol time.Duration) (float64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, d, ok := nearestRaw(s.raw(metricID), target)
	if vq, dq, okq := nearestCoarse(s.coarseOf(metricID), target); okq && (!ok || dq < d) {
		v, d, ok = vq, dq, true
	}
	if !ok || d > tol {
		return 0, false
	}
	return v, true
}

func nearestCoarse(buf []cpoint, target time.Time) (float64, time.Duration, bool) {
	n := len(buf)
	if n == 0 {
		return 0, 0, false
	}
	u := target.Unix()
	i := sort.Search(n, func(i int) bool { return buf[i].T >= u })
	dist := func(j int) time.Duration { return absDur(time.Unix(buf[j].T, 0).Sub(target)) }
	best := i
	if i == n || (i > 0 && dist(i-1) < dist(i)) {
		best = i - 1
	}
	return buf[best].V, dist(best), true
}

// Prune 删除最后一个点早于 before 的序列，返回被删除的 ID。
// 进程维度的序列（proc.cpu.<comm>）会随进程生灭出现又消失，不清理的话
// 序列数只增不减，σ 重算与转储的成本随运行时长线性上涨。
// 以两层中最新的点为准；长期层与峰值同时按保留期裁掉过老的点。
func (s *Series) Prune(before time.Time) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var gone []string
	for _, id := range s.idsLocked() {
		var last time.Time
		if b := s.data[id]; len(b) > 0 {
			last = time.Unix(0, b[len(b)-1].N)
		}
		if cb := s.coarse[id]; len(cb) > 0 {
			if c := time.Unix(cb[len(cb)-1].T, 0); c.After(last) {
				last = c
			}
		}
		if last.Before(before) {
			delete(s.data, id)
			delete(s.coarse, id)
			delete(s.peaks, id)
			delete(s.open, id)
			gone = append(gone, id)
			continue
		}
		cut := last.Add(-coarseRetention).Unix()
		if cb := s.coarse[id]; len(cb) > 0 {
			i := sort.Search(len(cb), func(i int) bool { return cb[i].T >= cut })
			if i > 0 {
				// 原地左移，保留容量：原来在这里 append 到一个新切片，每次都整条重新分配
				s.coarse[id] = cb[:copy(cb, cb[i:])]
			}
		}
		if pk := s.peaks[id]; len(pk) > 0 {
			i := sort.Search(len(pk), func(i int) bool { return pk[i].T >= cut })
			if i > 0 {
				s.peaks[id] = pk[:copy(pk, pk[i:])]
			}
		}
	}
	return gone
}

func (s *Series) idsLocked() []string {
	ids := make([]string, 0, len(s.data)+len(s.coarse))
	for id := range s.data {
		ids = append(ids, id)
	}
	for id := range s.coarse {
		if _, ok := s.data[id]; !ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// Count 返回全部指标的总点数（对外可见的），用于启动诊断。
func (s *Series) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for id := range s.data {
		n += len(s.raw(id))
	}
	for id := range s.coarse {
		n += len(s.coarseOf(id))
	}
	return n
}

// nanoDist 是 |a-b|，溢出时饱和（target 被 nanoOf 夹到边界时会碰到）。
func nanoDist(a, b int64) time.Duration {
	d := a - b
	if (a >= 0) != (b >= 0) && (d >= 0) != (a >= 0) { // 异号相减溢出
		return time.Duration(math.MaxInt64)
	}
	if d < 0 {
		d = -d
	}
	return time.Duration(d)
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// appendRawSamples / appendCoarseSamples 把整层直接写进 dst（复用调用方的缓冲），
// 给 σ 重算用。原来是 RawRange → []point → []deviation.Sample 转两道，
// 150 条序列每 5 分钟为此分配约 80MB。
func (s *Series) appendRawSamples(id string, dst []deviation.Sample) []deviation.Sample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.raw(id) {
		dst = append(dst, deviation.Sample{TS: time.Unix(0, p.N), Value: p.V})
	}
	return dst
}

func (s *Series) appendCoarseSamples(id string, dst []deviation.Sample) []deviation.Sample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.coarseOf(id) {
		dst = append(dst, deviation.Sample{TS: time.Unix(p.T, 0), Value: p.V})
	}
	return dst
}
