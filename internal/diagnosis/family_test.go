package diagnosis

import (
	"strings"
	"testing"
	"time"
)

// 实机注入内存故障时，领头指标在 mem.available → mem.free → mem.used_pct 之间轮换，
// 看起来像三件事，其实是一件。同族要固定一个代表领头，标题才稳定。
func TestMetricFamilyDedup(t *testing.T) {
	z := func(v float64) []float64 {
		a := make([]float64, 10)
		for i := range a {
			a[i] = v
		}
		return a
	}
	// 三次采样，谁的 |z| 最大轮换：不加族去重时领头会跟着换
	rounds := [][3]float64{{-6, -4, 3}, {-4, -6, 3}, {-3, -4, 6}}
	titles := map[string]bool{}
	for _, r := range rounds {
		devs := []Deviation{
			{MetricID: "mem.available", Value: 1e9, Unit: "bytes", Z: z(r[0]), Breadth: 6},
			{MetricID: "mem.free", Value: 5e8, Unit: "bytes", Z: z(r[1]), Breadth: 6},
			{MetricID: "mem.used_pct", Value: 92, Unit: "percent", Z: z(r[2]), Breadth: 6},
		}
		c := Diagnose(nil, devs, Options{ZThreshold: 3, Now: time.Now()})
		if len(c.Items) == 0 {
			t.Fatal("应有结论")
		}
		it := c.Items[0]
		head := strings.SplitN(strings.TrimPrefix(it.Title, "内存 劣化："), " ", 2)[0]
		titles[head] = true
		// 同族其余成员不再罗列
		if strings.Count(it.Description, "mem.") > 2 {
			t.Errorf("同族成员不该被逐条罗列：%q", it.Description)
		}
	}
	if len(titles) != 1 {
		t.Errorf("领头指标在同族之间轮换了 %v —— 同一件事被说成好几件", titles)
	}
	if !titles["mem.available"] {
		t.Errorf("内存余量族的代表应是 mem.available（已扣掉可回收页缓存），实际 %v", titles)
	}
}

// 页缓存被回收是内核正常干活，不该领头下结论——否则会得出
// "firefox 导致 mem.cached 下降"这种现象为真、因果为假的结论。
func TestEvidenceOnlyMetricsDoNotLead(t *testing.T) {
	z := func(v float64) []float64 {
		a := make([]float64, 10)
		for i := range a {
			a[i] = v
		}
		return a
	}
	// 只有 mem.cached 偏离：整类都不该出结论
	only := []Deviation{{MetricID: "mem.cached", Value: 1e9, Unit: "bytes", Z: z(-6), Breadth: 6}}
	if c := Diagnose(nil, only, Options{ZThreshold: 3, Now: time.Now()}); len(c.Items) != 0 {
		t.Errorf("只有页缓存变化时不该出结论：%q", c.Items[0].Title)
	}
	// 真故障 + 页缓存同时偏离：领头必须是真故障那个
	mixed := append(only, Deviation{MetricID: "mem.available", Value: 1e8, Unit: "bytes", Z: z(-5), Breadth: 6})
	c := Diagnose(nil, mixed, Options{ZThreshold: 3, Now: time.Now()})
	if len(c.Items) == 0 || !strings.Contains(c.Items[0].Title, "mem.available") {
		t.Fatalf("领头应是 mem.available：%+v", c.Items)
	}
}
