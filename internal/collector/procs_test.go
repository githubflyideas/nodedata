package collector

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeStat 写一个最小但字段位置正确的 /proc/PID/stat。
func writeStat(t *testing.T, root string, pid int, comm string, utime, stime, start, rssPages uint64) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 3 state .. 13 cmajflt | 14 utime 15 stime | 16..21 | 22 starttime 23 vsize 24 rss ...
	line := fmt.Sprintf("%d (%s) S 1 1 1 0 -1 4194304 0 0 0 0 %d %d 0 0 20 0 1 0 %d 1000000 %d 0 0 0\n",
		pid, comm, utime, stime, start, rssPages)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

func samplesMap(ss []Sample) map[string]float64 {
	m := map[string]float64{}
	for _, s := range ss {
		m[s.MetricID] = s.Value
	}
	return m
}

func TestCollectProcs(t *testing.T) {
	root := t.TempDir()
	c := New(Config{ProcRoot: root, Interval: 5 * time.Second})
	self := os.Getpid()
	t0 := time.Unix(1_800_000_000, 0)

	writeStat(t, root, 100, "Web Content", 1000, 0, 500, 10)     // comm 带空格
	writeStat(t, root, 101, "weird) (name", 2000, 0, 600, 10)    // comm 带括号
	writeStat(t, root, 102, "kworker/u4:2-events", 0, 0, 700, 0) // 内核线程
	writeStat(t, root, self, "nodedata", 50, 50, 800, 100)

	out, err := c.CollectProcs(root, t0)
	if err != nil {
		t.Fatal(err)
	}
	// 首轮：只记基线，绝不能把进程一辈子的 CPU 当增量报出来。
	if len(out) != 0 {
		t.Fatalf("first round must emit nothing, got %v", out)
	}

	// 5 秒后：100 用了 250 jiffies = 50% 核；101 用了 5 = 1%；自身 1 jiffy = 0.2%。
	writeStat(t, root, 100, "Web Content", 1200, 50, 500, 10)
	writeStat(t, root, 101, "weird) (name", 2005, 0, 600, 10)
	writeStat(t, root, 102, "kworker/u4:3-events_unbound", 10, 0, 700, 0) // 名字变了，key 不变
	writeStat(t, root, self, "nodedata", 51, 50, 800, 100)
	m := samplesMap(mustCollect(t, c, root, t0.Add(5*time.Second)))
	if got := m["proc.cpu.Web Content"]; got != 50 {
		t.Fatalf("Web Content = %v, want 50 (%% of a core)", got)
	}
	if got := m["proc.cpu.weird) (name"]; got != 1 {
		t.Fatalf("paren comm = %v, want 1", got)
	}
	if got := m["proc.cpu.kworker"]; got != 2 {
		t.Fatalf("kworker = %v, want 2", got)
	}

	top, _ := c.TopProcs()
	if len(top) == 0 || top[0].PID != 100 {
		t.Fatalf("top[0] should be pid 100, got %+v", top)
	}
	foundSelf := false
	for _, p := range top {
		if p.Self {
			foundSelf = true
			if p.PID != self {
				t.Fatalf("self pid mismatch")
			}
		}
	}
	if !foundSelf {
		t.Fatalf("nodedata itself must always be in the snapshot: %+v", top)
	}

	// PID 复用：101 退出，新进程拿到同一个 PID（starttime 不同）→ 不能出增量。
	writeStat(t, root, 101, "other", 999999, 0, 9999, 1)
	// 100 空闲：被跟踪的序列照样出 0，保证连续。
	m = samplesMap(mustCollect(t, c, root, t0.Add(10*time.Second)))
	if v, ok := m["proc.cpu.Web Content"]; !ok || v != 0 {
		t.Fatalf("idle tracked series must emit 0, got %v ok=%v", v, ok)
	}
	if _, ok := m["proc.cpu.other"]; ok {
		t.Fatalf("reused PID must not produce a delta")
	}

	// 进程退出：基线要回收。
	os.RemoveAll(filepath.Join(root, "102"))
	mustCollect(t, c, root, t0.Add(15*time.Second))
	if _, ok := c.procs.prev[102]; ok {
		t.Fatalf("exited pid 102 still in prev map")
	}

	// 路径审计集合必须有界：PID 段归一。
	for _, p := range c.OpenedPaths() {
		if strings.Contains(p, "/100/") {
			t.Fatalf("audit path not normalized: %s", p)
		}
	}
	if n := len(c.OpenedPaths()); n > 2 {
		t.Fatalf("audit set should collapse per-pid paths, got %d: %v", n, c.OpenedPaths())
	}
}

func mustCollect(t *testing.T, c *Collector, root string, now time.Time) []Sample {
	t.Helper()
	out, err := c.CollectProcs(root, now)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestProcKey(t *testing.T) {
	for in, want := range map[string]string{
		"kworker/u4:2-events_unbound": "kworker",
		"ksoftirqd/3":                 "ksoftirqd",
		"nodedata-linux-":             "nodedata-linux-",
		"/weird":                      "/weird",
	} {
		if got := ProcKey(in); got != want {
			t.Errorf("ProcKey(%q)=%q want %q", in, got, want)
		}
	}
}
