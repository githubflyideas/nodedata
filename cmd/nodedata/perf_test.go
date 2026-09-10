package main

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
)

// TestBuildCostFullBuffer 是 v3.0.9 的回归护栏：原始层灌满 24h、长期层灌满 56 天后，
// 一轮转储（5 个窗口）和一次 σ 重算都必须远低于采集周期。
//
// v3.0.8 在这个用例上：Build 每指标每窗口 ~370ms（Series.Lookup 线性扫 17280 点，
// 且每个点每个 lag 调一次），20 个指标一轮转储 ~37s —— 转储循环永不停歇，
// 页面每 5s 一次 /api/diagnosis 再叠一份，整进程 150% 核。
func TestBuildCostFullBuffer(t *testing.T) {
	if testing.Short() {
		t.Skip("fills a 24h buffer")
	}
	const M = 20
	s := NewSeries()
	end := time.Now().Truncate(5 * time.Second)
	start := end.Add(-time.Duration(maxPointsPerMetric) * 5 * time.Second)
	r := rand.New(rand.NewSource(1))
	batch := make([]collector.Sample, M)
	for k := 0; k < maxPointsPerMetric; k++ {
		ts := start.Add(time.Duration(k) * 5 * time.Second)
		for m := 0; m < M; m++ {
			batch[m] = collector.Sample{MetricID: fmt.Sprintf("cpu.m%02d", m), TS: ts, Value: 100 + r.NormFloat64()}
		}
		s.Add(batch)
	}
	// 长期层灌满 56 天（在原始层之前）
	cstart := start.Add(-coarseRetention)
	for m := 0; m < M; m++ {
		id := fmt.Sprintf("cpu.m%02d", m)
		cs := make([]collector.Sample, 0, maxCoarsePoints)
		for ts := cstart; ts.Before(start); ts = ts.Add(coarseStep) {
			cs = append(cs, collector.Sample{MetricID: id, TS: ts, Value: 100 + r.NormFloat64()})
		}
		s.AddCoarse(cs)
	}
	b := NewHeatmapBuilder(s)
	newest := start.Add(time.Duration(maxPointsPerMetric-1) * 5 * time.Second).Unix()

	t0 := time.Now()
	b.RefreshSigma()
	sig := time.Since(t0)

	t1 := time.Now()
	for _, w := range []string{"1h", "6h", "24h", "7d", "30d"} {
		d, _ := windowDuration(w)
		hm, _ := b.Build(end.Add(-d), end)
		if len(hm.Metrics) != M {
			t.Fatalf("window %s: %d metrics", w, len(hm.Metrics))
		}
		// 降采样后最后一个点必须是真正的最新点（表格"当前值"读它）。
		pts := hm.Metrics[0].Points
		if pts[len(pts)-1].TS != newest {
			t.Fatalf("window %s: last point %d != newest %d", w, pts[len(pts)-1].TS, newest)
		}
	}
	dump := time.Since(t1)
	t.Logf("M=%d full raw 24h + coarse 56d: RefreshSigma %v, one dump round (5 windows) %v", M, sig, dump)

	// 预算放得很宽（-race 下也要过），但比旧实现低两个数量级。
	if dump > 2*time.Second {
		t.Fatalf("dump round took %v; Build is O(points×lags×log N), should be ~100ms", dump)
	}
	if sig > 10*time.Second {
		t.Fatalf("RefreshSigma took %v", sig)
	}
}
