package main

import (
	"math"
	"testing"
	"time"
	"unsafe"

	"github.com/githubflyideas/nodedata/internal/collector"
)

func TestRawPointIs16Bytes(t *testing.T) {
	if n := unsafe.Sizeof(rpoint{}); n != 16 {
		t.Errorf("原始层每点 %d 字节，应为 16", n)
	}
}

// 容量有上限、不反复分配：跑 3 倍容量的点，底层数组容量不超过上限，
// 对外只看得到最新的 maxPointsPerMetric 个，而且是按时间连续的最新那些。
func TestRawBufferBoundedAndReused(t *testing.T) {
	s := NewSeries()
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	total := 3 * maxPointsPerMetric
	var capsSeen = map[int]bool{}
	for i := 0; i < total; i++ {
		s.Add([]collector.Sample{{MetricID: "cpu.user", TS: t0.Add(time.Duration(i) * 10 * time.Second), Value: float64(i)}})
		if i > maxPointsPerMetric+rawSlack {
			capsSeen[cap(s.data["cpu.user"])] = true
		}
	}
	if c := cap(s.data["cpu.user"]); c > maxPointsPerMetric+rawSlack {
		t.Errorf("容量 %d 超过上限 %d", c, maxPointsPerMetric+rawSlack)
	}
	if len(capsSeen) != 1 {
		t.Errorf("到顶之后容量还在变（%v）：说明还在重新分配", capsSeen)
	}
	pts := s.RawRange("cpu.user", time.Time{}, farFuture)
	if len(pts) != maxPointsPerMetric {
		t.Fatalf("对外可见 %d 个点，应为 %d", len(pts), maxPointsPerMetric)
	}
	if pts[0].V != float64(total-maxPointsPerMetric) || pts[len(pts)-1].V != float64(total-1) {
		t.Errorf("可见的不是最新那一段：%v … %v", pts[0].V, pts[len(pts)-1].V)
	}
	for i := 1; i < len(pts); i++ {
		if pts[i].V != pts[i-1].V+1 {
			t.Fatalf("第 %d 个点断档：%v → %v", i, pts[i-1].V, pts[i].V)
		}
	}
}

// 一个 40 秒的打满落在两次长期层抽样之间：抽样点看不到它，峰值要记住它。
func TestPeakCapturesShortSpike(t *testing.T) {
	s := NewSeries()
	t0 := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
	for i := 0; i < 60; i++ { // 10 分钟，10 秒一点
		v := 10.0
		if i >= 12 && i < 16 { // 第 2~2:40 分钟，40 秒打满
			v = 100
		}
		s.Add([]collector.Sample{{MetricID: "cpu.core_top1", TS: t0.Add(time.Duration(i) * 10 * time.Second), Value: v}})
	}
	// 第一个桶 [03:00, 03:05) 已结束，第二个还开着
	pk := s.TakePeaks()
	if len(pk) != 1 || pk[0].P != 100 || pk[0].T != t0.Unix() {
		t.Fatalf("应记下第一个桶的峰值 100：%+v", pk)
	}
	if again := s.TakePeaks(); len(again) != 0 {
		t.Errorf("取走之后不该再返回：%+v", again)
	}
}

// 稀疏：安静的桶一个峰值都不记。
func TestPeakSparseWhenQuiet(t *testing.T) {
	s := NewSeries()
	t0 := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
	for i := 0; i < 6*30*2; i++ { // 2 小时，小幅抖动
		v := 10 + math.Sin(float64(i))*1.5 // 最大偏离 1.5 个百分点，低于门槛 3
		s.Add([]collector.Sample{{MetricID: "cpu.user", TS: t0.Add(time.Duration(i) * 10 * time.Second), Value: v}})
	}
	if pk := s.TakePeaks(); len(pk) != 0 {
		t.Errorf("安静的机器不该记峰值，记了 %d 个", len(pk))
	}
}

// 降低才是坏事的指标（可用内存）：峰值取最小值。
func TestPeakDirectionFollowsRegistry(t *testing.T) {
	s := NewSeries()
	t0 := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
	for i := 0; i < 36; i++ {
		v := 8e9
		if i == 10 {
			v = 1e9 // 某一刻可用内存掉到 1GB
		}
		s.Add([]collector.Sample{{MetricID: "mem.available", TS: t0.Add(time.Duration(i) * 10 * time.Second), Value: v}})
	}
	pk := s.TakePeaks()
	if len(pk) != 1 || pk[0].P != 1e9 {
		t.Fatalf("可用内存的峰值应是最低点 1e9：%+v", pk)
	}
}

// Worst：原始层覆盖的部分看每个原始点；更早的部分看长期层和峰值。
func TestWorstUsesPeaksBeyondRawTier(t *testing.T) {
	s := NewSeries()
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	// 3 天前的长期层：抽样点平稳，但 02:10 那个桶里有过一个峰值
	for ts := day; ts.Before(day.Add(24 * time.Hour)); ts = ts.Add(coarseStep) {
		s.AddCoarse([]collector.Sample{{MetricID: "disk.await_w", TS: ts, Value: 3}})
	}
	s.AddPeak("disk.await_w", day.Add(2*time.Hour+10*time.Minute).Unix(), 42)
	// 最近的原始层
	now := day.Add(72 * time.Hour)
	for ts := now.Add(-time.Hour); !ts.After(now); ts = ts.Add(10 * time.Second) {
		s.Add([]collector.Sample{{MetricID: "disk.await_w", TS: ts, Value: 3}})
	}
	if w, ok := s.Worst("disk.await_w", day, day.Add(6*time.Hour)); !ok || w != 42 {
		t.Errorf("3 天前那段的最坏值应是峰值 42，实得 %v/%v", w, ok)
	}
	if w, _ := s.Worst("disk.await_w", day.Add(6*time.Hour), day.Add(12*time.Hour)); w != 3 {
		t.Errorf("没有峰值的时段最坏值就是抽样点 3，实得 %v", w)
	}
}

// 整层查询用 time.Time{} 和 farFuture 作边界：UnixNano 对它们是未定义的（实测溢出），
// 夹到边界之后必须返回整层。
func TestRawRangeWholeTierWithZeroTime(t *testing.T) {
	s := NewSeries()
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 100; i++ {
		s.Add([]collector.Sample{{MetricID: "net.tx", TS: t0.Add(time.Duration(i) * 10 * time.Second), Value: 1}})
	}
	if n := len(s.RawRange("net.tx", time.Time{}, farFuture)); n != 100 {
		t.Errorf("整层应返回 100 个点，实得 %d", n)
	}
	if _, ok := s.Lookup("net.tx", time.Time{}, time.Hour); ok {
		t.Error("离得十万八千里的目标不该配上")
	}
}
