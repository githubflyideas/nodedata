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

// maxPointsPerMetric 每个指标保留的最大点数。
// 5s 间隔 × 17280 点 ≈ 24 小时。
const maxPointsPerMetric = 17280

// point 是一个采样点。
type point struct {
	TS time.Time
	V  float64
}

// Series 是按 metricID 索引的时间序列缓冲区，并发安全。
type Series struct {
	mu   sync.RWMutex
	data map[string][]point
}

func NewSeries() *Series {
	return &Series{data: make(map[string][]point, 256)}
}

// Add 追加一批采样点，超出上限时丢弃最旧的点。
// 早于该序列最后一个点的样本直接丢弃（墙钟回拨），以保证二分查找的前提成立。
func (s *Series) Add(samples []collector.Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	}
}

// MetricIDs 返回当前已知的全部指标 ID（已排序，保证输出稳定）。
func (s *Series) MetricIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.data))
	for id := range s.data {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Range 返回 [from, to] 区间内某指标的点（副本，调用方可安全持有）。
func (s *Series) Range(metricID string, from, to time.Time) []point {
	s.mu.RLock()
	defer s.mu.RUnlock()
	buf := s.data[metricID]
	lo := sort.Search(len(buf), func(i int) bool { return !buf[i].TS.Before(from) })
	hi := sort.Search(len(buf), func(i int) bool { return buf[i].TS.After(to) })
	if lo >= hi {
		return nil
	}
	out := make([]point, hi-lo)
	copy(out, buf[lo:hi])
	return out
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

// Lookup 查找最接近 target 的点，超出 tol 容差则返回 false。O(log N)。
// 签名与 deviation.Deviation.LookupFn 兼容。
func (s *Series) Lookup(metricID string, target time.Time, tol time.Duration) (float64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	buf := s.data[metricID]
	n := len(buf)
	if n == 0 {
		return 0, false
	}
	i := sort.Search(n, func(i int) bool { return !buf[i].TS.Before(target) })
	best := i
	if i == n || (i > 0 && absDur(target.Sub(buf[i-1].TS)) < absDur(buf[i].TS.Sub(target))) {
		best = i - 1
	}
	if absDur(buf[best].TS.Sub(target)) > tol {
		return 0, false
	}
	return buf[best].V, true
}

// Prune 删除最后一个点早于 before 的序列，返回被删除的 ID。
// 进程维度的序列（proc.cpu.<comm>）会随进程生灭出现又消失，不清理的话
// 序列数只增不减，σ 重算与转储的成本随运行时长线性上涨。
func (s *Series) Prune(before time.Time) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var gone []string
	for id, buf := range s.data {
		if len(buf) == 0 || buf[len(buf)-1].TS.Before(before) {
			delete(s.data, id)
			gone = append(gone, id)
		}
	}
	return gone
}

// Count 返回全部指标的总点数，用于启动诊断。
func (s *Series) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, buf := range s.data {
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
