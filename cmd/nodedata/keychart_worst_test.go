package main

import (
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
)

// 24 小时窗口一步 6 分钟，等于 36 个原始点里取 1 个。一次 40 秒的磁盘写等待尖峰
// 落在两个取样点之间：曲线本身看不到它，最坏值必须把它带出来。
func TestKeySeriesWorstShowsSkippedSpike(t *testing.T) {
	s := NewSeries()
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	spikeAt := now.Add(-10*time.Hour + 2*time.Minute) // 刻意避开取样时刻
	for ts := now.Add(-24 * time.Hour); !ts.After(now); ts = ts.Add(10 * time.Second) {
		v := 3.0
		if !ts.Before(spikeAt) && ts.Before(spikeAt.Add(40*time.Second)) {
			v = 45
		}
		s.Add([]collector.Sample{{MetricID: "disk.await_w", TS: ts, Value: v}})
	}
	ks := NewHeatmapBuilder(s).KeySeries("24h", 24*time.Hour, now)
	for _, g := range ks.Groups {
		for _, cs := range g.Series {
			if cs.ID != "disk.await_w" {
				continue
			}
			sawInLine, sawWorst := false, 0
			for i, v := range cs.Values {
				if v != nil && *v > 40 {
					sawInLine = true
				}
				if i < len(cs.Worst) && cs.Worst[i] != nil && *cs.Worst[i] == 45 {
					sawWorst++
				}
			}
			if sawInLine {
				t.Skip("取样点恰好落在尖峰上，这个夹具没测到要测的情况")
			}
			if sawWorst != 1 {
				t.Fatalf("曲线跳过了尖峰，最坏值应在恰好一步里带出 45，实得 %d 步", sawWorst)
			}
			return
		}
	}
	t.Fatal("没找到写延迟那条曲线")
}

// 安静的曲线不传 w：整条都没有值得画的尖峰就不占传输。
func TestKeySeriesWorstOmittedWhenQuiet(t *testing.T) {
	s := NewSeries()
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	for ts := now.Add(-24 * time.Hour); !ts.After(now); ts = ts.Add(10 * time.Second) {
		s.Add([]collector.Sample{{MetricID: "disk.await_w", TS: ts, Value: 3}})
	}
	for _, g := range NewHeatmapBuilder(s).KeySeries("24h", 24*time.Hour, now).Groups {
		for _, cs := range g.Series {
			if cs.ID == "disk.await_w" && cs.Worst != nil {
				t.Errorf("平稳的曲线不该带 w")
			}
		}
	}
}
