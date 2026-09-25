package main

import (
	"runtime"
	"strings"
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
	// 进程序列：java 占 6GiB、mysqld 占 1GiB
	for ts := now.Add(-24 * time.Hour); !ts.After(now); ts = ts.Add(5 * time.Second) {
		s.Add([]collector.Sample{
			{MetricID: "proc.rss.java", TS: ts, Value: 6 << 30},
			{MetricID: "proc.cpu.java", TS: ts, Value: 40},
			{MetricID: "proc.rss.mysqld", TS: ts, Value: 1 << 30},
			{MetricID: "proc.cpu.__others__", TS: ts, Value: 3},
		})
	}
	c := NewHeatmapBuilder(s).Compare(now)
	rows := map[string]cmpRow{}
	for _, g := range c.Groups {
		for _, r := range g.Rows {
			rows[r.Label] = r
		}
	}
	// 按列名取，不按下标：时间点列表改过一次（6 列 → 10 列），下标写死的测试全得跟着改
	col := func(name string) int {
		for i, c := range c.Cols {
			if c == name {
				return i
			}
		}
		t.Fatalf("没有 %s 列：%v", name, c.Cols)
		return -1
	}
	mem := rows["可用内存"]
	if *mem.Now != 4<<30 || *mem.Past[col("1h")] != 4<<30 || mem.Past[col("1d")] == nil ||
		*mem.Past[col("3d")] != 8<<30 || *mem.Past[col("7d")] != 8<<30 {
		t.Fatalf("mem.available past values wrong: %+v", mem.Past)
	}
	// 14 天前没有数据（夹具只造了 8 天）：必须是 null，不能是 0
	if mem.Past[col("14d")] != nil {
		t.Fatalf("14d 前没有数据，应为 null：%v", *mem.Past[col("14d")])
	}
	busy := rows["CPU 忙碌 %"]
	if want := 160 / float64(runtime.NumCPU()); *busy.Now != want {
		t.Fatalf("CPU 忙碌 = %v, want (user+sys+softirq)/NumCPU = %v", *busy.Now, want)
	}
	if _, ok := rows["swap 使用"]; ok {
		t.Fatalf("metric absent on this host must be omitted, not shown as 0")
	}
	sdb, ok := rows["util % · sdb"]
	if !ok || *sdb.Now != 90 || sdb.Past[col("6h")] != nil {
		t.Fatalf("per-disk row: ok=%v %+v（6h 前 sdb 不存在，必须是 null 而不是 0）", ok, sdb.Past)
	}
	if _, ok := rows["util % · sdc"]; ok {
		t.Fatalf("an always-idle disk must not get a row")
	}
	if _, ok := rows["入向 · eth0"]; ok {
		t.Fatalf("single NIC must not get per-NIC rows")
	}
	if len(c.Cols) != len(timePoints) || c.Cols[0] != "5m" || c.Cols[len(c.Cols)-1] != "14d" {
		t.Fatalf("cols = %v", c.Cols)
	}
	// 进程组：内存大的在前，__others__ 不当成进程
	var pg *cmpGroup
	for i := range c.Groups {
		if c.Groups[i].Name == "进程" {
			pg = &c.Groups[i]
		}
	}
	if pg == nil || pg.Rows[0].Label != "java 内存" || pg.Rows[1].Label != "java CPU" {
		t.Fatalf("process group = %+v", pg)
	}
	for _, r := range pg.Rows {
		if strings.Contains(r.Label, "__others__") {
			t.Fatalf("__others__ must not appear as a process")
		}
	}
	// 默认只显示核心行；其余归入"更多指标"
	nCore, nMore := 0, 0
	for _, g := range c.Groups {
		for _, r := range g.Rows {
			if r.Core {
				nCore++
			} else {
				nMore++
			}
		}
	}
	if nCore == 0 || nMore == 0 || nCore > 20 {
		t.Fatalf("core=%d more=%d：默认行数要少而关键", nCore, nMore)
	}
	for _, g := range c.Groups {
		for _, r := range g.Rows {
			if strings.Contains(r.ID, "@") && r.Core {
				t.Fatalf("逐设备行不应默认显示: %s", r.Label)
			}
		}
	}
}
