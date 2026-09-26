package main

import (
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// USE 层的回放：跟故障语料同一套生成器（8 天长期层 + 3 小时原始层），
// 但评估的是页面和巡视台看到的 USE 五行，而不是 L4 诊断。

type useReplay struct {
	evals       int
	oldYellow   int // 旧规则（|z|≥3 就算偏离）会亮黄的次数
	newYellow   int // 新规则（坏方向 + 显著 + 持续在瓶颈区间）
	oldExamples []string
	final       []UseRow
}

func replayUse(sc scenario, cores int, evalFrom time.Duration) useReplay {
	r := rand.New(rand.NewSource(7))
	now := time.Date(2026, 9, 11, 14, 30, 0, 0, time.UTC)
	faultStart := now.Add(-sc.faultLen)
	rawStart := now.Add(-3 * time.Hour)
	s := NewSeries()
	for ts := rawStart.Add(-8 * 24 * time.Hour); ts.Before(rawStart); ts = ts.Add(coarseStep) {
		s.AddCoarse(toSamples(sc.values(ts, faultStart, r), ts))
	}
	b := NewHeatmapBuilder(s)
	d := &Diagnoser{builder: b, series: s, cores: cores, dataDir: "/nonexistent"}
	var out useReplay
	lastSigma := time.Time{}
	for ts := rawStart; !ts.After(now); ts = ts.Add(5 * time.Second) {
		s.Add(toSamples(sc.values(ts, faultStart, r), ts))
		if ts.Sub(lastSigma) >= 5*time.Minute {
			b.RefreshSigma()
			lastSigma = ts
		}
		if ts.Sub(rawStart) >= evalFrom && ts.Sub(rawStart)%(30*time.Second) == 0 {
			out.evals++
			rows := d.Use(ts)
			old, nw := false, false
			for _, row := range rows {
				for _, f := range row.Facts {
					if math.Abs(f.Z) >= useZThreshold {
						if !old && len(out.oldExamples) < 5 {
							out.oldExamples = append(out.oldExamples, ts.Format("15:04")+" "+row.Resource+" "+f.Label)
						}
						old = true
					}
					if f.Notable {
						nw = true
					}
				}
			}
			if old {
				out.oldYellow++
			}
			if nw {
				out.newYellow++
			}
		}
	}
	out.final = d.Use(now)
	return out
}

func rowOfUse(rows []UseRow, name string) UseRow {
	for _, r := range rows {
		if r.Resource == name {
			return r
		}
	}
	return UseRow{}
}

// 大部分时间机器是正常的。一台正常运行 3 小时的机器，墙上一次都不该亮黄。
// 同一次回放里同时数旧规则会亮几次，写进日志——这是改动的理由，不是装饰。
func TestUseQuietMachineStaysGreen(t *testing.T) {
	if testing.Short() {
		t.Skip("replays 8 days")
	}
	res := replayUse(scenario{name: "正常"}, 8, time.Hour)
	t.Logf("正常机器 %d 次评估：旧规则亮黄 %d 次（%.1f%%），新规则 %d 次；旧规则的例子：%s",
		res.evals, res.oldYellow, 100*float64(res.oldYellow)/float64(res.evals), res.newYellow, strings.Join(res.oldExamples, "；"))
	if res.newYellow != 0 {
		t.Errorf("正常机器不应亮黄，新规则亮了 %d 次", res.newYellow)
	}
}

// 用户截图里的情况：机器变闲了（运行队列 4→1、CPU 150%→10%），不是问题。
func TestUseIdleDropIsNotDeviation(t *testing.T) {
	if testing.Short() {
		t.Skip("replays 8 days")
	}
	sc := scenario{name: "变闲", faultLen: 20 * time.Minute, mods: map[string]mod{
		"cpu.user":       set(10, 1),
		"procs_running":  set(1, 0),
		"psi.cpu.some10": set(0.1, 0.05),
		"loadavg.1m":     set(0.3, 0.05),
	}}
	res := replayUse(sc, 8, 2*time.Hour+30*time.Minute)
	cpu := rowOfUse(res.final, "CPU")
	if cpu.Level != 0 || cpu.State != "正常" {
		t.Fatalf("变闲不是问题，CPU 应为正常：%s level=%d", cpu.State, cpu.Level)
	}
	lower := false
	for _, f := range cpu.Facts {
		if f.Notable {
			t.Errorf("%s 不应被标成偏离", f.Label)
		}
		if f.Change == "低于平时" {
			lower = true
		}
	}
	if !lower {
		t.Errorf("变化照样要展示：应有一条'低于平时'：%+v", cpu.Facts)
	}
	t.Logf("变闲：旧规则亮黄 %d/%d 次，新规则 %d 次", res.oldYellow, res.evals, res.newYellow)
}

// 真的瓶颈：8 核机器上跑满 5 核、排队 14 个、CPU 压力 30%——必须报。
func TestUseRealBottleneckIsDeviation(t *testing.T) {
	if testing.Short() {
		t.Skip("replays 8 days")
	}
	sc := scenario{name: "CPU 打满", faultLen: 5 * time.Minute, mods: map[string]mod{
		"cpu.user":       set(650, 10),
		"procs_running":  set(14, 1),
		"psi.cpu.some10": set(30, 2),
	}}
	res := replayUse(sc, 8, 2*time.Hour+50*time.Minute)
	cpu := rowOfUse(res.final, "CPU")
	if cpu.Level < 1 {
		t.Fatalf("真瓶颈应报偏离：%s %+v", cpu.State, cpu.Facts)
	}
	n := 0
	for _, f := range cpu.Facts {
		if f.Notable {
			n++
		}
	}
	if n == 0 {
		t.Errorf("应有标为偏离的读数：%+v", cpu.Facts)
	}
}

// 一闪而过：30 秒的尖峰进了区间，但没持续满 60 秒，不点亮墙上的方块。
func TestUseShortSpikeIsNotDeviation(t *testing.T) {
	if testing.Short() {
		t.Skip("replays 8 days")
	}
	sc := scenario{name: "尖峰", faultLen: 30 * time.Second, mods: map[string]mod{
		"cpu.user":       set(650, 10),
		"procs_running":  set(14, 1),
		"psi.cpu.some10": set(30, 2),
	}}
	res := replayUse(sc, 8, 2*time.Hour+59*time.Minute)
	if cpu := rowOfUse(res.final, "CPU"); cpu.Level != 0 {
		t.Errorf("30 秒的尖峰不应点亮：%s %+v", cpu.State, cpu.Facts)
	}
}

// 区间表本身：几条关键的线，改的时候这里会提醒。
func TestConcernTable(t *testing.T) {
	c := concernCtx{cores: 2, last: func(string) (float64, bool) { return 0, false }}
	cases := []struct {
		id string
		v  float64
		in bool
	}{
		{"cpu.core_top1", 59, false}, {"cpu.core_top1", 60, true},
		{"procs_running", 2, false}, {"procs_running", 3, true}, // 2 核：排到第 3 个才叫饱和
		{"cpu.busy_pct", 0.5, false},
		{"net.rx_drop", 0, false}, {"net.rx_drop", 1, true},
		{"net.rx", 1e9, false}, // 吞吐不知道链路速率：只展示
	}
	for _, x := range cases {
		if in, known := inConcern(x.id, x.v, c); in != x.in || !known {
			t.Errorf("%s=%v 应为 in=%v（得 in=%v known=%v）", x.id, x.v, x.in, in, known)
		}
	}
	if _, known := inConcern("some.unregistered", 1, c); known {
		t.Error("没定义区间的指标应返回 known=false")
	}
}

// 真正安静的机器：整机忙碌长期在 1.1% ± 0.02 抖，最近掉到 0.5%。
// Δ 的稳健尺度只有零点零几个百分点，光看 σ 会算出很大的 z；挡住它的是 z 自带的最小变化量门槛
// （不到 3 个百分点的变化 z 记 0）。v5.25 曾想另加 σ 下限，验证后发现被这道门槛完全覆盖，没加。
// 这个测试钉住这道门槛：谁把它拆了，安静机器就会开始误报。
func TestQuietMetricTinyChangeGivesNoZ(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	now := time.Date(2026, 9, 11, 14, 30, 0, 0, time.UTC)
	rawStart := now.Add(-3 * time.Hour)
	s := NewSeries()
	v := func(ts time.Time) float64 {
		if ts.After(now.Add(-10 * time.Minute)) {
			return 0.5 + r.NormFloat64()*0.02
		}
		return 1.1 + r.NormFloat64()*0.02
	}
	for ts := rawStart.Add(-8 * 24 * time.Hour); ts.Before(rawStart); ts = ts.Add(coarseStep) {
		s.AddCoarse(toSamples(map[string]float64{"cpu.busy_pct": v(ts)}, ts))
	}
	b := NewHeatmapBuilder(s)
	d := &Diagnoser{builder: b, series: s, cores: 2}
	last := time.Time{}
	for ts := rawStart; !ts.After(now); ts = ts.Add(5 * time.Second) {
		s.Add(toSamples(map[string]float64{"cpu.busy_pct": v(ts)}, ts))
		if ts.Sub(last) >= 5*time.Minute {
			b.RefreshSigma()
			last = ts
		}
	}
	for _, dv := range d.latestDeviationsAt(now) {
		if dv.MetricID != "cpu.busy_pct" {
			continue
		}
		for i, z := range dv.Z {
			if !math.IsNaN(z) && math.Abs(z) >= useZThreshold {
				t.Errorf("1.1%%→0.5%% 不到一个最小变化量（3 个百分点），|z| 不该到 3：档 %d z=%.1f", i, z)
			}
		}
		return
	}
	t.Fatal("没算出 cpu.busy_pct 的偏离度")
}

// 方向单独的保护：机器仍然很忙（8 核上排队 12 个，仍在饱和区间），但比平时的 30 个好多了。
// 区间这道闸拦不住它（12 > 8），只有"往坏的方向变才算"能拦住——好转不是偏离。
func TestUseImprovingButStillBusyIsNotDeviation(t *testing.T) {
	r := rand.New(rand.NewSource(5))
	now := time.Date(2026, 9, 11, 14, 30, 0, 0, time.UTC)
	rawStart := now.Add(-3 * time.Hour)
	s := NewSeries()
	v := func(ts time.Time) float64 {
		if ts.After(now.Add(-20 * time.Minute)) {
			return math.Round(12 + r.NormFloat64())
		}
		return math.Round(30 + r.NormFloat64()*2)
	}
	for ts := rawStart.Add(-8 * 24 * time.Hour); ts.Before(rawStart); ts = ts.Add(coarseStep) {
		s.AddCoarse(toSamples(map[string]float64{"procs_running": v(ts)}, ts))
	}
	b := NewHeatmapBuilder(s)
	d := &Diagnoser{builder: b, series: s, cores: 8, dataDir: "/nonexistent"}
	last := time.Time{}
	for ts := rawStart; !ts.After(now); ts = ts.Add(5 * time.Second) {
		s.Add(toSamples(map[string]float64{"procs_running": v(ts)}, ts))
		if ts.Sub(last) >= 5*time.Minute {
			b.RefreshSigma()
			last = ts
		}
	}
	cpu := rowOfUse(d.Use(now), "CPU")
	lower := false
	for _, f := range cpu.Facts {
		if f.ID == "procs_running" {
			if f.Notable {
				t.Errorf("排队从 30 降到 12 是好转，不应标偏离（z=%.1f）", f.Z)
			}
			lower = f.Change == "低于平时"
		}
	}
	if !lower {
		t.Errorf("应展示'低于平时'：%+v", cpu.Facts)
	}
	if cpu.Level != 0 {
		t.Errorf("CPU 行不应变黄：%s", cpu.State)
	}
}

// 绝对线：一个核被钉死在 100%，没有任何历史（z 没就绪）也要报——这就是"绝对指标是干啥的"。
// 但一闪而过的（不满 60 秒）不报。
func TestUseHardLineWithoutHistory(t *testing.T) {
	now := time.Date(2026, 9, 11, 14, 30, 0, 0, time.UTC)
	run := func(pinnedFor time.Duration) UseRow {
		s := NewSeries()
		for ts := now.Add(-5 * time.Minute); !ts.After(now); ts = ts.Add(5 * time.Second) {
			v := 3.0
			if !ts.Before(now.Add(-pinnedFor)) {
				v = 100
			}
			s.Add(toSamples(map[string]float64{"cpu.core_top1": v, "cpu.busy_pct": v / 2}, ts))
		}
		d := &Diagnoser{builder: NewHeatmapBuilder(s), series: s, cores: 2, dataDir: "/nonexistent"}
		return rowOfUse(d.Use(now), "CPU")
	}
	cpu := run(2 * time.Minute)
	if cpu.Level < 1 {
		t.Fatalf("单核钉死 2 分钟、没有历史，也应报偏离：%s %+v", cpu.State, cpu.Facts)
	}
	found := false
	for _, f := range cpu.Facts {
		if f.ID == "cpu.core_top1" && f.Notable && strings.Contains(f.Note, "绝对线") {
			found = true
		}
	}
	if !found {
		t.Errorf("应写明是绝对线：%+v", cpu.Facts)
	}
	if cpu := run(30 * time.Second); cpu.Level != 0 {
		t.Errorf("只钉了 30 秒不应报：%s", cpu.State)
	}
}
