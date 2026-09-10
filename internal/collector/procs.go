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
)

// ProcTop 是某一时刻单个进程的 CPU 占用。
type ProcTop struct {
	PID  int     `json:"pid"`
	Comm string  `json:"comm"` // /proc/PID/stat 里的原始 comm（内核截断到 15 字节）
	Key  string  `json:"key"`  // 归一化后的名字，对应 proc.cpu.<key> 序列
	CPU  float64 `json:"cpu"`  // 占一个核的百分比，与 cpu.user 等整机指标同单位
	RSS  uint64  `json:"rss"`  // 字节
	Self bool    `json:"self,omitempty"`
}

type procPrev struct {
	cpu   uint64 // utime+stime，jiffies
	start uint64 // starttime，用来识别 PID 复用
	gen   uint32
}

type procState struct {
	prev    map[int]procPrev
	gen     uint32
	prevTS  time.Time
	tracked map[string]time.Time // 名字 → 最近一次活跃时间
	selfPID int

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
func (c *Collector) parseProcStat(data []byte) (comm string, cpu, start, rss uint64, ok bool) {
	l := bytes.IndexByte(data, '(')
	r := bytes.LastIndexByte(data, ')')
	if l < 0 || r <= l || r+2 > len(data) {
		return "", 0, 0, 0, false
	}
	comm = string(data[l+1 : r])
	f := c.splitFieldsBuf(data[r+2:]) // f[0] 是第 3 个字段 state
	if len(f) < 22 {
		return "", 0, 0, 0, false
	}
	ut, err1 := parseUint64(f[11])    // 14 utime
	st, err2 := parseUint64(f[12])    // 15 stime
	start, err3 := parseUint64(f[19]) // 22 starttime
	if err1 != nil || err2 != nil || err3 != nil {
		return "", 0, 0, 0, false
	}
	pages, _ := parseUint64(f[21]) // 24 rss（页）
	return comm, ut + st, start, pages * uint64(os.Getpagesize()), true
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

	var (
		all      []ProcTop
		byKey    = make(map[string]float64, 64)
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
		comm, cpu, start, rss, ok := c.parseProcStat(data)
		if !ok {
			skipped++
			continue
		}
		prev, seen := p.prev[pid]
		p.prev[pid] = procPrev{cpu: cpu, start: start, gen: p.gen}
		// 首次见到、PID 被复用、计数回退：只记基线，不出增量。
		if !hasPrev || !seen || prev.start != start || cpu < prev.cpu {
			continue
		}
		pct := float64(cpu-prev.cpu) / dt // jiffies/s；USER_HZ=100 ⇒ 占一个核的百分比
		key := ProcKey(comm)
		total += pct
		byKey[key] += pct
		isSelf := pid == p.selfPID
		if isSelf {
			selfCPU, selfRSS = pct, rss
		}
		if pct > 0 || isSelf {
			all = append(all, ProcTop{PID: pid, Comm: comm, Key: key, CPU: pct, RSS: rss, Self: isSelf})
		}
	}
	// 回收已退出进程的基线，否则 map 随 PID 周转无限增长。
	for pid, pp := range p.prev {
		if pp.gen != p.gen {
			delete(p.prev, pid)
		}
	}

	// 快照：CPU 前 N，自身永远在列（sidecar 必须能看见自己的开销）。
	sort.Slice(all, func(i, j int) bool { return all[i].CPU > all[j].CPU })
	top := all
	if len(top) > ProcTopN {
		top = append([]ProcTop(nil), all[:ProcTopN]...)
		hasSelf := false
		for _, x := range top {
			hasSelf = hasSelf || x.Self
		}
		if !hasSelf {
			for _, x := range all[ProcTopN:] {
				if x.Self {
					top = append(top, x)
					break
				}
			}
		}
	}

	var out []Sample
	if hasPrev {
		// 跟踪集合：够活跃的名字进来，空闲一小时的出去。
		for key, v := range byKey {
			if v >= procTrackMinCPU {
				if _, in := p.tracked[key]; in || len(p.tracked) < procTrackMax {
					p.tracked[key] = now
				}
			}
		}
		var sumTracked float64
		for key, last := range p.tracked {
			if now.Sub(last) > procTrackIdle {
				delete(p.tracked, key)
				continue
			}
			v := byKey[key] // 不在本轮 = 0，照样出点，序列才连续
			sumTracked += v
			out = append(out, Sample{MetricID: "proc.cpu." + key, TS: now, Value: v})
		}
		other := total - sumTracked
		if other < 0 {
			other = 0
		}
		out = append(out, Sample{MetricID: "proc.cpu.__others__", TS: now, Value: other})
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
