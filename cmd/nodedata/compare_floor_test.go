package main

import (
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
)

// 实测截图：盘 util 0.08% → 0.52% 报 ▲550%，IO 压力 0.01% → 0.14% 报 ▲1.3k%。
// 这台机器基本在睡觉——分母接近 0 时百分比是垃圾。
// z-score 那条路径 v5.14.0 已有门槛，对比表是另一条路径，之前绕开了。
func TestCompareRowsCarryFloor(t *testing.T) {
	s := NewSeries()
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	for ts := now.Add(-2 * time.Hour); !ts.After(now); ts = ts.Add(time.Minute) {
		s.Add([]collector.Sample{
			{MetricID: "disk.util", TS: ts, Value: 0.5},
			{MetricID: "mem.available", TS: ts, Value: 13e9},
			{MetricID: "net.rx", TS: ts, Value: 200},
		})
	}
	c := NewHeatmapBuilder(s).Compare(now)
	want := map[string]float64{
		"最忙盘 util %": 3,         // 百分比：3 个百分点
		"可用内存":       128 << 20, // 字节：128MB
		"网卡入向":       1 << 20,   // 吞吐：1MB/s
	}
	seen := 0
	for _, g := range c.Groups {
		for _, r := range g.Rows {
			if w, ok := want[r.Label]; ok {
				seen++
				if r.Floor != w {
					t.Errorf("%s 的门槛 = %v，期望 %v", r.Label, r.Floor, w)
				}
			}
		}
	}
	if seen != len(want) {
		t.Fatalf("只核对到 %d 行，期望 %d 行", seen, len(want))
	}
}

// 看到"写 IOPS 涨了 10.8k%"，下一个问题必然是"谁写的"。
func TestCompareMoversAnswerWho(t *testing.T) {
	s := NewSeries()
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	for ts := now.Add(-2 * time.Hour); !ts.After(now); ts = ts.Add(time.Minute) {
		busy := 0.0
		if ts.After(now.Add(-10 * time.Minute)) {
			busy = 600 << 20 // 最近 10 分钟一个进程在狂写
		}
		s.Add([]collector.Sample{
			{MetricID: "disk.util", TS: ts, Value: 50},
			{MetricID: "disk.wbytes", TS: ts, Value: busy},
			{MetricID: "proc.io.rsync", TS: ts, Value: busy},
			{MetricID: "proc.io.sshd", TS: ts, Value: 1024}, // 小打小闹，不该上榜
		})
	}
	c := NewHeatmapBuilder(s).Compare(now)
	for _, g := range c.Groups {
		if g.Name != "磁盘" {
			continue
		}
		if len(g.Movers) == 0 {
			t.Fatal("磁盘组该给出\"谁写的\"")
		}
		if g.Movers[0].Name != "rsync" {
			t.Errorf("变化最大的应是 rsync，实得 %s", g.Movers[0].Name)
		}
		for _, m := range g.Movers {
			if m.Name == "sshd" {
				t.Errorf("1KB/s 的进程不该上榜：%+v", m)
			}
		}
		return
	}
	t.Fatal("没有磁盘组")
}

// 重启后原始层是空的，只有落盘的长期层（5 分钟一格）。此刻离格点差 2.5 分钟时，
// "1小时前"那一列也必须有数——原来容差只有 36 秒，这一列在重启后头一个小时基本是空的。
func TestCompareOneHourColumnAfterRestart(t *testing.T) {
	s := NewSeries()
	grid := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	for ts := grid.Add(-3 * time.Hour); !ts.After(grid); ts = ts.Add(5 * time.Minute) {
		s.AddCoarse([]collector.Sample{{MetricID: "disk.util", TS: ts, Value: 7}})
	}
	now := grid.Add(2*time.Minute + 30*time.Second) // 离最近格点最远的位置
	s.Add([]collector.Sample{{MetricID: "disk.util", TS: now, Value: 7}})

	c := NewHeatmapBuilder(s).Compare(now)
	for _, g := range c.Groups {
		for _, r := range g.Rows {
			if r.Label != "最忙盘 util %" {
				continue
			}
			if len(r.Past) == 0 || r.Past[0] == nil {
				t.Fatalf("重启后只有长期层时，1小时前那一列不该是空的")
			}
			return
		}
	}
	t.Fatal("没找到 最忙盘 util % 这一行")
}
