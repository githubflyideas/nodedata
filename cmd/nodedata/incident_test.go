package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/diagnosis"
)

func fakeTask(t *testing.T, dir string, id int, comm string, state byte, utime uint64) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	line := fmt.Sprintf("%d (%s) %c 1 1 1 0 -1 0 0 0 0 0 %d 0 0 0 20 0 1 0 100 0 0\n", id, comm, state, utime)
	os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o644)
	os.WriteFile(filepath.Join(dir, "wchan"), []byte("do_epoll_wait"), 0o644)
	os.WriteFile(filepath.Join(dir, "stack"), []byte("[<0>] do_epoll_wait+0x1/0x2\n[<0>] __x64_sys_epoll_wait+0x3/0x4\n"), 0o644)
}

func TestRecorderCapture(t *testing.T) {
	proc, dir := t.TempDir(), t.TempDir()
	fakeTask(t, filepath.Join(proc, "500"), 500, "burner", 'R', 1000)
	fakeTask(t, filepath.Join(proc, "500", "task", "500"), 500, "burner", 'R', 900)
	fakeTask(t, filepath.Join(proc, "500", "task", "501"), 501, "worker-1", 'R', 100)
	fakeTask(t, filepath.Join(proc, "600"), 600, "jbd2/sda1-8", 'D', 5)
	os.WriteFile(filepath.Join(proc, "500", "status"), []byte("Name:\tburner\nVmRSS:\t1024 kB\n"), 0o644)
	os.WriteFile(filepath.Join(proc, "500", "cmdline"), []byte("burner\x00--password=hunter2"), 0o644)
	os.WriteFile(filepath.Join(proc, "loadavg"), []byte("9.00 5.00 1.00 3/400 12345\n"), 0o644)
	kmsg := filepath.Join(t.TempDir(), "kmsg")
	os.WriteFile(kmsg, []byte("6,1,1000000,-;first line\n SUBSYSTEM=x\n3,2,2500000,-;EXT4-fs error (device sda1): bad\n"), 0o644)

	item := diagnosis.Item{Level: diagnosis.Warning, Class: diagnosis.ClassCPU, Title: "CPU 劣化 — 责任方 burner（PID 500）",
		Culprits: []diagnosis.Culprit{{Kind: "process", PID: 500, Name: "burner"}}}
	r, err := NewRecorder(dir, proc, func() *diagnosis.Chain { return &diagnosis.Chain{Items: []diagnosis.Item{item}} }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.kmsgPath = kmsg
	now := time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC)

	ids := r.Tick(now)
	if len(ids) != 1 {
		t.Fatalf("first tick should capture once, got %v", ids)
	}
	if again := r.Tick(now.Add(20 * time.Second)); len(again) != 0 {
		t.Fatalf("same signature within cooldown must not capture again: %v", again)
	}
	b, err := r.Get(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	var ev Evidence
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatal(err)
	}
	if th := ev.Threads["500"]; len(th) != 2 || th[0].Wchan != "do_epoll_wait" || len(th[0].Stack) != 2 {
		t.Fatalf("threads = %+v", ev.Threads)
	}
	if len(ev.DState) != 1 || ev.DState[0].PID != 600 || len(ev.DState[0].Stack) == 0 {
		t.Fatalf("d_state = %+v", ev.DState)
	}
	if len(ev.Kernel) != 2 || ev.Kernel[1] != "[2.500000] EXT4-fs error (device sda1): bad" {
		t.Fatalf("kernel_log = %q", ev.Kernel)
	}
	if ev.System["loadavg"] == "" || ev.ProcDetail["500"].Status == "" {
		t.Fatalf("system/proc detail missing")
	}
	if strings.Contains(string(b), "hunter2") {
		t.Fatalf("cmdline (may contain secrets) must never be captured")
	}
	if l := r.List(); len(l) != 1 || l[0].Culprit == nil || l[0].Culprit.PID != 500 {
		t.Fatalf("list = %+v", l)
	}
	if _, err := r.Get("../../etc/passwd"); err == nil {
		t.Fatalf("path traversal must be rejected")
	}
	// 保留上限
	for i := 0; i < incidentKeep+5; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("20200101T0000%02dZ-cpu.json", i)), []byte("{}"), 0o640)
	}
	r.expire()
	if files, _ := filepath.Glob(filepath.Join(dir, "*.json")); len(files) != incidentKeep {
		t.Fatalf("keep %d, got %d", incidentKeep, len(files))
	}
}

// TestKernelLogNeverBlocks：可 poll、有写端但没有数据的描述符（与空闲的 /dev/kmsg 同类），
// 读取必须立即返回。v3.1.0 开发中用 os.File 读 /dev/kmsg，Go 的 netpoller 把 EAGAIN
// 变成"等下一条内核消息"，留证 goroutine 实测卡死 4 分钟、之后所有留证停摆。
func TestKernelLogNeverBlocks(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "kmsg")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("mkfifo unavailable:", err)
	}
	w, err := os.OpenFile(fifo, os.O_RDWR, 0) // 保持一个写端：读端拿到的是 EAGAIN 而不是 EOF
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	r := &Recorder{kmsgPath: fifo}
	done := make(chan error, 1)
	go func() { _, err := r.kernelLog(); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("kernelLog: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("kernelLog blocked on an idle pollable descriptor")
	}
}
