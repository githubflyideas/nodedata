package diagnosis

import (
	"strings"
	"testing"
	"time"
)

// 被 cgroup 配额限流的进程是**受害者**，不是元凶。
// 实机验证过：把进程关进 0.2 核的 cgroup，旧逻辑报"责任方 该进程"，
// 照这条去 kill 进程方向完全反了——真正的原因是配额。
func TestThrottledProcessIsVictim(t *testing.T) {
	z := func(v float64) []float64 {
		a := make([]float64, 10)
		for i := range a {
			a[i] = v
		}
		return a
	}
	// 被限流时整机 CPU 天然上不去（配额就那么点），绝对值闸门必须为限流放行
	devs := []Deviation{{MetricID: "cpu.user", Value: 20, Unit: "percent", Z: z(6), Breadth: 6, OnsetLag: "L1"}}
	throttled := Proc{PID: 300, Comm: "worker", Key: "worker", CPU: 19,
		ThrottledFrac: 1.0, ThrottledPerS: 10, ThrottledRatio: 0.8, CGroup: "/app.slice"}
	c := Diagnose(nil, devs, Options{ZThreshold: 3, Procs: []Proc{throttled}, Now: time.Now()})
	if len(c.Items) == 0 {
		t.Fatal("应有结论")
	}
	it := c.Items[0]
	if !strings.Contains(it.Title, "受害者") {
		t.Errorf("标题应说明它是受害者而不是责任方：%q", it.Title)
	}
	if len(it.Culprits) == 0 || it.Culprits[0].Kind != "cgroup" {
		t.Fatalf("责任方类型应为 cgroup：%+v", it.Culprits)
	}
	if !strings.Contains(it.Culprits[0].Detail, "配额") {
		t.Errorf("详情应指向配额：%q", it.Culprits[0].Detail)
	}
	joined := strings.Join(it.Commands, " ")
	if !strings.Contains(joined, "cpu.stat") || !strings.Contains(joined, "quota") {
		t.Errorf("下一步该看 cpu.stat 与配额，而不是 top/kill：%v", it.Commands)
	}

	// 没被限流时维持原行为：正常指认进程。
	// 这里 cpu.user 要给真实的高值——绝对值闸门在机器不忙时本就不出结论。
	normal := throttled
	normal.ThrottledFrac, normal.ThrottledRatio, normal.ThrottledPerS = 0, 0, 0
	normal.CPU = 300
	busyDevs := []Deviation{{MetricID: "cpu.user", Value: 290, Unit: "percent", Z: z(6), Breadth: 6, OnsetLag: "L1"}}
	c2 := Diagnose(nil, busyDevs, Options{ZThreshold: 3, Procs: []Proc{normal}, Now: time.Now()})
	if len(c2.Items) == 0 {
		t.Fatal("机器确实很忙时应有结论")
	}
	if strings.Contains(c2.Items[0].Title, "受害者") {
		t.Errorf("未被限流时不该说受害者：%q", c2.Items[0].Title)
	}
}
