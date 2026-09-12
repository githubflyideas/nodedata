// procs.go — 进程维度采集：把整机 CPU 的偏离落到具体 PID 上。
//
// v3.0.8 及之前的实现有四个问题，合起来导致"L3 里找不到对应进程"，
// 以及 nodedata 自己吃掉 1.5 个核却从不出现在自己的表里：
//
//  1. 每轮从全部 PID 里随机抽 100 个。一个进程平均要好几轮才被抽中一次，
//     被抽中时的"增量"是距上次被抽中（或进程启动）以来的累计 CPU —— 数是错的，
//     而且首次抽中时 prev=0，直接把进程一辈子的 CPU 当成 5 秒的增量报出来。
//  2. 每轮只输出 top-5 的 comm。序列时有时无，同时段差分攒不够 8 个，
//     z 永远是斜纹；同名进程（多个 php-fpm）在同一时刻写出多个点互相覆盖。
//  3. 用 split-by-space 解析 /proc/PID/stat，comm 里带空格（"Web Content"、
//     "tmux: server"）时 utime/stime 取错字段。
//  4. kworker/u4:2-events_unbound 这类名字随工作项变化，每个变体都成为一条新序列，
//     序列数随运行时长无限增长。
//
// 现在：每轮扫全部 PID（和 top 一样，一个 PID 一次 read），用 starttime 识别 PID 复用，
// 首次见到只记基线不出增量；按归一化名字聚合；被跟踪的名字每轮都出点（空闲出 0），
// 保证序列连续；另外保留"此刻 CPU 前 N 的进程"快照（含 PID），供页面直接给出下一步命令。
//
// v3.1.0：同一轮扫描里再读 /proc/PID/io（read_bytes/write_bytes，块层口径），并记录主缺页、
// 状态（D = 不可中断睡眠，IO 卡住的典型表现）和每 5 分钟一格、共 12 格的 RSS 环，
// 得出"1 小时 RSS 增长"。快照 = CPU 前 N ∪ IO 前 N ∪ RSS 增长前 5 ∪ RSS 最大前 5 ∪ D 状态 ∪ 自身，
// 让 L4 能分别回答"谁在吃 CPU / 谁在写盘 / 谁在涨内存 / 谁卡在 IO 上"。
package collector

import (
	"bytes"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// ProcTopN 是快照里保留的进程数。
	ProcTopN = 10
	// procTrackMinCPU：某个名字的 CPU 占用（占一个核的百分比）达到这个值才开始为它建序列。
	procTrackMinCPU = 1.0
	// procTrackIdle：被跟踪的名字空闲超过这么久就不再出点（序列随后由上层 Prune 回收）。
	procTrackIdle = time.Hour
	// procTrackMax：同时跟踪的名字上限，防止 fork 风暴把序列数撑爆。
	procTrackMax = 32
	// procTrackMinIO：某个名字的块设备读写达到 1MiB/s 才为它建 proc.io.<名字> 序列。
	procTrackMinIO = 1 << 20
	// procRSSTop：为本轮内存占用最高的这么多个名字建 proc.rss.<名字> 序列。
	// 用相对排名而不是绝对阈值：小机器上最大的进程可能只有 30MiB，用绝对阈值就一个进程行都没有。
	// 内存泄漏是"几小时慢慢涨"，只有进程内存也有历史，才能在对比表里看出"比昨天多了多少"。
	procRSSTop = 8
	// rssRingSlots × rssRingStep = RSS 增长的回看窗口（1 小时）。
	rssRingSlots = 12
	rssRingStep  = 5 * time.Minute
	procSnapMax  = 40
)

// ProcTop 是某一时刻单个进程的 CPU 占用。
type ProcTop struct {
	PID  int     `json:"pid"`
	Comm string  `json:"comm"` // /proc/PID/stat 里的原始 comm（内核截断到 15 字节）
	Key  string  `json:"key"`  // 归一化后的名字，对应 proc.cpu.<key> 序列
	CPU  float64 `json:"cpu"`  // 占一个核的百分比，与 cpu.user 等整机指标同单位
	RSS  uint64  `json:"rss"`  // 字节
	Self bool    `json:"self,omitempty"`

	ReadBps    float64 `json:"read_bps"`   // 块设备读，字节/秒（/proc/PID/io read_bytes）
	WriteBps   float64 `json:"write_bps"`  // 块设备写，字节/秒（write_bytes，脏页归属到弄脏它的进程）
	MajFlt     float64 `json:"majflt"`     // 主缺页/秒（要去盘上读页，内存紧张或冷启动）
	RSSGrowth  int64   `json:"rss_growth"` // RSS 相对 GrowthSpan 秒前的变化，字节
	GrowthSpan int     `json:"growth_span"`
	State      string  `json:"state"` // R/S/D/Z…
}

