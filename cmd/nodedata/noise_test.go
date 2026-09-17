package main

import (
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/diagnosis"
)

// 实机注入时四分钟留了 3 份、四个类别轮着来。同类/同责任方的限流挡不住"轮流报"，
// 证据一多真正的那份就被淹掉，而且同一责任方的冷却期可能正好把它挡在门外。
func TestIncidentRateLimits(t *testing.T) {
	dir := t.TempDir()
	item := func(cls, name string, pid int) diagnosis.Item {
		return diagnosis.Item{Level: diagnosis.Warning, Class: cls, Title: cls + " 劣化",
			Culprits: []diagnosis.Culprit{{Kind: "process", PID: pid, Name: name}}}
	}
	var chain diagnosis.Chain
	r, err := NewRecorder(dir, t.TempDir(), func() *diagnosis.Chain { return &chain }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.kmsgPath = dir + "/nokmsg"
	now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)

	// 1) 同一责任方换了 PID（崩溃重启循环）不该被当成新事故
	chain = diagnosis.Chain{Items: []diagnosis.Item{item(diagnosis.ClassCPU, "flaky", 100)}}
	if got := r.Tick(now); len(got) != 1 {
		t.Fatalf("首次该留证：%v", got)
	}
	chain = diagnosis.Chain{Items: []diagnosis.Item{item(diagnosis.ClassCPU, "flaky", 200)}}
	if got := r.Tick(now.Add(6 * time.Minute)); len(got) != 0 {
		t.Errorf("同一责任方换 PID 不该再留一份（崩溃循环会把额度占满）：%v", got)
	}

	// 2) 一小时总量上限：四个类别轮流报也要被挡住
	total := 1
	classes := []string{diagnosis.ClassCPU, diagnosis.ClassIO, diagnosis.ClassMem, diagnosis.ClassNet}
	for i := 0; i < 24; i++ { // 48 分钟内轮流报，全在一小时窗口内
		cls := classes[i%len(classes)]
		chain = diagnosis.Chain{Items: []diagnosis.Item{item(cls, cls+"-proc", 300+i)}}
		total += len(r.Tick(now.Add(time.Duration(10+i*2) * time.Minute)))
	}
	if total > incidentMaxPerHour {
		t.Errorf("一小时内留证 %d 份，超过上限 %d", total, incidentMaxPerHour)
	}
	if total < 2 {
		t.Errorf("限流过头了，一小时只留了 %d 份", total)
	}
	t.Logf("一小时内共留证 %d 份（上限 %d）", total, incidentMaxPerHour)
}
