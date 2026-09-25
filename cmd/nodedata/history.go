// history.go — 长期层落盘：<data-dir>/history/YYYY-MM-DD.jsonl（UTC 日期），每 5 分钟追加一行。
//
//	{"t":1789079400,"v":{"cpu.user":218.2,"mem.free":2.07e9,...}}
//	{"t":1789079400,"p":{"disk.await_w":42}}        ← 峰值行（v5.20，稀疏，只在有峰值时写）
//
// 今天以前的文件压成 YYYY-MM-DD.jsonl.gz（v5.20），用 zcat 看。
//
// 为什么是这个格式：
//   - 只追加、不改写：每 5 分钟写一次 ~2KB，崩溃最多丢最后一行，半行在读取时跳过；
//   - 按天分文件：保留期到了直接删文件，不需要压缩/合并；
//   - 纯文本：出了问题 tail/grep/jq 就能看，不引入任何依赖。
//
// 规模：~85 条序列 × 288 行/天 × 14 天 ≈ 9MB 磁盘；启动时全量读回（约 4k 行）约 0.25s。
// 这条 5 分钟流也正是将来汇到中心 ClickHouse 的那一份（1000 节点 ≈ 280 行/秒）。
package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
)

type histLine struct {
	T int64              `json:"t"`
	V map[string]float64 `json:"v,omitempty"`
	// P 是峰值行（v5.20）：{"t":桶的时刻,"p":{"指标":最坏值}}。桶结束时才知道最坏值，
	// 所以峰值行写在下一个桶的抽样行之后，时刻指向它所属的桶；只在有峰值时才写。
	P map[string]float64 `json:"p,omitempty"`
}

// History 管理长期层的落盘与回读。
type History struct {
	dir    string
	retain time.Duration

	mu  sync.Mutex
	f   *os.File
	day string

	compactMu sync.Mutex     // 压缩在后台跑，防止两次换天叠在一起
	bg        sync.WaitGroup // Close 要等后台压缩结束，否则读的人可能撞上正在改名的文件
}

func NewHistory(dir string, retain time.Duration) (*History, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &History{dir: dir, retain: retain}, nil
}

func dayOf(t time.Time) string { return t.UTC().Format("2006-01-02") }

// Load 把保留期内的历史装回 s 的长期层，返回读到的行数与点数。
func (h *History) Load(s *Series, now time.Time) (lines, points int, err error) {
	files, err := h.files()
	if err != nil {
		return 0, 0, err
	}
	oldest := dayOf(now.Add(-h.retain))
	for _, name := range files { // 文件名即日期，字典序 = 时间序
		if dayOfName(name) < oldest {
			continue
		}
		f, err := os.Open(filepath.Join(h.dir, name))
		if err != nil {
			continue
		}
		var rd io.Reader = f
		if strings.HasSuffix(name, ".gz") {
			zr, err := gzip.NewReader(f)
			if err != nil {
				f.Close()
				continue
			}
			rd = zr
		}
		sc := bufio.NewScanner(rd)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			var hl histLine
			if json.Unmarshal(sc.Bytes(), &hl) != nil || hl.T == 0 {
				continue // 崩溃留下的半行
			}
			for id, v := range hl.P {
				s.AddPeak(id, hl.T, v)
			}
			if len(hl.V) == 0 {
				continue
			}
			ts := time.Unix(hl.T, 0)
			batch := make([]collector.Sample, 0, len(hl.V))
			for id, v := range hl.V {
				batch = append(batch, collector.Sample{MetricID: id, TS: ts, Value: v})
			}
			s.AddCoarse(batch)
			lines++
			points += len(batch)
		}
		f.Close()
	}
	h.compactOld(now)
	return lines, points, nil
}