type procPrev struct {
	cpu    uint64 // utime+stime，jiffies
	start  uint64 // starttime，用来识别 PID 复用
	gen    uint32
	rb, wb uint64 // /proc/PID/io read_bytes / write_bytes
	ioOK   bool
	majflt uint64
	ring   [rssRingSlots]uint64 // 每 5 分钟一格的 RSS
	ringTS [rssRingSlots]int64  // 每格写入时刻（unix 秒）；首格是首次见到该进程的时刻
	ringN  int
	ringAt int // 下一格写入位置
}

type procState struct {
	prev       map[int]procPrev
	gen        uint32
	prevTS     time.Time
	tracked    map[string]time.Time // proc.cpu.<名字>：名字 → 最近一次活跃时间
	trackedIO  map[string]time.Time // proc.io.<名字>
	trackedRSS map[string]time.Time // proc.rss.<名字>
	ringSlot   int64                // 当前 RSS 环所在的 5 分钟格
	selfPID    int

	topMu   sync.Mutex
	top     []ProcTop
	topTS   time.Time
	selfCPU float64
	selfRSS uint64
	scanDur time.Duration
	nProcs  int
}

func (p *procState) init() {
	if p.prev == nil {
		p.prev = make(map[int]procPrev, 1024)
		p.tracked = make(map[string]time.Time, procTrackMax)
		p.trackedIO = make(map[string]time.Time, procTrackMax)
		p.trackedRSS = make(map[string]time.Time, procTrackMax)
		p.selfPID = os.Getpid()
	}
}

// ProcKey 把 comm 归一化成稳定的序列名：内核线程去掉 "/" 之后的 CPU 号与工作项
// （kworker/u4:2-events_unbound → kworker，ksoftirqd/3 → ksoftirqd）。
func ProcKey(comm string) string {
	if i := strings.IndexByte(comm, '/'); i > 0 {
		return comm[:i]
	}
	return comm
}

// parseProcStat 解析 /proc/PID/stat。comm 可能含空格与括号，必须以最后一个 ')' 为界。
type procStat struct {
	comm                    string
	state                   byte
	cpu, start, rss, majflt uint64
}

func (c *Collector) parseProcStat(data []byte) (ps procStat, ok bool) {
	l := bytes.IndexByte(data, '(')
	r := bytes.LastIndexByte(data, ')')
	if l < 0 || r <= l || r+2 > len(data) {
		return ps, false
	}
	ps.comm = string(data[l+1 : r])
	f := c.splitFieldsBuf(data[r+2:]) // f[0] 是第 3 个字段 state
	if len(f) < 22 || len(f[0]) == 0 {
		return ps, false
	}
	ut, err1 := parseUint64(f[11])    // 14 utime
	st, err2 := parseUint64(f[12])    // 15 stime
	start, err3 := parseUint64(f[19]) // 22 starttime
	if err1 != nil || err2 != nil || err3 != nil {
		return ps, false
	}
	ps.state = f[0][0]
	ps.majflt, _ = parseUint64(f[9]) // 12 majflt
	pages, _ := parseUint64(f[21])   // 24 rss（页）
	ps.cpu, ps.start, ps.rss = ut+st, start, pages*pageSize
	return ps, true
}

var pageSize = uint64(os.Getpagesize())

// parseProcIO 取 /proc/PID/io 的 read_bytes 与 write_bytes。
func parseProcIO(data []byte) (rb, wb uint64, ok bool) {
	var got int
	for len(data) > 0 {
		var line []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			line, data = data, nil
		}
		k, v, found := bytes.Cut(line, []byte(": "))
		if !found {
			continue
		}
		switch string(k) { // 编译器对 switch string([]byte) 不分配
		case "read_bytes":
			rb, _ = parseUint64(bytes.TrimSpace(v))
			got++
		case "write_bytes":
			wb, _ = parseUint64(bytes.TrimSpace(v))
			got++
		}
	}
	return rb, wb, got == 2
}

