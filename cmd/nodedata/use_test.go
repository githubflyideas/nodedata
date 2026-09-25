package main

import (
	"strings"
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/cmd/nodedata/check"
	"github.com/githubflyideas/nodedata/internal/collector"
)

// newUseFixture 造一个只依赖内存的 Diagnoser：series + builder，L0 由调用方注入。
func newUseFixture(t *testing.T, l0 check.FullResult) (*Diagnoser, *Series) {
	t.Helper()
	s := NewSeries()
	b := NewHeatmapBuilder(s)
	d := NewDiagnoser(t.TempDir(), s, b)
	d.l0Fn = func() check.FullResult { return l0 }
	return d, s
}

// feed 往两层都塞点：Last 走原始层，baselineOf 走长期层。
func feed(s *Series, now time.Time, id string, val float64) {
	for ts := now.Add(-3 * time.Hour); !ts.After(now); ts = ts.Add(5 * time.Minute) {
		sm := []collector.Sample{{MetricID: id, TS: ts, Value: val}}
		s.Add(sm)
		s.AddCoarse(sm)
	}
}

// 最要命的失败模式：一个资源压根没被采到，却报"正常"。
// 运维看见"网络 正常"就不会再去查网络了——而真相是容器里读不到 softnet。
// 宁可说"无数据"，也不要给一个没有依据的 OK。
func TestUseNeverClaimsNormalWithoutEvidence(t *testing.T) {
	d, s := newUseFixture(t, check.FullResult{})
	now := time.Now()
	feed(s, now, "cpu.user", 10)

	rows := d.Use(now)
	byName := map[string]UseRow{}
	for _, r := range rows {
		byName[r.Resource] = r
	}

	if got := byName["CPU"].State; got != "正常" {
		t.Errorf("CPU 有数据且无告警，应为正常，实得 %q", got)
	}
	for _, name := range []string{"内存", "磁盘", "网络"} {
		r := byName[name]
		if r.State == "正常" {
			t.Errorf("%s 一个指标都没采到，不能报正常（实得 level=%d）", name, r.Level)
		}
		if r.State != "无数据" {
			t.Errorf("%s 应为无数据，实得 %q", name, r.State)
		}
	}
}

// L0 判定是 State 的唯一来源。这里确认 fail 能穿透到行上，
// 而且那条判定本身会被列出来——只说"异常"不说为什么等于没说。
func TestUseStateComesFromL0(t *testing.T) {
	l0 := check.FullResult{Categories: []check.Category{{
		Name:  "Disk/IO",
		Level: 2,
		Checks: []check.Check{
			{ID: "D01", Name: "根分区空间", Level: 2, Message: "已用 96.2%"},
			{ID: "D05", Name: "块设备", Level: 0, Message: "正常"},
		},
	}}}
	d, s := newUseFixture(t, l0)
	now := time.Now()
	feed(s, now, "disk.util", 3)

	var disk UseRow
	for _, r := range d.Use(now) {
		if r.Resource == "磁盘" {
			disk = r
		}
	}
	if disk.State != "异常" || disk.Level != 2 {
		t.Fatalf("D01 是 fail，磁盘该是异常，实得 %q/%d", disk.State, disk.Level)
	}
	if len(disk.Checks) != 1 || disk.Checks[0].ID != "D01" {
		t.Fatalf("只有越线的判定该进 Checks，实得 %+v", disk.Checks)
	}
	// D05 正常，不该占位置——37 项全列出来就又是一张网格了
	for _, c := range disk.Checks {
		if c.ID == "D05" {
			t.Error("Level=0 的判定不该出现")
		}
	}
}

// 主指标无论正不正常都要出现，并且带上基线。
// "CPU 正常" 远不如 "CPU 正常（用户态 12%，平时 12%）"：
// 后者顺带证明了采集是通的、基线在哪。
func TestUsePrimaryFactAlwaysShownWithBaseline(t *testing.T) {
	d, s := newUseFixture(t, check.FullResult{})
	now := time.Now()
	feed(s, now, "cpu.busy_pct", 12)

	for _, r := range d.Use(now) {
		if r.Resource != "CPU" {
			continue
		}
		if len(r.Facts) == 0 {
			t.Fatal("主指标必须出现，哪怕一切正常")
		}
		f := r.Facts[0]
		if f.ID != "cpu.busy_pct" || f.Value != 12 {
			t.Fatalf("主指标应是 cpu.busy_pct=12，实得 %+v", f)
		}
		if f.Base == nil {
			t.Fatal("喂了 3 小时长期层，基线不该是 nil")
		}
		if *f.Base != 12 {
			t.Errorf("基线应为中位数 12，实得 %v", *f.Base)
		}
		return
	}
	t.Fatal("没有 CPU 行")
}

// 历史不够时宁可不给基线，也不要拿 3 个点编一个出来。
func TestUseBaselineNilWhenHistoryThin(t *testing.T) {
	d, s := newUseFixture(t, check.FullResult{})
	now := time.Now()
	// 只喂 3 个长期层点，低于 12 点门槛
	for i := 0; i < 3; i++ {
		sm := []collector.Sample{{MetricID: "cpu.user", TS: now.Add(-time.Duration(i+2) * time.Hour), Value: 5}}
		s.Add(sm)
		s.AddCoarse(sm)
	}
	s.Add([]collector.Sample{{MetricID: "cpu.user", TS: now, Value: 5}})

	if b := d.baselineOf("cpu.user", now); b != nil {
		t.Errorf("只有 3 个点，不该给基线，实得 %v", *b)
	}
}