// AppendPeaks 落盘一批峰值。写进当天的文件（峰值所属的桶可能是昨天的最后一个，
// 读回时按时刻归位，不依赖写在哪个文件里）。
func (h *History) AppendPeaks(peaks []Peak, now time.Time) error {
	if len(peaks) == 0 {
		return nil
	}
	byT := map[int64]map[string]float64{}
	var order []int64
	for _, p := range peaks {
		if math.IsNaN(p.P) || math.IsInf(p.P, 0) {
			continue
		}
		m, ok := byT[p.T]
		if !ok {
			m = map[string]float64{}
			byT[p.T] = m
			order = append(order, p.T)
		}
		m[p.MetricID] = sig6(p.P)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	var buf []byte
	for _, t := range order {
		b, err := json.Marshal(histLine{T: t, P: byT[t]})
		if err != nil {
			continue
		}
		buf = append(append(buf, b...), '\n')
	}
	if len(buf) == 0 {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.openLocked(dayOf(now), now); err != nil {
		return err
	}
	_, err := h.f.Write(buf)
	return err
}

// Append 落盘一批长期层样本：按时间戳分组，每个时刻一行，但只做一次写系统调用。
//
// 必须按各自的时间戳分组，不能拿第一条的时间戳给整批打标：
// 一批里的样本可能相差一个长期层周期（5 分钟），而短档位配对的容差只有 30 秒，
// 错标时间戳会让这些点永远配不上对。
func (h *History) Append(samples []collector.Sample) error {
	if len(samples) == 0 {
		return nil
	}
	byTS := make(map[int64]map[string]float64, 4)
	var order []int64
	for _, sm := range samples {
		if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) { // JSON 表示不了
			continue
		}
		t := sm.TS.Unix()
		m, ok := byTS[t]
		if !ok {
			m = make(map[string]float64, len(samples))
			byTS[t] = m
			order = append(order, t)
		}
		m[sm.MetricID] = sig6(sm.Value)
	}
	if len(order) == 0 {
		return nil
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })

	var buf []byte
	for _, t := range order {
		b, err := json.Marshal(histLine{T: t, V: byTS[t]})
		if err != nil {
			continue
		}
		buf = append(append(buf, b...), '\n')
	}
	if len(buf) == 0 {
		return nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.openLocked(dayOf(time.Unix(order[0], 0)), time.Unix(order[0], 0)); err != nil {
		return err
	}
	_, err := h.f.Write(buf) // 一次 write 写完整批
	return err
}

// openLocked 确保当前打开的是 day 那天的文件；换天时顺手清过期文件、压缩昨天。
func (h *History) openLocked(day string, now time.Time) error {
	if day == h.day && h.f != nil {
		return nil
	}
	switched := h.f != nil
	if h.f != nil {
		h.f.Close()
	}
	f, err := os.OpenFile(filepath.Join(h.dir, day+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		h.f = nil
		return err
	}
	h.f, h.day = f, day
	h.expire(now)
	if switched {
		// 昨天的文件不会再写了：压缩。放后台，别让采集这一轮等它（1MB 大约几十毫秒）。
		h.bg.Add(1)
		go func() {
			defer h.bg.Done()
			h.compactOld(now)
		}()
	}
	return nil
}

// compactOld 把"今天以前"的 .jsonl 压成 .jsonl.gz。
//
// 键名占每行的三分之二（同样的指标名每 5 分钟写一遍），gzip 对这种重复压得很狠：
// 实测一天 917KB → 190KB。当天的文件保持纯文本，出了问题照样 tail；旧的用 zcat。
//
// 先写 .tmp、fsync、改名，再删原文件：任何一步崩溃，最坏是两份都在（读取时取 .gz），
// 不会丢数据。
func (h *History) compactOld(now time.Time) {
	h.compactMu.Lock()
	defer h.compactMu.Unlock()
	today := dayOf(now)
	ents, err := os.ReadDir(h.dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		n := e.Name()
		if strings.HasSuffix(n, ".jsonl.gz.tmp") {
			os.Remove(filepath.Join(h.dir, n)) // 上次压到一半进程被杀，留下的半截
			continue
		}
		if e.IsDir() || !isDayFile(n) || strings.HasSuffix(n, ".gz") || dayOfName(n) >= today {
			continue
		}
		src := filepath.Join(h.dir, n)
		dst := src + ".gz"
		if _, err := os.Stat(dst); err == nil {
			os.Remove(src) // 上次压完没来得及删
			continue
		}
		if err := gzipFile(src, dst); err != nil {
			os.Remove(dst + ".tmp")
			continue
		}
		os.Remove(src)
	}
}

func gzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	zw, _ := gzip.NewWriterLevel(out, gzip.BestCompression)
	if _, err := io.Copy(zw, in); err != nil {
		out.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// expire 删除保留期外的日文件（纯文本和压缩的都删）。
func (h *History) expire(now time.Time) {
	ents, _ := os.ReadDir(h.dir)
	oldest := dayOf(now.Add(-h.retain))
	for _, e := range ents {
		if n := e.Name(); isDayFile(n) && dayOfName(n) < oldest {
			os.Remove(filepath.Join(h.dir, n))
		}
	}
}

// isDayFile：YYYY-MM-DD.jsonl 或 YYYY-MM-DD.jsonl.gz。
func isDayFile(n string) bool {
	return (strings.HasSuffix(n, ".jsonl") && len(n) == len("2006-01-02.jsonl")) ||
		(strings.HasSuffix(n, ".jsonl.gz") && len(n) == len("2006-01-02.jsonl.gz"))
}

func dayOfName(n string) string { return n[:len("2006-01-02")] }

// files 按日期列出要读的文件；同一天两份都在时（压缩后没来得及删原文件）只取 .gz。
func (h *History) files() ([]string, error) {
	ents, err := os.ReadDir(h.dir)
	if err != nil {
		return nil, err
	}
	byDay := map[string]string{}
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !isDayFile(n) {
			continue
		}
		d := dayOfName(n)
		if cur, ok := byDay[d]; !ok || strings.HasSuffix(n, ".gz") && !strings.HasSuffix(cur, ".gz") {
			byDay[d] = n
		}
	}
	out := make([]string, 0, len(byDay))
	for _, n := range byDay {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

func (h *History) Close() {
	h.bg.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.f != nil {
		h.f.Close()
		h.f = nil
	}
}

// sig6 把值收成 6 位有效数字再落盘（v5.20）。
//
// 采集器吐的是满精度浮点，JSON 会把 17 位全写出来："fs.inode_used_pct":1.6489923000335693。
// 6 位有效数字的相对误差是百万分之一，比任何一个门槛都小几个量级
// （内存 128MB、吞吐 1MB/s、百分比 3 个点），却让文件小一半、压得更狠。
func sig6(v float64) float64 {
	if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	r, err := strconv.ParseFloat(strconv.FormatFloat(v, 'g', 6, 64), 64)
	if err != nil {
		return v
	}
	return r
}
