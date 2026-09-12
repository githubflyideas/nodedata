package main

// 页面级回归：整张 z 表不能通红。
//
// 用一批"长得像真实节点"的合成指标喂进去（恒速累计计数器、整数抖动、恒 0、
// 慢涨的 page cache、带噪声的利用率），其中只注入一个真异常。
// 早期算法在这种输入上几乎每一格都是 ±6（z=Δ/σ 少了减去 Δ 的历史中位数，
// 有稳定漂移的指标恒定顶格；整数指标因 MAD=0 落到 1/6 硬地板，动一格就是 6）。
// 这个测试要求：正常指标的红格比例极低，而注入的异常必须被抓到。

import (
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
	"github.com/githubflyideas/nodedata/internal/deviation"
)

func TestPageLevelZIsNotAllRed(t *testing.T) {
	const (
		stepSec = 5
		spanMin = 120
	)
	n := spanMin * 60 / stepSec
	end := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	start := end.Add(-time.Duration(n*stepSec) * time.Second)

	// 正常指标：各自的"平时长什么样"。
	normal := map[string]func(i int) float64{
		"net.rx":        func(i int) float64 { return float64(i) * 6e6 },            // 恒速计数器
		"net.tx":        func(i int) float64 { return float64(i) * 2e6 },            // 恒速计数器
		"disk.inflight": func(i int) float64 { return float64(i) * 37 },             // 恒速计数器
		"mem.cached":    func(i int) float64 { return 8.9e9 + float64(i)*1e5 },      // 慢涨
		"slab":          func(i int) float64 { return 9.0e8 + float64(i%13)*1e6 },   // 锯齿
		"mem.free":      func(i int) float64 { return 2.7e9 - float64(i%97)*3e6 },   // 上下波动
		"procs_running": func(i int) float64 { return float64((i * 7919) % 5) },     // 小整数乱跳
		"proc.cpu.a":    func(i int) float64 { return float64((i * 31) % 3) },       // 整数 0/1/2
		"proc.cpu.b":    func(i int) float64 { return 0 },                           // 恒 0
		"proc.cpu.c":    func(i int) float64 { return float64(i / 60) },             // 每分钟 +1
		"loadavg.1m":    func(i int) float64 { return 0.4 + float64(i%23)*0.01 },    // 浮点小波动
		"disk.wiops":    func(i int) float64 { return 7.6 + float64((i*13)%9)*0.1 }, // 浮点噪声
	}
	// 注入异常：这个指标在最后一段时间里增速翻了 40 倍。
	const anomalous = "disk.await"

	s := NewSeries()
	var batch []collector.Sample
	for i := 0; i < n; i++ {
		ts := start.Add(time.Duration(i*stepSec) * time.Second)
		for id, f := range normal {
			batch = append(batch, collector.Sample{MetricID: id, TS: ts, Value: f(i)})
		}
		v := 2.0 + float64(i%11)*0.05
		if i > n-40 { // 最后约 3 分钟真的坏了
			v = 2.0 + float64(i-(n-40))*4
		}
		batch = append(batch, collector.Sample{MetricID: anomalous, TS: ts, Value: v})
	}
	s.Add(batch)

	b := NewHeatmapBuilder(s)
	b.RefreshSigma()
	hm, err := b.Build(start, end)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	red, total := 0, 0
	redBy := map[string]int{}
	anomalyFlagged := false
	for _, mp := range hm.Metrics {
		for _, p := range mp.Points {
			for _, zp := range p.Z {
				if zp == nil {
					continue // 斜纹格：历史没攒够，不参与统计
				}
				total++
				z := float64(*zp) / 20
				if z >= 3 || z <= -3 {
					red++
					redBy[mp.MetricID]++
					if mp.MetricID == anomalous {
						anomalyFlagged = true
					}
				}
			}
		}
	}
	if total == 0 {
		t.Fatal("没有任何已就绪的 z 格，测试前提不成立")
	}
	if !anomalyFlagged {
		t.Errorf("注入的异常指标 %s 没被抓到（红格分布：%v）", anomalous, redBy)
	}
	// 正常指标的红格必须是零星的。早期算法在同样输入下会接近 100%。
	normalRed := red - redBy[anomalous]
	frac := float64(normalRed) / float64(total)
	if frac > 0.02 {
		t.Errorf("正常指标的红格比例 %.1f%%（%d/%d）过高，整张表又通红了；分布：%v",
			frac*100, normalRed, total, redBy)
	}
	t.Logf("已就绪格 %d，红格 %d（其中注入异常 %d），正常指标红格比例 %.2f%%",
		total, red, redBy[anomalous], frac*100)

	// 顺带确认 σ 表里存的是位置+尺度两件事，不是只有尺度。
	ls := deviation.LagSigma{}
	_ = ls.Center
}
