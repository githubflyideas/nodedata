package main

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
)

func writeDays(t *testing.T, dir string, now time.Time, days int) {
	t.Helper()
	h, err := NewHistory(dir, coarseRetention)
	if err != nil {
		t.Fatal(err)
	}
	for ts := now.Add(-time.Duration(days) * 24 * time.Hour); ts.Before(now); ts = ts.Add(coarseStep) {
		if err := h.Append([]collector.Sample{
			{MetricID: "disk.await_w", TS: ts, Value: 3},
			{MetricID: "cpu.user", TS: ts, Value: 10},
		}); err != nil {
			t.Fatal(err)
		}
	}
	h.Close()
}

func listDir(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var n []string
	for _, e := range ents {
		n = append(n, e.Name())
	}
	sort.Strings(n)
	return n
}

// 峰值行写下去、读回来，3 天前那一次尖峰还在。
func TestPeaksSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	writeDays(t, dir, now, 4)
	spike := time.Date(2026, 9, 22, 2, 10, 0, 0, time.UTC)
	h, _ := NewHistory(dir, coarseRetention)
	if err := h.AppendPeaks([]Peak{{MetricID: "disk.await_w", T: spike.Unix(), P: 42}}, now); err != nil {
		t.Fatal(err)
	}
	h.Close()

	s := NewSeries()
	h2, _ := NewHistory(dir, coarseRetention)
	if _, _, err := h2.Load(s, now); err != nil {
		t.Fatal(err)
	}
	if w, ok := s.Worst("disk.await_w", spike.Add(-time.Hour), spike.Add(time.Hour)); !ok || w != 42 {
		t.Fatalf("重启后 3 天前的峰值应还在：%v/%v", w, ok)
	}
}

// 今天以前的文件压成 .gz，今天的保持纯文本；读回的点数跟压缩前完全一样。
func TestOldDaysCompressedAndStillReadable(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	writeDays(t, dir, now, 4)

	var before int
	{
		s := NewSeries()
		// 先数一遍纯文本时的点数（Load 结束时才压缩，所以这次读的全是 .jsonl）
		h, _ := NewHistory(dir, coarseRetention)
		_, before, _ = h.Load(s, now)
	}
	names := listDir(dir)
	for _, n := range names {
		if strings.HasPrefix(n, "2026-09-25") {
			if n != "2026-09-25.jsonl" {
				t.Errorf("今天的文件应保持纯文本：%v", names)
			}
		} else if !strings.HasSuffix(n, ".jsonl.gz") {
			t.Errorf("今天以前的文件应已压缩：%v", names)
		}
	}
	s := NewSeries()
	h, _ := NewHistory(dir, coarseRetention)
	_, after, _ := h.Load(s, now)
	if after != before || before == 0 {
		t.Errorf("压缩前读到 %d 个点，压缩后读到 %d 个", before, after)
	}
}

// 压完没来得及删原文件就崩了：两份都在时只读一份，不能重复计点；下次压缩时删掉原文件。
func TestBothCopiesPresentReadOnce(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	writeDays(t, dir, now, 2)
	plain := filepath.Join(dir, "2026-09-24.jsonl")
	// 换天时昨天已经被压缩了；把原文件还原回来，模拟"压完、改名完，还没删原文件"
	f, err := os.Open(plain + ".gz")
	if err != nil {
		t.Fatal(err)
	}
	zr, _ := gzip.NewReader(f)
	orig, _ := io.ReadAll(zr)
	f.Close()
	os.WriteFile(plain, orig, 0o644)

	s := NewSeries()
	h, _ := NewHistory(dir, coarseRetention)
	lines, _, _ := h.Load(s, now)
	if want := 2 * 288; lines != want {
		t.Errorf("两份都在时读了 %d 行，应为 %d（重复计点了）", lines, want)
	}
	if _, err := os.Stat(plain); err == nil {
		t.Error("原文件应在压缩整理时删掉")
	}
}

// 过期的 .gz 也要删：只删 .jsonl 的话压缩过的旧文件会永远留着。
func TestExpireRemovesCompressed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "2026-08-01.jsonl.gz"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "2026-09-24.jsonl.gz"), []byte("x"), 0o644)
	h, _ := NewHistory(dir, coarseRetention)
	h.expire(time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC))
	if got := listDir(dir); len(got) != 1 || got[0] != "2026-09-24.jsonl.gz" {
		t.Errorf("过期的压缩文件没删：%v", got)
	}
}

// 压到一半进程被杀留下的 .tmp：下次整理时清掉，不能越积越多。
func TestStaleTmpCleaned(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "2026-09-20.jsonl.gz.tmp")
	os.WriteFile(stale, []byte("half"), 0o644)
	h, _ := NewHistory(dir, coarseRetention)
	h.compactOld(time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC))
	if _, err := os.Stat(stale); err == nil {
		t.Error("半截的 .tmp 没清掉")
	}
}
