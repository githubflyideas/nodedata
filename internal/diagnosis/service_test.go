package diagnosis

import (
	"strings"
	"testing"
	"time"
)

// L4 结论要说服务名：值班的人问的是"MySQL 是不是又卡了"，不是"pid 1200 怎么了"。
func TestCulpritCarriesServiceName(t *testing.T) {
	z := func(v float64) []float64 {
		a := make([]float64, 10)
		for i := range a {
			a[i] = v
		}
		return a
	}
	devs := []Deviation{
		{MetricID: "cpu.user", Value: 350, Unit: "percent", Z: z(6), Breadth: 6, OnsetLag: "L1"},
		{MetricID: "proc.cpu.mysqld", Value: 300, Unit: "percent", Z: z(5), Breadth: 5},
	}
	procs := []Proc{{PID: 1200, Comm: "mysqld", Key: "mysqld", CPU: 300,
		Service: "MySQL", Ports: []int{3306}}}
	c := Diagnose(nil, devs, Options{ZThreshold: 3, Procs: procs, Now: time.Now()})
	if len(c.Items) == 0 {
		t.Fatal("应有结论")
	}
	it := c.Items[0]
	if !strings.Contains(it.Title, "MySQL") || !strings.Contains(it.Title, "mysqld") || !strings.Contains(it.Title, "1200") {
		t.Fatalf("标题应同时给出服务名、进程名、PID: %q", it.Title)
	}
	var got *Culprit
	for i := range it.Culprits {
		if it.Culprits[i].PID == 1200 {
			got = &it.Culprits[i]
		}
	}
	if got == nil || got.Service != "MySQL" {
		t.Fatalf("责任方应带 Service 字段: %+v", got)
	}
	if !strings.Contains(got.Detail, "3306") {
		t.Fatalf("详情应带监听端口: %q", got.Detail)
	}

	// 没识别出服务时，退回进程名，不能出现空的"责任方 （PID…）"
	procs[0].Service, procs[0].Ports = "", nil
	c = Diagnose(nil, devs, Options{ZThreshold: 3, Procs: procs, Now: time.Now()})
	if tl := c.Items[0].Title; !strings.Contains(tl, "mysqld（PID 1200）") {
		t.Fatalf("未识别服务时应退回进程名: %q", tl)
	}
}
