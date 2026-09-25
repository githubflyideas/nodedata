package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
)

// 服务消失时，父进程是最有用的一条"遗照"：父进程是 bash/sshd 的，多半是 SSH 会话一断
// 就跟着没了（SIGHUP），不是被 OOM 或谁 kill 的。这条必须穿过事件流、重启后还在。
func TestVanishedServiceKeepsParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "services.jsonl")
	day0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	l := NewServiceLog(path, 14*24*time.Hour)
	l.Load(day0)
	app := svc("Redis", 4021, day0.Add(-48*time.Hour).Unix(), 6379)
	app.PPID, app.Parent = 3310, "bash"
	for i := 0; i < minSeenRounds; i++ {
		l.Update([]collector.Service{app}, day0.Add(time.Duration(i)*time.Minute))
	}
	for i := 0; i < vanishConfirm; i++ {
		l.Update(nil, day0.Add(time.Hour))
	}

	// 重新打开：父进程要从磁盘上的事件流里读回来，不能只活在内存里
	l2 := NewServiceLog(path, 14*24*time.Hour)
	if _, err := l2.Load(day0.Add(2 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	var got *svcRow
	for _, r := range servicesJSON(l2, day0.Add(2*time.Hour)).Rows {
		if r.Name == "Redis" {
			r := r
			got = &r
		}
	}
	if got == nil || got.Alive {
		t.Fatalf("Redis 应作为已消失的服务留在表里：%+v", got)
	}
	if got.Parent != "bash" || got.PPID != 3310 {
		t.Errorf("消失的服务要带着最后一眼的父进程，实得 %q/%d", got.Parent, got.PPID)
	}
}

// 活着的服务：父进程原样透传到表里。
func TestLiveServiceShowsParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "services.jsonl")
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	l := NewServiceLog(path, 14*24*time.Hour)
	l.Load(now)
	ng := svc("Nginx", 890, now.Add(-time.Hour).Unix(), 80)
	ng.PPID, ng.Parent = 1, "systemd"
	for i := 0; i < minSeenRounds; i++ {
		l.Update([]collector.Service{ng}, now.Add(time.Duration(i)*time.Minute))
	}
	for _, r := range servicesJSON(l, now.Add(5*time.Minute)).Rows {
		if r.Name == "Nginx" {
			if r.Parent != "systemd" || r.PPID != 1 {
				t.Errorf("父进程没透传：%q/%d", r.Parent, r.PPID)
			}
			return
		}
	}
	t.Fatal("没有 Nginx 行")
}
