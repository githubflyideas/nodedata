package diagnosis

import (
	"math"
	"strings"
	"testing"
)

func nan14() []float64 {
	z := make([]float64, 14)
	for i := range z {
		z[i] = math.NaN()
	}
	return z
}

// 空输入必须产出空诊断链——证明结论不是硬编码的。
func TestEmptyInputYieldsNoFindings(t *testing.T) {
	c := Diagnose(nil, nil, Options{})
	if c.Count != 0 || len(c.Items) != 0 {
		t.Fatalf("expected 0 items for empty input, got %d: %+v", c.Count, c.Items)
	}
	if c.Priority != Info {
		t.Fatalf("expected priority Info, got %v", c.Priority)
	}
}

// L0 全部通过时不应产出诊断。
func TestL0AllPassYieldsNothing(t *testing.T) {
	cats := []L0Category{{Name: "CPU", Checks: []L0Check{
		{ID: "cpu.1", Name: "load sane", Level: 0},
		{ID: "cpu.2", Name: "no steal", Level: 0},
	}}}
	if c := Diagnose(cats, nil, Options{}); c.Count != 0 {
		t.Fatalf("expected 0 items, got %d", c.Count)
	}
}

// L0 失败项必须逐条转成诊断，且级别映射正确。
func TestL0FailuresBecomeItems(t *testing.T) {
	cats := []L0Category{{Name: "Time/Sync", Checks: []L0Check{
		{ID: "t.1", Name: "ntp synced", Level: 2, Message: "offset 150ms"},
		{ID: "t.2", Name: "rtc ok", Level: 1, Message: "drift"},
		{ID: "t.3", Name: "tz set", Level: 0},
	}}}
	c := Diagnose(cats, nil, Options{})
	if c.Count != 2 {
		t.Fatalf("expected 2 items, got %d", c.Count)
	}
	if c.Priority != Critical {
		t.Fatalf("expected Critical priority, got %v", c.Priority)
	}
	// critical 必须排在 warn 之前
	if c.Items[0].Level != Critical {
		t.Fatalf("expected critical first, got %v", c.Items[0].Level)
	}
	// 描述必须来自入参 message，而不是写死的文案
	if !strings.Contains(c.Items[0].Description, "offset 150ms") {
		t.Fatalf("description not derived from input: %q", c.Items[0].Description)
	}
}

// 未就绪（全 NaN）的偏离度不得产出诊断，只应记入 notes。
func TestNotReadyDeviationSkipped(t *testing.T) {
	devs := []Deviation{{MetricID: "cpu.user", Domain: "cpu", Z: nan14()}}
	c := Diagnose(nil, devs, Options{})
	if c.Count != 0 {
		t.Fatalf("expected 0 items for NaN z, got %d", c.Count)
	}
	joined := strings.Join(c.Notes, " ")
	if !strings.Contains(joined, "1 metrics skipped") {
		t.Fatalf("expected skip note, got %v", c.Notes)
	}
}

// 低于阈值不报，高于阈值才报；数值必须出现在证据里。
func TestZThresholdBoundary(t *testing.T) {
	mk := func(z float64) []Deviation {
		zs := nan14()
		zs[3] = z
		return []Deviation{{MetricID: "disk.util", Domain: "disk", Unit: "percent", Value: 91.5, Z: zs, Breadth: 1}}
	}
	if c := Diagnose(nil, mk(2.9), Options{ZThreshold: 3.0}); c.Count != 0 {
		t.Fatalf("z=2.9 should not fire, got %d", c.Count)
	}
	c := Diagnose(nil, mk(4.25), Options{ZThreshold: 3.0})
	if c.Count != 1 {
		t.Fatalf("z=4.25 should fire once, got %d", c.Count)
	}
	ev := strings.Join(c.Items[0].Evidence, " | ")
	if !strings.Contains(ev, "+4.25") || !strings.Contains(ev, "L4") {
		t.Fatalf("evidence must carry the actual z and lag, got %q", ev)
	}
	if !strings.Contains(c.Items[0].Description, "91.500") {
		t.Fatalf("description must carry actual value, got %q", c.Items[0].Description)
	}
}

// breadth 达标时升级为 critical。
func TestBreadthEscalatesToCritical(t *testing.T) {
	zs := nan14()
	zs[0] = -5.0
	devs := []Deviation{{MetricID: "mem.available", Domain: "mem", Z: zs, Breadth: 6}}
	c := Diagnose(nil, devs, Options{ZThreshold: 3.0, BreadthCritical: 5})
	if c.Count != 1 || c.Items[0].Level != Critical {
		t.Fatalf("expected 1 critical, got %d / %v", c.Count, c.Items[0].Level)
	}
	// 负向偏离必须描述为下降
	if !strings.Contains(c.Items[0].Title, "下降") {
		t.Fatalf("expected 下降 for negative z, got %q", c.Items[0].Title)
	}
}
