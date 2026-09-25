package main

import (
	"strings"
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/cmd/nodedata/check"
	"github.com/githubflyideas/nodedata/internal/collector"
)

// busyFixture 造一台"根分区快满 + 盘在挨打"的机器，用来看报告长什么样。
func busyFixture(t *testing.T) (*Diagnoser, time.Time) {
	t.Helper()
	l0 := check.FullResult{Categories: []check.Category{
		{Name: "Disk/IO", Level: 2, Checks: []check.Check{
			{ID: "D01", Name: "根分区空间", Level: 2, Message: "已用 96.2%"},
			{ID: "D07", Name: "平均等待", Level: 1, Message: "写等待 41ms"},
		}},
		{Name: "CPU", Level: 0, Checks: []check.Check{
			{ID: "C02", Name: "运行队列压力", Level: 0, Message: "正常"},
		}},
	}}
	d, s := newUseFixture(t, l0)
	now := time.Now()
	feed(s, now, "cpu.user", 12)
	feed(s, now, "mem.used_pct", 41)
	feed(s, now, "net.rx", 2.2e6)
	feed(s, now, "procs_running", 3)
	// 盘：基线安静，最近 10 分钟被打爆
	for ts := now.Add(-3 * time.Hour); !ts.After(now); ts = ts.Add(5 * time.Minute) {
		util, await, io := 8.0, 2.0, 1024.0
		if ts.After(now.Add(-10 * time.Minute)) {
			util, await, io = 94, 41, 620<<20
		}
		sm := []collector.Sample{
			{MetricID: "disk.util", TS: ts, Value: util},
			{MetricID: "disk.await_w", TS: ts, Value: await},
			{MetricID: "proc.io.rsync", TS: ts, Value: io},
		}
		s.Add(sm)
		s.AddCoarse(sm)
	}
	return d, now
}

// 把报告打出来看一眼。设计这份东西的全部意义在于"读起来像不像话"，
// 那件事没有断言能替代。
func TestReportLooksLikeSomethingYouCanRead(t *testing.T) {
	d, now := busyFixture(t)
	t.Log("\n" + textReport("jp02-dns-01", d.Use(now), d.Run(3.0), now))
}

// 最容易被省掉、后果最严重的一段：没有它，"网络 正常"会被模型
// 读成"网络查过了没问题"。这条测试是那一段的看门人。
func TestReportAlwaysDeclaresBlindSpots(t *testing.T) {
	d, now := busyFixture(t)
	out := textReport("h", d.Use(now), nil, now)

	if !strings.Contains(out, "看不到") {
		t.Fatal("报告必须有『看不到』一段")
	}
	if !strings.Contains(out, "不要据此下结论") {
		t.Error("『看不到』要明说不能据此下结论，否则模型还是会当成已查过")
	}
	if !strings.Contains(out, "每进程网络计数") {
		t.Error("网络归因缺口没写进去")
	}
}

// 严重的排前面：上下文被截断时，先丢掉的应该是"正常"那几行。
func TestReportPutsTroubleFirst(t *testing.T) {
	d, now := busyFixture(t)
	out := textReport("h", d.Use(now), nil, now)

	iDisk := strings.Index(out, "磁盘 异常")
	if iDisk < 0 {
		t.Fatalf("磁盘该是异常：\n%s", out)
	}
	for _, ok := range []string{"CPU 正常", "内存 正常"} {
		if i := strings.Index(out, ok); i >= 0 && i < iDisk {
			t.Errorf("%q 排在了异常行前面", ok)
		}
	}
}

// 数值要带基线：▲550% 离开基数没有意义，"94%（平时 8%）"才有。
func TestReportCarriesBaselines(t *testing.T) {
	d, now := busyFixture(t)
	out := textReport("h", d.Use(now), nil, now)
	if !strings.Contains(out, "平时") {
		t.Fatalf("观测必须带基线：\n%s", out)
	}
	if strings.Contains(out, "%%") || strings.Contains(out, "▲") {
		t.Errorf("不该出现裸百分比变化：\n%s", out)
	}
}

// 头一行要能让一个没有上下文的模型知道这是哪台机器、数据多新。
func TestReportHeaderIdentifiesTheMachine(t *testing.T) {
	d, now := busyFixture(t)
	first := strings.SplitN(textReport("jp02-dns-01", d.Use(now), nil, now), "\n", 2)[0]
	for _, want := range []string{"NODEDATA", "host=jp02-dns-01", "at=", "state="} {
		if !strings.Contains(first, want) {
			t.Errorf("头一行缺 %q：%q", want, first)
		}
	}
}

// 主机名缺失时不能留一个空 host=，那会让下游以为字段丢了。
func TestReportHandlesMissingHostname(t *testing.T) {
	d, now := busyFixture(t)
	if !strings.Contains(textReport("", d.Use(now), nil, now), "host=unknown") {
		t.Error("主机名为空时该写 unknown")
	}
}

// token 预算。超了说明"只报异常"那条没做到——这是唯一会随时间悄悄
// 退化的指标，所以钉一根桩在这里。
func TestReportStaysWithinTokenBudget(t *testing.T) {
	d, now := busyFixture(t)
	out := textReport("jp02-dns-01", d.Use(now), d.Run(3.0), now)
	// 中文大致 1 字 1 token，取 rune 数当上界估计
	n := len([]rune(out))
	t.Logf("报告 %d 字符 / %d 行", n, strings.Count(out, "\n"))
	if n > 1500 {
		t.Errorf("报告 %d 字符，超出 1500 预算——检查是不是把正常指标也报了", n)
	}
}

// 线索是"测试中"的推测，必须单独成段并且标注清楚。
// 混进资源行里，模型会把它当成已经成立的判定。
func TestReportFencesOffSpeculation(t *testing.T) {
	d, now := busyFixture(t)
	out := textReport("h", d.Use(now), d.Run(3.0), now)
	if i := strings.Index(out, "线索"); i >= 0 {
		seg := out[i:]
		if !strings.Contains(strings.SplitN(seg, "\n", 2)[0], "测试中") {
			t.Error("线索段必须标注测试中")
		}
	}
}

// swap.in/out 是速率不是存量。落到"swap. 开头 = 字节"那条规则上，
// 每秒换出 50MB 会显示成 "50MB"，看不出这台机器正在挨打。
func TestSwapRateIsThroughputNotSize(t *testing.T) {
	for _, id := range []string{"swap.in", "swap.out"} {
		if got := unitOf(id); got != "bytes/s" {
			t.Errorf("unitOf(%q) = %q，期望 bytes/s", id, got)
		}
	}
	if got := fmtUnit(52428800, "bytes/s"); got != "50MB/s" {
		t.Errorf("50MB/s 格式化成了 %q", got)
	}
}

func TestFmtUnit(t *testing.T) {
	cases := []struct {
		v          float64
		unit, want string
	}{
		{94, "percent", "94%"},
		{0.52, "percent", "0.5%"},
		{41, "ms", "41ms"},
		{650117120, "bytes/s", "620MB/s"},
		{13 << 30, "bytes", "13GB"},
		{1.23, "load", "1.23"},
		{12000, "/s", "12k/s"},
		{5, "/s", "5/s"},
		{-0.04, "percent", "0%"},
	}
	for _, c := range cases {
		if got := fmtUnit(c.v, c.unit); got != c.want {
			t.Errorf("fmtUnit(%v, %q) = %q，期望 %q", c.v, c.unit, got, c.want)
		}
	}
}
