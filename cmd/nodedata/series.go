// series.go — 进程内时间序列缓冲区（L1 采集 → L2 转储 的中间层）。
// 不依赖 ClickHouse：保留最近 N 个采样点在内存中，供 heatmap 查询与静态转储使用。
//
// 性能约定（v3.0.9）：每条序列按时间非降序追加，Lookup / Range 一律二分。
// 早期版本 Lookup 是全缓冲线性扫描（17280 点），而 Build 对每个点、每个 lag 都要
// Lookup 一次 —— 满缓冲时单指标单窗口 ~0.37s，转储 5 个窗口 + 页面每 5s 一次
// /api/diagnosis，进程自己吃掉 1.5 个核。
package main

import (
	"sort"
	"sync"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
)

// maxPointsPerMetric 每个指标保留的最大原始点数。
// 5s 间隔 × 17280 点 ≈ 24 小时。
const maxPointsPerMetric = 17280

// 长期层：每 5 分钟从原始点里"抽"一个点（不求平均），保留 14 天，并落盘。
//
// 为什么抽点而不平均：z 比的是 Δ=v(t)-v(t-H) 与它的历史分布。抽出来的点仍是原始点，
// Δ 的分布与用原始数据算的完全同分布，只是样本少；平均会把 Δ 的离散度压小，σ 偏小、z 偏大。
// 为什么 14 天：对比表与服务表要能看到"14 天前"，而 14 天以前的数据基本不看。
// 它同时决定 z 的最长档位——一档需要 2×H 的历史，所以 14 天保留期对应最长 7 天档。
// 长期层用 16 字节的 cpoint（unix 秒 + float64）而不是 32 字节的 point，
// 14 天 × 5 分钟 = 4032 点 ≈ 64KB/序列，85 条序列约 5.5MB。
const (
	coarseStep      = 5 * time.Minute
	coarseRetention = 14 * 24 * time.Hour
	maxCoarsePoints = int(coarseRetention/coarseStep) + 12
)

// cpoint 是长期层的紧凑点。
type cpoint struct {
	T int64 // unix 秒
	V float64
}

func (c cpoint) point() point { return point{TS: time.Unix(c.T, 0), V: c.V} }

// point 是一个采样点。
type point struct {
	TS time.Time
	V  float64
}

// Series 是按 metricID 索引的时间序列缓冲区，并发安全。
// data 是原始层（24h），coarse 是长期层（5 分钟一点，14 天）。
type Series struct {
	mu     sync.RWMutex
	data   map[string][]point
	coarse map[string][]cpoint
}

func NewSeries() *Series {
	return &Series{data: make(map[string][]point, 256), coarse: make(map[string][]cpoint, 256)}
}

const coarseStepSec = int64(coarseStep / time.Second)

func coarseBucket(unix int64) int64 { return unix / coarseStepSec }

// Add 追加一批采样点，超出上限时丢弃最旧的点。
// 早于该序列最后一个点的样本直接丢弃（墙钟回拨），以保证二分查找的前提成立。
// 返回本批中被抽进长期层的样本（每指标每 5 分钟桶第一个点），由调用方落盘。
func (s *Series) Add(samples []collector.Sample) []collector.Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	var kept []collector.Sample
	for _, sm := range samples {
		buf := s.data[sm.MetricID]
		if n := len(buf); n > 0 && sm.TS.Before(buf[n-1].TS) {
			continue
		}
		buf = append(buf, point{TS: sm.TS, V: sm.Value})
		if len(buf) > maxPointsPerMetric {
			buf = buf[len(buf)-maxPointsPerMetric:]
		}
		s.data[sm.MetricID] = buf

		cb := s.coarse[sm.MetricID]
		if u := sm.TS.Unix(); len(cb) == 0 || coarseBucket(u) > coarseBucket(cb[len(cb)-1].T) {
			s.coarse[sm.MetricID] = appendCapped(cb, cpoint{T: u, V: sm.Value})
			kept = append(kept, sm)
		}
	}
	return kept
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

