package diagnosis

import (
	"strings"
	"testing"
	"time"
)

func zs(v float64) []float64 {
	a := make([]float64, 10)
	for i := range a {
		a[i] = v
	}
	return a
}

// "可用内存上升 6.0σ"被报成 Critical——内存变多了反而告警，是实测截图里最刺眼的一条。
func TestGoodDirectionNotReported(t *testing.T) {
	devs := []Deviation{
		{MetricID: "mem.available", Value: 15e9, Unit: "bytes", Z: zs(6), Breadth: 6},
		{MetricID: "mem.used_pct", Value: 12, Unit: "percent", Z: zs(-6), Breadth: 6},
	}
	c := Diagnose(nil, devs, Options{ZThreshold: 3, BreadthCritical: 5, Now: time.Now()})
	for _, it := range c.Items {
		if strings.Contains(it.Title, "mem.available") {
			t.Errorf("可用内存变多不该报：%q", it.Title)
		}
	}
}

// 变了不等于坏了：还剩八成内存时，可用内存掉 6σ 也只是缓存在动。
func TestAbsoluteGate(t *testing.T) {
	mk := func(usedPct float64) *Chain {
		devs := []Deviation{
			{MetricID: "mem.available", Value: 15e9, Unit: "bytes", Z: zs(-6), Breadth: 6},
			{MetricID: "mem.used_pct", Value: usedPct, Unit: "percent", Z: zs(1), Breadth: 1},
		}
		return Diagnose(nil, devs, Options{ZThreshold: 3, BreadthCritical: 5, Now: time.Now()})
	}
	if c := mk(20); len(c.Items) != 0 {
		t.Errorf("内存才用两成，不该出结论：%q", c.Items[0].Title)
	}
	if c := mk(93); len(c.Items) == 0 {
		t.Error("内存已用 93% 时必须报")
	}

	// 盘不忙时，延迟的相对变化没有意义
	idle := []Deviation{
		{MetricID: "disk.await_w", Value: 9, Unit: "ms", Z: zs(6), Breadth: 6},
		{MetricID: "disk.util", Value: 3, Unit: "percent", Z: zs(1), Breadth: 1},
	}
	if c := Diagnose(nil, idle, Options{ZThreshold: 3, BreadthCritical: 5, Now: time.Now()}); len(c.Items) != 0 {
		t.Errorf("盘 util 才 3%%，不该出结论：%q", c.Items[0].Title)
	}
	busy := []Deviation{
		{MetricID: "disk.await_w", Value: 90, Unit: "ms", Z: zs(6), Breadth: 6},
		{MetricID: "disk.util", Value: 97, Unit: "percent", Z: zs(5), Breadth: 5},
	}
	if c := Diagnose(nil, busy, Options{ZThreshold: 3, BreadthCritical: 5, Now: time.Now()}); len(c.Items) == 0 {
		t.Error("盘 util 97% + 写延迟飙升，必须报")
	}
}
