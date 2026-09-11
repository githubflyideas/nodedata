package main

import (
	"runtime"
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
)

func TestCompare(t *testing.T) {
	s := NewSeries()
	now := time.Date(2026, 9, 11, 14, 0, 0, 0, time.UTC)
	// 长期层：8 天前到 25 小时前，mem.available 恒为 8GiB；原始层：最近 24 小时为 4GiB
	for ts := now.Add(-8 * 24 * time.Hour); ts.Before(now.Add(-25 * time.Hour)); ts = ts.Add(coarseStep) {
		s.AddCoarse([]collector.Sample{{MetricID: "mem.available", TS: ts, Value: 8 << 30}})
	}
	for ts := now.Add(-24 * time.Hour); !ts.After(now); ts = ts.Add(5 * time.Second) {
		s.Add([]collector.Sample{
			{MetricID: "mem.available", TS: ts, Value: 4 << 30},
			{MetricID: "cpu.user", TS: ts, Value: 100}, {MetricID: "cpu.sys", TS: ts, Value: 50}, {MetricID: "cpu.softirq", TS: ts, Value: 10},
			{MetricID: "disk.util", TS: ts, Value: 30}, {MetricID: "disk.util@sda", TS: ts, Value: 30},
			{MetricID: "net.rx", TS: ts, Value: 1000}, {MetricID: "net.rx@eth0", TS: ts, Value: 1000},
		})
	}
	// sdb 只在最近 2 小时出现
	for ts := now.Add(-2 * time.Hour); !ts.After(now); ts = ts.Add(5 * time.Second) {
		s.Add([]collector.Sample{{MetricID: "disk.util@sdb", TS: ts, Value: 90}})
	}
	// sdc 一直空闲：不该占行
	for ts := now.Add(-2 * time.Hour); !ts.After(now); ts = ts.Add(5 * time.Second) {
		s.Add([]collector.Sample{{MetricID: "disk.util@sdc", TS: ts, Value: 0}})
	}
	c := NewHeatmapBuilder(s).Compare(now)
	rows := map[string]cmpRow{}
	for _, g := range c.Groups {
		for _, r := range g.Rows {
			rows[r.Label] = r
		}
	}
	mem := rows["可用内存"]
	if *mem.Now != 4<<30 || *mem.Past[0] != 4<<30 || mem.Past[3] == nil || *mem.Past[4] != 8<<30 || *mem.Past[5] != 8<<30 {
		t.Fatalf("mem.available past values wrong: %+v", mem.Past)
	}
	busy := rows["CPU 忙碌 %"]
	if want := 160 / float64(runtime.NumCPU()); *busy.Now != want {
		t.Fatalf("CPU 忙碌 = %v, want (user+sys+softirq)/NumCPU = %v", *busy.Now, want)
	}
	if _, ok := rows["swap 使用"]; ok {
		t.Fatalf("metric absent on this host must be omitted, not shown as 0")
	}
	sdb, ok := rows["util % · sdb"]
	if !ok || *sdb.Now != 90 || sdb.Past[1] != nil {
		t.Fatalf("per-disk row: ok=%v %+v（6h 前 sdb 不存在，必须是 null 而不是 0）", ok, sdb.Past)
	}
	if _, ok := rows["util % · sdc"]; ok {
		t.Fatalf("an always-idle disk must not get a row")
	}
	if _, ok := rows["入向 · eth0"]; ok {
		t.Fatalf("single NIC must not get per-NIC rows")
	}
	if len(c.Cols) != 6 || c.Cols[0] != "1h" || c.Cols[5] != "7d" {
		t.Fatalf("cols = %v", c.Cols)
	}
}
