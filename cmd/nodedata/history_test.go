package main

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
	"github.com/githubflyideas/nodedata/internal/deviation"
)

// TestLongLagsSurviveRestart 是长档位（L9=1d、L10=7d）的端到端验收：
// 16 天历史经 Series.Add → History 落盘；然后"重启"（新 Series，只从磁盘装回），
// 再采几轮。要求：
//  1. L9/L10 在重启后立即就绪（不必重新攒历史）；
//  2. 正常指标（有日周期 + 噪声）在长 lag 上不报；
//  3. 一个"缓慢劣化"的指标 —— 一直平稳，最近 3 天每天涨 5%，每 5 分钟几乎不变，
//     短 lag 上看不出来 —— 在 L10（7 天）上必须被抓到。
//
// 注意 z 的语义：它比的是"这次的 Δ"与"同 lag 的 Δ 平时是多少"。一个一直匀速上涨的
// 指标（每天都涨 1%），7 天 Δ 平时就是 +7%，z≈0 —— 这是 v3.0.8 起有意的设计
// （减中位数，把"平时就在涨"排除掉）。长 lag 抓的是"行为变了"，不是"一直在涨"；
// 容量类问题（磁盘按固定速率写满）归 L0 的绝对判定。
func TestLongLagsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	hist, err := NewHistory(dir, coarseRetention)
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewSource(3))
	end := time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC)
	start := end.Add(-16 * 24 * time.Hour)
	daily := func(ts time.Time) float64 { // 日周期
		return 30 * math.Sin(2*math.Pi*float64(ts.Unix()%86400)/86400)
	}
	value := func(id string, ts time.Time) float64 {
		base := 200 + daily(ts) + r.NormFloat64()*3
		if id == "cpu.user" {
			return base
		}
		// mem.used：一直平稳，最近 3 天每天涨 5%
		if d := ts.Sub(end.Add(-72*time.Hour)).Hours() / 24; d > 0 {
			return base * math.Pow(1.05, d)
		}
		return base
	}

	s := NewSeries()
	// 前 29 天：5 分钟一轮（每轮都开新桶，全部进长期层并落盘）
	ts := start
	for ; ts.Before(end.Add(-24 * time.Hour)); ts = ts.Add(coarseStep) {
		b := []collector.Sample{{MetricID: "cpu.user", TS: ts, Value: value("cpu.user", ts)},
			{MetricID: "mem.used", TS: ts, Value: value("mem.used", ts)}}
		if err := hist.Append(s.Add(b)); err != nil {
			t.Fatal(err)
		}
	}
	// 最后 24 小时：真实的 5 秒采集
	for ; !ts.After(end); ts = ts.Add(5 * time.Second) {
		b := []collector.Sample{{MetricID: "cpu.user", TS: ts, Value: value("cpu.user", ts)},
			{MetricID: "mem.used", TS: ts, Value: value("mem.used", ts)}}
		if err := hist.Append(s.Add(b)); err != nil {
			t.Fatal(err)
		}
	}
	hist.Close()

	// ── 重启：内存全丢，只从磁盘装回
	s2 := NewSeries()
	h2, _ := NewHistory(dir, coarseRetention)
	lines, pts, err := h2.Load(s2, end)
	if err != nil || lines < 3500 {
		t.Fatalf("load: lines=%d pts=%d err=%v", lines, pts, err)
	}
	// 重启后再采一分钟
	for ts = end.Add(5 * time.Second); !ts.After(end.Add(time.Minute)); ts = ts.Add(5 * time.Second) {
		s2.Add([]collector.Sample{{MetricID: "cpu.user", TS: ts, Value: value("cpu.user", ts)},
			{MetricID: "mem.used", TS: ts, Value: value("mem.used", ts)}})
	}
	now := end.Add(time.Minute)

	b := NewHeatmapBuilder(s2)
	b.RefreshSigma()
	hm := b.Latest(now)
	z := map[string][deviation.NLag]*int8{}
	for _, m := range hm.Metrics {
		z[m.MetricID] = m.Points[len(m.Points)-1].Z
	}
	zf := func(id string, lag int) float64 {
		p := z[id][lag-1]
		if p == nil {
			return math.NaN()
		}
		return float64(*p) / 20
	}
	for lag := 9; lag <= 10; lag++ {
		if math.IsNaN(zf("cpu.user", lag)) {
			t.Fatalf("L%d not ready after restart — long-term tier was not used", lag)
		}
		if a := math.Abs(zf("cpu.user", lag)); a >= 3 {
			t.Errorf("normal metric flagged at L%d: z=%.2f", lag, zf("cpu.user", lag))
		}
	}
	// 原始层随重启丢了，L1 在一分钟后理应还没就绪 —— 这正是分层的含义
	if !math.IsNaN(zf("cpu.user", 1)) {
		t.Errorf("L1 should not be ready one minute after restart")
	}
	if zf("mem.used", 10) < 3 {
		t.Errorf("slow degradation missed at L10: z=%.2f", zf("mem.used", 10))
	}
	for _, id := range []string{"cpu.user", "mem.used"} {
		t.Logf("after restart %-8s L1..L10: %v", id, fmtZ(zf, id))
	}

	// 14d 窗口是真的 14 天
	full, _ := b.Build(now.Add(-14*24*time.Hour), now)
	for _, m := range full.Metrics {
		if first := time.Unix(m.Points[0].TS, 0); now.Sub(first) < 13*24*time.Hour {
			t.Errorf("%s: 14d window only reaches back %v", m.MetricID, now.Sub(first))
		}
	}
}

func TestHistoryTruncatedLineAndRetention(t *testing.T) {
	dir := t.TempDir()
	h, _ := NewHistory(dir, coarseRetention)
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	// 一个 70 天前的文件，应在轮转时被删、加载时被跳过
	old := filepath.Join(dir, dayOf(now.Add(-70*24*time.Hour))+".jsonl")
	os.WriteFile(old, []byte(`{"t":1,"v":{"x":1}}`+"\n"), 0o644)

	for i := 0; i < 3; i++ {
		ts := now.Add(time.Duration(i) * coarseStep)
		h.Append([]collector.Sample{{MetricID: "cpu.user", TS: ts, Value: float64(i)},
			{MetricID: "bad", TS: ts, Value: math.NaN()}})
	}
	h.Close()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("expired day file not removed")
	}
	// 模拟崩溃：追加半行
	f, _ := os.OpenFile(filepath.Join(dir, dayOf(now)+".jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"t":17890`)
	f.Close()

	s := NewSeries()
	lines, pts, err := h.Load(s, now)
	if err != nil || lines != 3 || pts != 3 {
		t.Fatalf("lines=%d pts=%d err=%v (NaN must be dropped, half line skipped)", lines, pts, err)
	}
	if got := s.CoarseRange("cpu.user", time.Time{}, farFuture); len(got) != 3 || got[2].V != 2 {
		t.Fatalf("coarse = %+v", got)
	}
}

func fmtZ(zf func(string, int) float64, id string) string {
	out := ""
	for lag := 1; lag <= deviation.NLag; lag++ {
		v := zf(id, lag)
		if math.IsNaN(v) {
			out += "   — "
		} else {
			out += fmt.Sprintf("%+5.1f", v)
		}
	}
	return out
}