// 基线要掐掉最近 10 分钟。否则一个持续 20 分钟的故障会把自己的基线抬上去，
// 页面上显示"41ms（平时 40ms）"，于是没人觉得不对。
func TestUseBaselineExcludesTheOngoingSpike(t *testing.T) {
	d, s := newUseFixture(t, check.FullResult{})
	now := time.Now()
	for ts := now.Add(-3 * time.Hour); !ts.After(now); ts = ts.Add(5 * time.Minute) {
		v := 2.0
		if ts.After(now.Add(-10 * time.Minute)) {
			v = 400 // 正在发生的尖峰
		}
		s.AddCoarse([]collector.Sample{{MetricID: "disk.await_w", TS: ts, Value: v}})
	}
	b := d.baselineOf("disk.await_w", now)
	if b == nil {
		t.Fatal("点够了，应该有基线")
	}
	if *b != 2 {
		t.Errorf("基线该是尖峰之前的 2，实得 %v —— 尖峰把自己的基线抬上去了", *b)
	}
}

// F01/F02 是盘上的容量，F03 是文件描述符。按首字母一刀切会把 fd 算成磁盘问题。
func TestL0ResourceMapping(t *testing.T) {
	want := map[string]string{
		"C02": "CPU", "M05": "内存", "D07": "磁盘",
		"F01": "磁盘", "F02": "磁盘", "F03": "系统",
		"N04": "网络", "S01": "网络",
		"E02": "系统", "T03": "系统",
		"": "", "X99": "",
	}
	for id, w := range want {
		if got := l0Resource(id); got != w {
			t.Errorf("l0Resource(%q) = %q，期望 %q", id, got, w)
		}
	}
}

// 安静的机器不该列一串进程名——那是噪音，不是归因。
func TestUseMoversOnlyWhenSomethingIsWrong(t *testing.T) {
	d, s := newUseFixture(t, check.FullResult{})
	now := time.Now()
	feed(s, now, "cpu.user", 10)
	feed(s, now, "proc.cpu.mysqld", 40)

	for _, r := range d.Use(now) {
		if r.Resource == "CPU" && r.Level == 0 && len(r.Movers) > 0 {
			t.Errorf("CPU 正常却给了归因：%+v", r.Movers)
		}
	}
}

// 网络归因这个缺口必须写在脸上，否则用户以为已经查过了。
// （逐核在 v5.17.0 补上了，那条盲区随之删掉——还留着就是在说假话。）
func TestUseDeclaresItsBlindSpots(t *testing.T) {
	d, s := newUseFixture(t, check.FullResult{})
	now := time.Now()
	feed(s, now, "cpu.user", 10)
	feed(s, now, "net.rx", 1000)

	blind := map[string]string{}
	for _, r := range d.Use(now) {
		blind[r.Resource] = strings.Join(r.Blind, " | ")
	}
	if strings.Contains(blind["CPU"], "逐核") {
		t.Errorf("逐核已经有了，CPU 行不该再声明没有：%q", blind["CPU"])
	}
	if !strings.Contains(blind["网络"], "归因") {
		t.Errorf("网络必须声明归因不到进程，实得 %q", blind["网络"])
	}
}

// 实测发现的内核记账口径：子进程被 wait() 回收时，它的 write_bytes 被累加进
// 父进程的 signal->ioac，而 /proc/PID/io 读的就是这个。实验里
// `bash -c 'dd ... bs=1M count=3000'` 结束后，父 bash 的 write_bytes
// 精确多了 3145805824 字节。归因如实转述了内核的说法，但看的人会以为
// bash 在写盘——所以这一条必须写在脸上。
func TestUseDeclaresParentIOAccounting(t *testing.T) {
	d, s := newUseFixture(t, check.FullResult{})
	now := time.Now()
	feed(s, now, "disk.util", 5)

	for _, r := range d.Use(now) {
		if r.Resource != "磁盘" {
			continue
		}
		if !strings.Contains(strings.Join(r.Blind, " "), "父进程") {
			t.Fatalf("磁盘行必须声明父进程记账口径，实得 %v", r.Blind)
		}
		return
	}
	t.Fatal("没有磁盘行")
}

// 一个被单线程钉死在 100% 的核，天天如此，z 永远是 0。
// 如果"最忙单核"只在偏离时显示，最典型的单核瓶颈永远不会出现在页面上。
func TestUseBusiestCoreAlwaysShown(t *testing.T) {
	d, s := newUseFixture(t, check.FullResult{})
	now := time.Now()
	feed(s, now, "cpu.busy_pct", 6.5)  // 16 核整机：看上去很闲
	feed(s, now, "cpu.core_top1", 100) // 其实有个核一直满着
	for _, r := range d.Use(now) {
		if r.Resource != "CPU" {
			continue
		}
		for _, f := range r.Facts {
			if f.ID == "cpu.core_top1" {
				if f.Value != 100 {
					t.Errorf("最忙单核应为 100，实得 %v", f.Value)
				}
				return
			}
		}
		t.Fatalf("稳态打满的核没有显示出来：%+v", r.Facts)
	}
	t.Fatal("没有 CPU 行")
}
