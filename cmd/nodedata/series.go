// series.go — 进程内时间序列缓冲区（L1 采集 → L2 转储 的中间层）。
// 不依赖 ClickHouse：保留最近 N 个采样点在内存中，供 heatmap 查询与静态转储使用。
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
func (s *Series) Add(samples []collector.Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sm := range samples {
		buf := s.data[sm.MetricID]
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
	out := make([]point, 0, len(buf))
	for _, p := range buf {
		if !p.TS.Before(from) && !p.TS.After(to) {
			out = append(out, p)
		}
	}
	return out
}

// Lookup 查找最接近 target 的点，超出 tol 容差则返回 false。
// 签名与 deviation.Deviation.LookupFn 兼容。
func (s *Series) Lookup(metricID string, target time.Time, tol time.Duration) (float64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	buf := s.data[metricID]
	if len(buf) == 0 {
		return 0, false
	}
	best := -1
	var bestDiff time.Duration
	for i, p := range buf {
		d := p.TS.Sub(target)
		if d < 0 {
			d = -d
		}
		if best < 0 || d < bestDiff {
			best, bestDiff = i, d
		}
	}
	if bestDiff > tol {
		return 0, false
	}
	return buf[best].V, true
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
