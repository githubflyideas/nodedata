package collector

import (
	"bytes"
	"math"
	"testing"
	"time"
)

// feedStat 把一份 /proc/stat 的 cpuN 行喂给采集器，返回本轮产出的样本。
func feedStat(c *Collector, stat string, hasPrev bool) map[string]float64 {
	var out []Sample
	for _, line := range bytes.Split([]byte(stat), []byte("\n")) {
		if len(line) > 3 && line[3] >= '0' && line[3] <= '9' {
			c.noteCoreLine(c.splitFieldsBuf(line))
		}
	}
	c.finishCores(time.Unix(0, 0), hasPrev, &out)
	m := map[string]float64{}
	for _, s := range out {
		m[s.MetricID] = s.Value
	}
	return m
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// 四核机器：cpu2 被一个单线程打满，cpu0 软中断很重（网卡中断全压在它身上），
// 其余两个核很闲。整机平均看上去只有 ~35%，单核视角才看得出问题。
func TestCoreTopThreeAndSoftirq(t *testing.T) {
	c := &Collector{}
	//            user nice sys idle iowait irq softirq steal
	round1 := "cpu0 0 0 0 0 0 0 0 0\ncpu1 0 0 0 0 0 0 0 0\ncpu2 0 0 0 0 0 0 0 0\ncpu3 0 0 0 0 0 0 0 0"
	round2 := "cpu0 10 0 10 40 0 0 40 0\n" + // 忙 60%，软中断 40%
		"cpu1 5 0 5 90 0 0 0 0\n" + // 忙 10%
		"cpu2 95 0 5 0 0 0 0 0\n" + // 忙 100%
		"cpu3 2 0 3 90 5 0 0 0" // 忙 5%（iowait 不算忙）

	if got := feedStat(c, round1, false); len(got) != 0 {
		t.Fatalf("第一轮没有基准，不该产出任何值：%v", got)
	}
	got := feedStat(c, round2, true)

	want := map[string]float64{
		"cpu.core_top1": 100, "cpu.core_top2": 60, "cpu.core_top3": 10,
		"cpu.core_softirq_max": 40,
		"cpu.busy_pct":         (60 + 10 + 100 + 5) / 4.0, // 同口径的整机平均
	}
	for k, w := range want {
		if !near(got[k], w) {
			t.Errorf("%s = %.2f，期望 %.2f", k, got[k], w)
		}
	}
	// 实时明细：4 个核都在，按核号排列
	cs := c.Cores()
	if len(cs) != 4 || cs[2].CPU != 2 || !near(cs[2].Busy, 100) {
		t.Errorf("逐核快照不对：%+v", cs)
	}
}

// 2 核机器没有第 3 名：不编一个不存在的核出来。
func TestCoreTopOnTwoCores(t *testing.T) {
	c := &Collector{}
	feedStat(c, "cpu0 0 0 0 0 0 0 0 0\ncpu1 0 0 0 0 0 0 0 0", false)
	got := feedStat(c, "cpu0 50 0 0 50 0 0 0 0\ncpu1 10 0 0 90 0 0 0 0", true)
	if _, ok := got["cpu.core_top3"]; ok {
		t.Error("2 核机器不该有 core_top3")
	}
	if !near(got["cpu.core_top1"], 50) || !near(got["cpu.core_top2"], 10) {
		t.Errorf("top1/top2 不对：%v", got)
	}
}

// 核被下线又上线（热插拔）或计数回绕：节拍倒退的核这一轮跳过，不能算出负数或天文数字。
func TestCoreCounterGoesBackwards(t *testing.T) {
	c := &Collector{}
	feedStat(c, "cpu0 100 0 100 800 0 0 0 0\ncpu1 100 0 100 800 0 0 0 0", false)
	got := feedStat(c, "cpu0 5 0 5 90 0 0 0 0\ncpu1 150 0 150 900 0 0 0 0", true)
	if !near(got["cpu.core_top1"], 50) {
		t.Errorf("cpu1 忙 50%%，cpu0 倒退应被跳过；实得 %v", got)
	}
	if _, ok := got["cpu.core_top2"]; ok {
		t.Errorf("倒退的核不该进排名：%v", got)
	}
}

// 核数变化（上线了新核）：新核这一轮没有基准，只跳过它，其余照常。
func TestCoreHotplugGrow(t *testing.T) {
	c := &Collector{}
	feedStat(c, "cpu0 0 0 0 0 0 0 0 0", false)
	got := feedStat(c, "cpu0 20 0 0 80 0 0 0 0\ncpu1 99 0 0 1 0 0 0 0", true)
	if !near(got["cpu.core_top1"], 20) {
		t.Errorf("新上线的 cpu1 没有基准不该参与；实得 %v", got)
	}
}

// 稳态下节拍缓冲不再增长：采集器每 10 秒跑一次，一直跑两周。
func TestCoreBuffersReused(t *testing.T) {
	c := &Collector{}
	stat := "cpu0 1 0 1 8 0 0 0 0\ncpu1 1 0 1 8 0 0 0 0"
	feedStat(c, stat, false)
	feedStat(c, stat, true)
	capCur, capPrev := cap(c.coreCur), cap(c.corePrev)
	for i := 0; i < 50; i++ {
		feedStat(c, stat, true)
	}
	if cap(c.coreCur) != capCur || cap(c.corePrev) != capPrev {
		t.Errorf("缓冲在涨：cur %d→%d prev %d→%d", capCur, cap(c.coreCur), capPrev, cap(c.corePrev))
	}
}

func TestCoreMetricsAreNotPercentOfAllCores(t *testing.T) {
	// 防回归：整机 cpu.* 是"核数×100"口径，逐核必须是 0–100。
	c := &Collector{}
	feedStat(c, "cpu0 0 0 0 0 0 0 0 0", false)
	got := feedStat(c, "cpu0 1000 0 0 0 0 0 0 0", true)
	if got["cpu.core_top1"] > 100 {
		t.Errorf("单核占用超过 100：%v", got["cpu.core_top1"])
	}
}
