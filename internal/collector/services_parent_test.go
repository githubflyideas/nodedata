package collector

import (
	"testing"
	"time"
)

// 父进程名从同一轮扫描的进程表里查，不额外读文件。
// 找不到（容器里 ppid=0、父进程刚好退出）就留空，不猜。
func TestServiceParentResolved(t *testing.T) {
	c := &Collector{}
	procs := []svcProc{
		{pid: 1, comm: "systemd", exe: "systemd"},
		{pid: 3310, ppid: 2200, comm: "bash", exe: "bash"},
		{pid: 890, ppid: 1, comm: "nginx", exe: "nginx", inodes: []uint64{11}},
		{pid: 4021, ppid: 3310, comm: "redis-server", exe: "redis-server", inodes: []uint64{12}},
		{pid: 5000, ppid: 0, comm: "mysqld", exe: "mysqld", inodes: []uint64{13}},
	}
	listen := map[uint64]int{11: 80, 12: 6379, 13: 3306}
	got := map[string]Service{}
	for _, s := range c.groupServices(procs, listen, time.Now()) {
		got[s.Name] = s
	}
	cases := []struct {
		name, parent string
		ppid         int
	}{
		{"Nginx", "systemd", 1}, // 正常托管
		{"Redis", "bash", 3310}, // 有人手工起的
		{"MySQL", "", 0},        // 查不到就留空
	}
	for _, c := range cases {
		s, ok := got[c.name]
		if !ok {
			t.Fatalf("没识别出 %s：%v", c.name, got)
		}
		if s.Parent != c.parent || s.PPID != c.ppid {
			t.Errorf("%s 父进程 = %q/%d，期望 %q/%d", c.name, s.Parent, s.PPID, c.parent, c.ppid)
		}
	}
}