// CollectProcs 扫描全部进程，返回 proc.cpu.<key> 序列样本，并刷新 TopProcs 快照。
func (c *Collector) CollectProcs(procRoot string, now time.Time) ([]Sample, error) {
	if procRoot == "" {
		procRoot = c.cfg.ProcRoot
	}
	t0 := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	p := &c.procs
	p.init()

	d, err := os.Open(procRoot)
	if err != nil {
		return nil, err
	}
	names, err := d.Readdirnames(-1)
	d.Close()
	if err != nil && len(names) == 0 {
		return nil, err
	}

	dt := now.Sub(p.prevTS).Seconds()
	hasPrev := !p.prevTS.IsZero() && dt > 0
	p.prevTS = now
	p.gen++

	// RSS 环每 5 分钟推进一格；本轮是否是新格子。
	slot := now.Unix() / int64(rssRingStep/time.Second)
	newSlot := slot != p.ringSlot
	p.ringSlot = slot

	var (
		all      []ProcTop
		byKey    = make(map[string]float64, 64)
		byKeyIO  = make(map[string]float64, 64)
		byKeyRSS = make(map[string]float64, 64)
		total    float64
		scanned  int32
		skipped  int32
		selfCPU  float64
		selfRSS  uint64
		pathBase = procRoot + "/"
	)
	for _, name := range names {
		if name == "" || name[0] < '0' || name[0] > '9' {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		scanned++
		data, err := c.readFileAbs(pathBase + name + "/stat")
		if err != nil {
			skipped++ // 进程在 readdir 与 open 之间退出，正常
			continue
		}
		ps, ok := c.parseProcStat(data)
		if !ok {
			skipped++
			continue
		}
		cur := procPrev{cpu: ps.cpu, start: ps.start, gen: p.gen, majflt: ps.majflt}
		if d, err := c.readFileAbs(pathBase + name + "/io"); err == nil { // 需要 root 或同 uid
			cur.rb, cur.wb, cur.ioOK = parseProcIO(d)
		}
		prev, seen := p.prev[pid]
		reused := seen && prev.start != ps.start
		if seen && !reused { // 继承 RSS 环
			cur.ring, cur.ringTS, cur.ringN, cur.ringAt = prev.ring, prev.ringTS, prev.ringN, prev.ringAt
		}
		if newSlot || cur.ringN == 0 {
			cur.ring[cur.ringAt] = ps.rss
			cur.ringTS[cur.ringAt] = now.Unix()
			cur.ringAt = (cur.ringAt + 1) % rssRingSlots
			if cur.ringN < rssRingSlots {
				cur.ringN++
			}
		}
		p.prev[pid] = cur
		// 首次见到、PID 被复用、计数回退：只记基线，不出增量。
		if !hasPrev || !seen || reused || ps.cpu < prev.cpu {
			continue
		}
		pct := float64(ps.cpu-prev.cpu) / dt // jiffies/s；USER_HZ=100 ⇒ 占一个核的百分比
		key := ProcKey(ps.comm)
		total += pct
		byKey[key] += pct
		byKeyRSS[key] += float64(ps.rss) // 同名多进程（php-fpm、nginx worker）合计
		t := ProcTop{PID: pid, Comm: ps.comm, Key: key, CPU: pct, RSS: ps.rss,
			State: string(ps.state), MajFlt: udiff(ps.majflt, prev.majflt) / dt}
		if cur.ioOK && prev.ioOK {
			t.ReadBps = udiff(cur.rb, prev.rb) / dt
			t.WriteBps = udiff(cur.wb, prev.wb) / dt
			byKeyIO[key] += t.ReadBps + t.WriteBps
		}
		// 相对环里最老的一格算增长；首格就是首次见到的时刻，所以刚开始泄漏的进程
		// 不必等满一个 5 分钟格才看得出增长。
		if cur.ringN > 0 {
			oldest := 0
			if cur.ringN == rssRingSlots {
				oldest = cur.ringAt
			}
			if span := now.Unix() - cur.ringTS[oldest]; span > 0 {
				t.RSSGrowth = int64(ps.rss) - int64(cur.ring[oldest])
				t.GrowthSpan = int(span)
			}
		}
		if pid == p.selfPID {
			t.Self = true
			selfCPU, selfRSS = pct, ps.rss
		}
		all = append(all, t)
	}
	// 回收已退出进程的基线，否则 map 随 PID 周转无限增长。
	for pid, pp := range p.prev {
		if pp.gen != p.gen {
			delete(p.prev, pid)
		}
	}

	top := pickSnapshot(all)

	var out []Sample
	if hasPrev {
		sumTracked := emitTracked(p.tracked, byKey, procTrackMinCPU, "proc.cpu.", now, &out)
		other := total - sumTracked
		if other < 0 {
			other = 0
		}
		out = append(out, Sample{MetricID: "proc.cpu.__others__", TS: now, Value: other})
		emitTracked(p.trackedIO, byKeyIO, procTrackMinIO, "proc.io.", now, &out)
		emitTracked(p.trackedRSS, topByValue(byKeyRSS, procRSSTop), 0, "proc.rss.", now, &out)
	}

	p.topMu.Lock()
	p.top, p.topTS = top, now
	p.selfCPU, p.selfRSS = selfCPU, selfRSS
	p.scanDur = time.Since(t0)
	p.nProcs = int(scanned)
	p.topMu.Unlock()
	c.setProcCounters(scanned, skipped)
	return out, nil
}

// TopProcs 返回最近一轮的进程快照（副本）与采样时刻。
func (c *Collector) TopProcs() ([]ProcTop, time.Time) {
	p := &c.procs
	p.topMu.Lock()
	defer p.topMu.Unlock()
	return append([]ProcTop(nil), p.top...), p.topTS
}

// procHealth 供 Health() 合并：自身开销与扫描成本。
func (c *Collector) procHealth(h map[string]float64) {
	p := &c.procs
	p.topMu.Lock()
	defer p.topMu.Unlock()
	h["self.cpu_pct"] = p.selfCPU
	h["self.rss_mb"] = float64(p.selfRSS) / (1 << 20)
	h["collector.procs_scan_ms"] = float64(p.scanDur.Microseconds()) / 1000
}

// emitTracked 维护一个"被跟踪名字"集合并为其每轮出点（空闲出 0，序列才连续）。返回被跟踪部分之和。
func emitTracked(tracked map[string]time.Time, byKey map[string]float64, minV float64, prefix string, now time.Time, out *[]Sample) float64 {
	for key, v := range byKey {
		if v >= minV {
			if _, in := tracked[key]; in || len(tracked) < procTrackMax {
				tracked[key] = now
			}
		}
	}
	var sum float64
	for key, last := range tracked {
		if now.Sub(last) > procTrackIdle {
			delete(tracked, key)
			continue
		}
		v := byKey[key]
		sum += v
		*out = append(*out, Sample{MetricID: prefix + key, TS: now, Value: v})
	}
	return sum
}

// pickSnapshot：CPU 前 N ∪ IO 前 N ∪ RSS 增长前 5 ∪ RSS 最大前 5 ∪ D 状态 ∪ 自身，按 CPU 降序。
func pickSnapshot(all []ProcTop) []ProcTop {
	chosen := make(map[int]bool, procSnapMax)
	var out []ProcTop
	add := func(t ProcTop) {
		if !chosen[t.PID] && len(out) < procSnapMax {
			chosen[t.PID] = true
			out = append(out, t)
		}
	}
	take := func(less func(a, b ProcTop) bool, n int, keep func(ProcTop) bool) {
		idx := make([]int, 0, len(all))
		for i := range all {
			if keep(all[i]) {
				idx = append(idx, i)
			}
		}
		sort.Slice(idx, func(i, j int) bool { return less(all[idx[i]], all[idx[j]]) })
		for k := 0; k < len(idx) && k < n; k++ {
			add(all[idx[k]])
		}
	}
	for _, t := range all {
		if t.Self {
			add(t)
		}
	}
	take(func(a, b ProcTop) bool { return a.CPU > b.CPU }, ProcTopN, func(t ProcTop) bool { return t.CPU > 0 })
	take(func(a, b ProcTop) bool { return a.ReadBps+a.WriteBps > b.ReadBps+b.WriteBps }, ProcTopN,
		func(t ProcTop) bool { return t.ReadBps+t.WriteBps > 0 })
	take(func(a, b ProcTop) bool { return a.RSSGrowth > b.RSSGrowth }, 5, func(t ProcTop) bool { return t.RSSGrowth > 16<<20 })
	take(func(a, b ProcTop) bool { return a.RSS > b.RSS }, 5, func(t ProcTop) bool { return t.RSS > 0 })
	take(func(a, b ProcTop) bool { return a.PID < b.PID }, ProcTopN, func(t ProcTop) bool { return t.State == "D" })
	sort.SliceStable(out, func(i, j int) bool { return out[i].CPU > out[j].CPU })
	return out
}

// topByValue 返回值最大的 n 个键（其余丢弃）。用于把 RSS 序列限制在占用最高的几个进程上。
func topByValue(m map[string]float64, n int) map[string]float64 {
	if len(m) <= n {
		return m
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j] // 同值时稳定，避免序列集合每轮抖动
	})
	out := make(map[string]float64, n)
	for _, k := range keys[:n] {
		out[k] = m[k]
	}
	return out
}
