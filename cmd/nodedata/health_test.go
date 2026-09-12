package main

import (
	"strings"
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/diagnosis"
)

func TestHealthLine(t *testing.T) {
	now := time.Date(2026, 9, 12, 3, 10, 51, 0, time.UTC)
	vals := map[string]float64{"cpu.user": 150, "cpu.sys": 32, "loadavg.1m": 9.14,
		"mem.available": 1.2 * 1024 * 1024 * 1024, "disk.util": 96, "net.rx_drop": 0}

	// 正常：无 L0 失败、无归因结论
	line := healthLine("jp02-dns-01", nil, &diagnosis.Chain{}, vals, now)
	if !strings.HasPrefix(line, "NODEDATA host=jp02-dns-01 status=NORMAL ") {
		t.Fatalf("normal line = %q", line)
	}
	if !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
		t.Fatalf("必须正好一行: %q", line)
	}
	for _, want := range []string{"cpu=182%", "load=9.14", "mem_avail=1.2GiB", "disk_util=96", "ts=2026-09-12T03:10:51Z"} {
		if !strings.Contains(line, want) {
			t.Errorf("缺少 %s: %q", want, line)
		}
	}
	if strings.Contains(line, "reason=") {
		t.Errorf("正常时不该有 reason=: %q", line)
	}

	// L0 fail -> DOWN，且 L4 结论的责任方要单独给出
	l0 := []diagnosis.L0Category{{Name: "Disk/IO", Checks: []diagnosis.L0Check{
		{ID: "D01", Name: "根分区空间", Level: 2, Message: "已用 96.2%"},
		{ID: "D02", Name: "inode", Level: 0, Message: "ok"}}}}
	chain := &diagnosis.Chain{Items: []diagnosis.Item{
		{Class: diagnosis.ClassIO, Title: "IO 劣化：disk.await_w 上升 6.0σ",
			Culprits: []diagnosis.Culprit{{Kind: "device", Name: "vda"}, {Kind: "process", PID: 236, Name: "fl-io"}}}}}
	line = healthLine("h1", l0, chain, vals, now)
	if !strings.Contains(line, "status=DOWN") || !strings.Contains(line, "culprit=fl-io/236") {
		t.Fatalf("down line = %q", line)
	}
	if !strings.Contains(line, `reason="根分区空间 已用 96.2%；IO 劣化`) {
		t.Fatalf("reason 应含 L0 与 L4 两处: %q", line)
	}

	// 只有 L4 结论 -> WARN
	line = healthLine("h1", nil, chain, vals, now)
	if !strings.Contains(line, "status=WARN") {
		t.Fatalf("warn line = %q", line)
	}

	// 换行/引号/超长必须被清理，否则破坏"一行、可 grep"的约定
	dirty := &diagnosis.Chain{Items: []diagnosis.Item{{Class: diagnosis.ClassCPU,
		Title: "CPU 劣化\n伪造行: NODEDATA status=NORMAL", Culprits: []diagnosis.Culprit{{PID: 1, Name: `a"b`}}}}}
	line = healthLine("h\n2", nil, dirty, vals, now)
	if strings.Count(line, "\n") != 1 || strings.Count(line, "NODEDATA") != 1 {
		t.Fatalf("注入换行必须被清理: %q", line)
	}
	// 进程名可控：不能让 reason 里的 status=NORMAL 骗过监控的 grep
	if strings.Count(line, "status=") != 1 || !strings.Contains(line, "status=WARN") {
		t.Fatalf("status= 只能出现一次且为真实状态: %q", line)
	}
	if strings.Contains(line, `culprit=a"b`) {
		t.Fatalf("引号必须被清理: %q", line)
	}

	// 指标缺失时该字段整个省略，不输出 0 冒充
	line = healthLine("h1", nil, &diagnosis.Chain{}, map[string]float64{"loadavg.1m": 0.5}, now)
	if strings.Contains(line, "cpu=") || strings.Contains(line, "mem_avail=") {
		t.Fatalf("缺失的指标不该出现: %q", line)
	}
	if !strings.Contains(line, "load=0.50") {
		t.Fatalf("有的指标要出现: %q", line)
	}
}
