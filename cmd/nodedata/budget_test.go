package main

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
)

// 预算（v5.20）：轻量要能量化，不然每加一个功能都会悄悄长一点。
//
// 实测（150 条序列，原始层满 24 小时、长期层满 14 天）：
//
//	          数据     查询后常驻   查询期间堆峰值   σ 重算一轮分配
//	v5.19    52.8MB     54.4MB        115.7MB          368MB
//	v5.20    32.1MB     33.6MB         51.7MB          3.2MB
//
// 数据 32MB 是 24 小时 × 10 秒分辨率的原始层本身，不压精度就下不去；
// 进程常驻内存在此之上再加 Go 运行时和 GC 余量（main 里把 GC 触发点设在 1.5 倍）。
const (
	budgetSeries     = 150      // 按 150 条序列算（实测一台机器 134 条）
	budgetDataBytes  = 34 << 20 // 两层数据本身
	budgetHeapBytes  = 40 << 20 // 页面查询跑过一轮、GC 之后的 Go 堆
	budgetSigmaAlloc = 16 << 20 // σ 重算一轮的分配量：每 5 分钟一次，超了就是在制造垃圾
)

// fullSeries 造一个"跑满了"的序列集：原始层满 24 小时、长期层满 14 天。
func fullSeries(n int, now time.Time) *Series {
	s := NewSeries()
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("proc.cpu.p%03d", i)
	}
	ids[0], ids[1], ids[2] = "cpu.user", "disk.await_w", "mem.available"
	batch := make([]collector.Sample, n)
	for ts := now.Add(-coarseRetention); ts.Before(now.Add(-24 * time.Hour)); ts = ts.Add(coarseStep) {
		for i, id := range ids {
			batch[i] = collector.Sample{MetricID: id, TS: ts, Value: float64(i)}
		}
		s.AddCoarse(batch)
	}
	for ts := now.Add(-24 * time.Hour); !ts.After(now); ts = ts.Add(10 * time.Second) {
		for i, id := range ids {
			batch[i] = collector.Sample{MetricID: id, TS: ts, Value: float64(i) + float64(ts.Unix()%7)}
		}
		s.Add(batch)
	}
	return s
}

func heapInUse() uint64 {
	runtime.GC()
	debug.FreeOSMemory()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse
}

func TestMemoryBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("造满 24 小时原始层要几秒")
	}
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	before := heapInUse()
	s := fullSeries(budgetSeries, now)
	data := heapInUse() - before

	// 页面每 20 秒会调的那几个：跑一轮，看堆涨到多少
	b := NewHeatmapBuilder(s)
	b.RefreshSigma()
	b.Latest(now)
	b.Compare(now)
	b.KeySeries("24h", 24*time.Hour, now)
	b.KeySeries("14d", 14*24*time.Hour, now)
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	after := heapInUse() - before

	t.Logf("%d 条序列：数据 %.1fMB，查询后常驻 %.1fMB，查询期间堆峰值 %.1fMB",
		budgetSeries, float64(data)/(1<<20), float64(after)/(1<<20), float64(m.HeapSys)/(1<<20))
	if data > budgetDataBytes {
		t.Errorf("数据 %.1fMB 超预算 %dMB", float64(data)/(1<<20), budgetDataBytes>>20)
	}
	if after > budgetHeapBytes {
		t.Errorf("查询后常驻 %.1fMB 超预算 %dMB", float64(after)/(1<<20), budgetHeapBytes>>20)
	}
	runtime.KeepAlive(s)
	runtime.KeepAlive(b)
}

// σ 每 5 分钟重算一遍。v5.19 一轮分配 368MB 短命内存，常驻内存被 GC 节奏顶到 70MB 以上。
func TestSigmaRefreshAllocBudget(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	b := NewHeatmapBuilder(fullSeries(budgetSeries, now))
	b.RefreshSigma() // 第一次会建 σ 表本身，不算
	var a, c runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	b.RefreshSigma()
	runtime.ReadMemStats(&c)
	got := c.TotalAlloc - a.TotalAlloc
	t.Logf("σ 重算一轮分配 %.1fMB", float64(got)/(1<<20))
	if got > budgetSigmaAlloc {
		t.Errorf("σ 重算一轮分配 %.1fMB，超预算 %dMB", float64(got)/(1<<20), budgetSigmaAlloc>>20)
	}
}

// 磁盘预算：150 条序列、满精度随机值（最难压的情况；真实数据压得更好）。
//
//	          一天纯文本   一天 gzip   14 天
//	v5.19       1742KB       —        ≈ 24MB（不压缩）
//	v5.20       1297KB     247KB      ≈ 4.4MB（旧日子 gzip + 6 位有效数字）
const (
	budgetDailyWrite = 2 << 20 // 每天写盘：长期层一天的纯文本
	budgetDisk14d    = 5 << 20 // 14 天：13 天 gzip + 今天纯文本
)

func TestDiskBudget(t *testing.T) {
	dir := t.TempDir()
	h, _ := NewHistory(dir, coarseRetention)
	r := rand.New(rand.NewSource(1))
	day := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	ids := make([]string, budgetSeries)
	for i := range ids {
		ids[i] = fmt.Sprintf("proc.cpu.process-%03d", i)
	}
	copy(ids, []string{"cpu.user", "cpu.sys", "mem.available", "disk.await_w@nvme0n1", "net.rx@eth0", "fs.inode_used_pct"})
	for ts := day; ts.Before(day.Add(24 * time.Hour)); ts = ts.Add(coarseStep) {
		b := make([]collector.Sample, len(ids))
		for i, id := range ids {
			b[i] = collector.Sample{MetricID: id, TS: ts, Value: r.Float64() * float64(1+i*37)}
		}
		h.Append(b)
	}
	h.Close()
	plain, err := os.ReadFile(filepath.Join(dir, "2026-09-24.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := gzipFile(filepath.Join(dir, "2026-09-24.jsonl"), filepath.Join(dir, "x.gz")); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(filepath.Join(dir, "x.gz"))
	total := st.Size()*13 + int64(len(plain))
	t.Logf("一天纯文本 %.0fKB，gzip %.0fKB，14 天 ≈ %.1fMB",
		float64(len(plain))/1024, float64(st.Size())/1024, float64(total)/(1<<20))
	if len(plain) > budgetDailyWrite {
		t.Errorf("每天写盘 %.0fKB 超预算 %dMB", float64(len(plain))/1024, budgetDailyWrite>>20)
	}
	if total > budgetDisk14d {
		t.Errorf("14 天 %.1fMB 超预算 %dMB", float64(total)/(1<<20), budgetDisk14d>>20)
	}
}

// 6 位有效数字：误差远小于任何门槛。
func TestSig6(t *testing.T) {
	for in, want := range map[float64]float64{
		1.6489923000335693: 1.64899, 7847669760: 7.84767e9, 0.000123456789: 0.000123457, 0: 0, -3.14159265: -3.14159,
	} {
		if got := sig6(in); got != want {
			t.Errorf("sig6(%v) = %v，期望 %v", in, got, want)
		}
	}
}
