package main

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
	"github.com/githubflyideas/nodedata/internal/deviation"
)

// 造一台"刚装上 nodedata"的机器：span 分钟的 5 秒采样，平稳，最后来一次正常的突发。
func freshHost(span time.Duration, now time.Time) *Series {
	r := rand.New(rand.NewSource(3))
	s := NewSeries()
	for ts := now.Add(-span); ts.Before(now); ts = ts.Add(5 * time.Second) {
		s.Add([]collector.Sample{{MetricID: "net.tx", TS: ts, Value: 1000 + r.NormFloat64()}})
	}
	// 桌面上极常见：浏览器开始下载，流量跳一个量级
	for _, off := range []time.Duration{-10 * time.Second, -5 * time.Second, 0} {
		s.Add([]collector.Sample{{MetricID: "net.tx", TS: now.Add(off), Value: 500000}})
	}
	return s
}

// TestYoungBaselineNotReady 复现"刚装上就一屏红"：
// 旧实现只要求"历史跨度 ≥ 该档时长"，L1=300s ⇒ 装上 5 分钟就开始出 z，
// 而那时基线只见过几分钟安静时段，之后任何正常波动都顶格
// （实测截图：十几行齐刷刷 ±6.00，广度 2——只有 L1/L2 就绪）。
func TestYoungBaselineNotReady(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 55, 0, 0, time.UTC)

	// 装上 15 分钟：L1 的跨度早已 ≥ 300s，但基线太年轻，不该出 z
	b := NewHeatmapBuilder(freshHost(15*time.Minute, now))
	b.RefreshSigma()
	if z := firstZ(b, now); z != nil {
		t.Errorf("装上 15 分钟就出 z=%.2f —— 基线只覆盖了几分钟安静时段", *z)
	}

	// 装满一小时之后才开始判定
	b2 := NewHeatmapBuilder(freshHost(70*time.Minute, now))
	b2.RefreshSigma()
	if firstZ(b2, now) == nil {
		t.Errorf("基线覆盖满 %v 后应开始出 z", deviation.MinBaselineSpan)
	}
}

func firstZ(b *HeatmapBuilder, now time.Time) *float64 {
	for _, m := range b.Latest(now).Metrics {
		if m.MetricID != "net.tx" {
			continue
		}
		if p := m.Points[len(m.Points)-1].Z[0]; p != nil {
			v := float64(*p) / 20
			return &v
		}
	}
	return nil
}

// L3 照常显示低置信档位（摆事实），L4 不拿它下结论。
func TestLowConfLagsExcludedFromL4(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 55, 0, 0, time.UTC)
	// 刚过一小时门槛：同时段样本还很少
	s := NewSeries()
	r := rand.New(rand.NewSource(7))
	for ts := now.Add(-65 * time.Minute); ts.Before(now); ts = ts.Add(3 * time.Minute) {
		s.Add([]collector.Sample{{MetricID: "net.tx", TS: ts, Value: 1000 + r.NormFloat64()}})
	}
	s.Add([]collector.Sample{{MetricID: "net.tx", TS: now, Value: 500000}})
	b := NewHeatmapBuilder(s)
	b.RefreshSigma()

	low := b.LowConfLags("net.tx", now)
	if !low[0] {
		t.Skip("这份数据的 L1 样本已经够多，低置信路径无从验证")
	}
	d := &Diagnoser{builder: b, series: s}
	for _, dev := range d.latestDeviationsAt(now) {
		if dev.MetricID == "net.tx" && !math.IsNaN(dev.Z[0]) {
			t.Errorf("L4 不该用 %d 个以下样本估出的 σ 下结论，却拿到 z=%.2f",
				deviation.HighConfN, dev.Z[0])
		}
	}
}
