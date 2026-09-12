package main

import (
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
)

// 接线测试：进程快照 → 服务名。L4 的责任方要说"MySQL"，不能只说"mysqld"。
func TestProcsCarryServiceName(t *testing.T) {
	l := NewServiceLog(t.TempDir()+"/s.jsonl", 14*24*time.Hour)
	now := time.Now()
	l.Load(now)
	l.Update([]collector.Service{
		{Name: "MySQL", Exe: "mysqld", PID: 1200, Ports: []int{3306}, StartTS: now.Unix() - 3600},
		{Name: "Nginx", Exe: "nginx", PID: 890, Ports: []int{80, 443}, StartTS: now.Unix() - 7200},
	}, now)

	// 按 PID 命中
	if name, ports := l.ServiceOf(1200); name != "MySQL" || len(ports) != 1 || ports[0] != 3306 {
		t.Fatalf("ServiceOf(1200) = %q %v", name, ports)
	}
	// 按进程名兜底：服务识别每分钟一次、L4 每 15 秒一次，
	// worker 的 PID 可能还没进集合，但名字能对上
	if name, _ := l.ServiceByKey("nginx"); name != "Nginx" {
		t.Fatalf("ServiceByKey(nginx) = %q", name)
	}
	// 认不出来要返回空，不能瞎猜
	if name, _ := l.ServiceOf(999999); name != "" {
		t.Fatalf("未知 PID 应返回空，got %q", name)
	}
	if name, _ := l.ServiceByKey("some-random-daemon"); name != "" {
		t.Fatalf("未知进程名应返回空，got %q", name)
	}
}