func appendCapped(buf []cpoint, p cpoint) []cpoint {
	buf = append(buf, p)
	if len(buf) > maxCoarsePoints {
		buf = buf[len(buf)-maxCoarsePoints:]
	}
	return buf
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

func sliceRange(buf []point, from, to time.Time) []point {
	lo := sort.Search(len(buf), func(i int) bool { return !buf[i].TS.Before(from) })
	hi := sort.Search(len(buf), func(i int) bool { return buf[i].TS.After(to) })
	if lo >= hi {
		return nil
	}
	return buf[lo:hi]
}

// Range 返回 [from, to] 区间内某指标的点（副本）。
// 原始层覆盖不到的更早部分由长期层补上，所以 7d/14d 窗口是真的 7 天/14 天。
func (s *Series) Range(metricID string, from, to time.Time) []point {
	s.mu.RLock()
	defer s.mu.RUnlock()
	raw := s.data[metricID]
	coarseTo := to.Unix()
	if len(raw) > 0 && raw[0].TS.Before(to) {
		coarseTo = unixCeil(raw[0].TS) - 1 // 长期层只补原始层覆盖不到的更早部分
	}
	c := coarseSlice(s.coarse[metricID], unixCeil(from), coarseTo)
	r := sliceRange(raw, from, to)
	return append(c, r...)
}

// RawRange 只取原始层。
func (s *Series) RawRange(metricID string, from, to time.Time) []point {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]point(nil), sliceRange(s.data[metricID], from, to)...)
}

// CoarseRange 只取长期层。
func (s *Series) CoarseRange(metricID string, from, to time.Time) []point {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return coarseSlice(s.coarse[metricID], unixCeil(from), to.Unix())
}

// Last 返回某指标最后一个点。
func (s *Series) Last(metricID string) (point, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	buf := s.data[metricID]
	if len(buf) == 0 {
		return point{}, false
	}
	return buf[len(buf)-1], true
}

// nearest 在有序缓冲里找离 target 最近的点。
func nearest(buf []point, target time.Time) (point, time.Duration, bool) {
	n := len(buf)
	if n == 0 {
		return point{}, 0, false
	}
	i := sort.Search(n, func(i int) bool { return !buf[i].TS.Before(target) })
	best := i
	if i == n || (i > 0 && absDur(target.Sub(buf[i-1].TS)) < absDur(buf[i].TS.Sub(target))) {
		best = i - 1
	}
	return buf[best], absDur(buf[best].TS.Sub(target)), true
}

// Lookup 在两层里找离 target 最近的点，超出 tol 容差则返回 false。O(log N)。
// 签名与 deviation.Deviation.LookupFn 兼容。
func (s *Series) Lookup(metricID string, target time.Time, tol time.Duration) (float64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, d, ok := nearest(s.data[metricID], target)
	if v, dq, okq := nearestCoarse(s.coarse[metricID], target); okq && (!ok || dq < d) {
		p, d, ok = point{V: v}, dq, true
	}
	if !ok || d > tol {
		return 0, false
	}
	return p.V, true
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
// 以两层中最新的点为准；长期层同时按保留期裁掉过老的点。
func (s *Series) Prune(before time.Time) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var gone []string
	for _, id := range s.idsLocked() {
		var last time.Time
		if b := s.data[id]; len(b) > 0 {
			last = b[len(b)-1].TS
		}
		if cb := s.coarse[id]; len(cb) > 0 {
			if c := time.Unix(cb[len(cb)-1].T, 0); c.After(last) {
				last = c
			}
		}
		if last.Before(before) {
			delete(s.data, id)
			delete(s.coarse, id)
			gone = append(gone, id)
			continue
		}
		if cb := s.coarse[id]; len(cb) > 0 {
			cut := last.Add(-coarseRetention).Unix()
			i := sort.Search(len(cb), func(i int) bool { return cb[i].T >= cut })
			if i > 0 {
				s.coarse[id] = append([]cpoint(nil), cb[i:]...)
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

// Count 返回全部指标的总点数，用于启动诊断。
func (s *Series) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, buf := range s.data {
		n += len(buf)
	}
	for _, buf := range s.coarse {
		n += len(buf)
	}
	return n
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
