package main

import (
	"math/rand"
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
	"github.com/githubflyideas/nodedata/internal/deviation"
)

// TestShortLagsSurviveRestart：L1–L5 只用原始层，而原始层在内存里、重启即清空。
// 结果是每次重启后短档位瞎一个小时——可"突发抖动最先出现在 L1–L4"正是它们的用处，
// 重启后第一个小时恰恰最需要看。长期层是落盘的、5 分钟一点，
// 而 L1 的滞后正好 300 秒，相邻两点刚好配成一对，可以顶上。
func TestShortLagsSurviveRestart(t *testing.T) {
	// σ 按小时桶分别估计，now 取在小时末尾，这样原始层的两小时里有一整个小时落在同一桶
	now := time.Date(2026, 9, 16, 10, 59, 0, 0, time.UTC)
	r := rand.New(rand.NewSource(9))

	// 重启后的状态：长期层从磁盘装回 3 天，原始层只有刚采的 2 分钟
	s := NewSeries()
	var coarse []collector.Sample
	for ts := now.Add(-72 * time.Hour); ts.Before(now.Add(-2 * time.Minute)); ts = ts.Add(coarseStep) {
		coarse = append(coarse, collector.Sample{MetricID: "cpu.user", TS: ts, Value: 100 + r.NormFloat64()})
	}
	s.AddCoarse(coarse)
	for ts := now.Add(-2 * time.Minute); !ts.After(now); ts = ts.Add(5 * time.Second) {
		s.Add([]collector.Sample{{MetricID: "cpu.user", TS: ts, Value: 100 + r.NormFloat64()}})
	}
	b := NewHeatmapBuilder(s)
	b.RefreshSigma()

	ls := b.sigmaAt("cpu.user", 0, now)
	if !ls.Ready {
		t.Fatalf("重启后 L1 应能靠长期层顶上，实际未就绪（N=%d）", ls.N)
	}
	// L5 同理
	if ls5 := b.sigmaAt("cpu.user", 4, now); !ls5.Ready {
		t.Errorf("L5 也应就绪，N=%d", ls5.N)
	}

	// 原始层攒够跨度之后，应切回原始层（点更密，N 大得多）
	s2 := NewSeries()
	s2.AddCoarse(coarse)
	for ts := now.Add(-2 * time.Hour); !ts.After(now); ts = ts.Add(5 * time.Second) {
		s2.Add([]collector.Sample{{MetricID: "cpu.user", TS: ts, Value: 100 + r.NormFloat64()}})
	}
	b2 := NewHeatmapBuilder(s2)
	b2.RefreshSigma()
	full := b2.sigmaAt("cpu.user", 0, now)
	if full.N <= ls.N {
		t.Errorf("原始层攒够后 N 应远大于长期层顶替时的 N：%d vs %d", full.N, ls.N)
	}
	_ = deviation.MinBaselineSpan
}
