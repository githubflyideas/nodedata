package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSIGTERMWritesDownEvent：systemctl stop 发的是 SIGTERM，Go 默认直接退出、defer 不跑。
// 少了 down 事件，重启后回看这段停机时间，状态机以为"我们一直在看"，
// 把服务显示成"确实不在"而不是"不知道"——等于告诉运维"你的服务那天挂了"。
func TestSIGTERMWritesDownEvent(t *testing.T) {
	if testing.Short() {
		t.Skip("需要真的起一个进程")
	}
	bin := filepath.Join(t.TempDir(), "nodedata")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Skipf("构建失败（沙箱可能没有工具链）: %v %s", err, out)
	}
	dir := t.TempDir()
	cmd := exec.Command(bin, "serve", "--port", "19771", "--data-dir", dir)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	// 等服务识别跑完第一轮（启动时立刻 tick 一次）
	path := filepath.Join(dir, "services.jsonl")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), `"k":"up"`) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("收到 SIGTERM 后没有退出")
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"k":"down"`) {
		t.Fatalf("SIGTERM 之后必须写 down 事件，实际内容：\n%s", b)
	}
}
