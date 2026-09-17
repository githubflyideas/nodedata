package main

import (
	"math/rand"
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
)

// TestExclusionNeverStarvesShortLags：基线排除不能把"当前小时桶"剔空。
//
// 机器运行不满一天时，当前小时只有今天这一份样本，而异常起点通常就在最近一小时内，
// 于是这个桶被全部排除、N=0、档位变"未就绪"。表现是 L1–L5（用原始层）整片斜纹，
// 而 L6–L10（用跨天的长期层）正常——线上实测正是这个组合。
func TestExclusionNeverStarvesShortLags(t *testing.T) {
	now := time.Date(2026, 9, 17, 10, 30, 0, 0, time.UTC)
	for _, uptime := range []time.Duration{6 * time.Hour, 10 * time.Hour, 24 * time.Hour} {
		r := rand.New(rand.NewSource(1))
		s := NewSeries()
		var cs []collector.Sample
		for ts := now.Add(-72 * time.Hour); ts.Before(now.Add(-uptime)); ts = ts.Add(coarseStep) {
			cs = append(cs, collector.Sample{MetricID: "cpu.user", TS: ts, Value: 100 + r.NormFloat64()})
		}
		s.AddCoarse(cs)
		for ts := now.Add(-uptime); !ts.After(now); ts = ts.Add(5 * time.Second) {
			s.Add([]collector.Sample{{MetricID: "cpu.user", TS: ts, Value: 100 + r.NormFloat64()}})
		}
		b := NewHeatmapBuilder(s)
		b.RefreshSigma()
		if !b.sigmaAt("cpu.user", 0, now).Ready {
			t.Fatalf("uptime=%v 前提不成立：标记前 L1 就该就绪", uptime)
		}
		// 该指标进入 L4 结论
		b.MarkAnomaly([]string{"cpu.user"}, now)
		b.RefreshSigma()
		if ls := b.sigmaAt("cpu.user", 0, now); !ls.Ready {
			t.Errorf("uptime=%v：进入 L4 结论后 L1 变成未就绪（N=%d）——短档位整片失明", uptime, ls.N)
		}
		if ls := b.sigmaAt("cpu.user", 5, now); !ls.Ready {
			t.Errorf("uptime=%v：L6 也不该失明（N=%d）", uptime, ls.N)
		}
	}
}
