// report_data.go — 报告 v2 需要的"背景"：机器、学习进度、先后顺序、最近变化。
//
// 这些都是给一个没有上下文的读者（人或模型）补的前提：
//   - 机器：没有它，"运行队列 19"会被判成严重，而这是 16 核的机器；
//   - 先后：谁先偏离、谁后跟上，是推因果最强的线索——我们不替读者推，只把顺序摆出来；
//   - 最近变化：大多数故障紧跟在一次变化之后。写"没有变化"跟写"有变化"一样重要，
//     它把发版、重启、配置变更排除掉了。
package main

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
	"github.com/githubflyideas/nodedata/internal/deviation"
	"github.com/githubflyideas/nodedata/internal/diagnosis"
	"github.com/githubflyideas/nodedata/internal/metrics"
)

// reportWindow 是"最近变化"回看的时长：72 小时刚好盖住一个周末。
const reportWindow = 72 * time.Hour

type machineInfo struct {
	Cores    int
	MemTotal uint64
	Kernel   string
	Virt     string
	Disks    []string // "vda(虚拟盘)"
	Uptime   time.Duration
}

type learnInfo struct {
	Span     time.Duration // 历史覆盖多久
	Usable   []string      // 已经能比的档位："1天"
	Next     string        // 下一个还不能比的档位
	NextWait time.Duration // 还要等多久
}

type tlEvent struct {
	At       time.Time
	What     string // "java(24117) CPU"
	From, To string
	First    bool
}

type changeLine struct {
	At, End time.Time
	Text    string
}

type reportData struct {
	Host     string
	At       time.Time
	Machine  *machineInfo
	Learn    *learnInfo
	Rows     []UseRow
	Timeline []tlEvent
	// 最近变化
	HaveChanges bool // false = 拿不到服务台账等来源，整段不写
	Services    []changeLine
	SvcCount    int
	NewProcs    []changeLine
	Kernel      []changeLine
	KernelErr   string // 读不到内核日志的原因；空 = 读到了
	Episodes    []changeLine
	Chain       *diagnosis.Chain
}

// gatherReport 收集报告需要的一切。每个来源缺了就少一段，不报错。
func (d *Diagnoser) gatherReport(host string, now time.Time) reportData {
	rd := reportData{Host: host, At: now, Rows: d.Use(now), Chain: d.Run(3.0)}
	rd.Machine = d.machine()
	if d.builder != nil {
		rd.Learn = d.learning(now)
		rd.Timeline = d.timeline(rd.Rows, now)
	}
	if d.svcLog != nil {
		rd.HaveChanges = true
		for _, e := range d.svcLog.ChangesSince(now.Add(-reportWindow)) {
			verb := map[string]string{evAppear: "出现", evVanish: "消失", evRestart: "重启"}[e.Kind]
			if e.DuringDowntime {
				verb = "出现（nodedata 停机期间）"
			}
			rd.Services = append(rd.Services, changeLine{At: time.Unix(e.TS, 0), Text: "服务 " + e.ID + " " + verb})
		}
		if len(rd.Services) > 8 { // 一次部署动了一堆服务时，只留最近的 8 条
			rd.Services = rd.Services[len(rd.Services)-8:]
		}
		rd.SvcCount = len(d.svcLog.Current())
	}
	if d.procs != nil {
		rd.NewProcs = newProcesses(d.procs(), now)
	}
	if d.kernel != nil {
		lines, err := d.kernel()
		if err != nil {
			rd.KernelErr = err.Error()
		}
		rd.Kernel = kernelEvents(lines, d.bootTime(), now)
	} else {
		rd.KernelErr = "留证模块未启用"
	}
	if d.series != nil {
		rd.Episodes = d.episodes(now)
	}
	return rd
}

// machine 读机器背景。都是启动后基本不变的东西，读几个小文件。
func (d *Diagnoser) machine() *machineInfo {
	m := &machineInfo{Cores: runtime.NumCPU()}
	if d.cores > 0 {
		m.Cores = d.cores
	}
	root := d.procRoot()
	if b, err := os.ReadFile(root + "/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				f := strings.Fields(line)
				if len(f) >= 2 {
					kb, _ := strconv.ParseUint(f[1], 10, 64)
					m.MemTotal = kb << 10
				}
				break
			}
		}
	}
	if b, err := os.ReadFile(root + "/sys/kernel/osrelease"); err == nil {
		m.Kernel = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(root + "/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			sec, _ := strconv.ParseFloat(f[0], 64)
			m.Uptime = time.Duration(sec) * time.Second
		}
	}
	if d.virt != nil {
		m.Virt = d.virt()
	}
	if d.disks != nil {
		kinds := d.disks().Kinds
		names := make([]string, 0, len(kinds))
		for n := range kinds {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			m.Disks = append(m.Disks, n+"("+collector.DiskKindName(kinds[n])+")")
		}
	}
	return m
}

func (d *Diagnoser) procRoot() string {
	if d.procRootPath != "" {
		return d.procRootPath
	}
	return "/proc"
}

// bootTime 读 /proc/stat 的 btime。内核日志的时间戳是"开机后多少微秒"，要它换成墙钟。
func (d *Diagnoser) bootTime() int64 {
	b, err := os.ReadFile(d.procRoot() + "/stat")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "btime ") {
			v, _ := strconv.ParseInt(strings.TrimSpace(line[6:]), 10, 64)
			return v
		}
	}
	return 0
}

// learning：历史覆盖多久、哪些档位已经能比、下一个还要等多久。
// 刚装上的几天，"跟上周比"天然是空的——要说出来，不然读者以为是坏了。
func (d *Diagnoser) learning(now time.Time) *learnInfo {
	li := &learnInfo{}
	if old, ok := d.series.oldest(); ok {
		li.Span = now.Sub(old)
	}
	diag := d.builder.DiagnoseLags(now)
	for i, x := range diag {
		if x.ZAvailable > 0 {
			li.Usable = append(li.Usable, deviation.LagName(i))
			continue
		}
		if li.Next == "" && i >= 4 { // 短档没数多半是刚重启，几分钟就好，不值得写进"学习进度"
			li.Next = deviation.LagName(i)
			need := 2 * time.Duration(deviation.LagSeconds[i]) * time.Second
			if w := need - li.Span; w > 0 {
				li.NextWait = w
			}
		}
	}
	return li
}

// timeline：本次异常里各指标"从什么时候开始"的先后。
//
// 取每个异常指标在最近一小时里、一直延续到现在的那段 |z|≥3 的起点。允许中间断一个点
// （噪声会让单点掉回去），不允许断两个。起点在窗口最开头的，说明一小时前就已经在了。
func (d *Diagnoser) timeline(rows []UseRow, now time.Time) []tlEvent {
	type cand struct{ id, label string }
	var cands []cand
	seen := map[string]bool{}
	for _, r := range rows {
		if r.Level <= 0 {
			continue
		}
		for _, f := range r.Facts {
			if math.Abs(f.Z) >= useZThreshold && !seen[f.ID] {
				seen[f.ID] = true
				cands = append(cands, cand{f.ID, f.Label})
			}
		}
		for _, m := range r.Movers {
			if seen[m.Metric] {
				continue
			}
			seen[m.Metric] = true
			what := m.Name
			if m.PID > 0 {
				what += "(" + strconv.Itoa(m.PID) + ")"
			}
			kind := map[string]string{"proc.cpu.": " CPU", "proc.io.": " 读写", "proc.rss.": " 内存"}
			for pre, k := range kind {
				if strings.HasPrefix(m.Metric, pre) {
					what += k
				}
			}
			cands = append(cands, cand{m.Metric, what})
		}
	}
	if len(cands) == 0 {
		return nil
	}
	var out []tlEvent
	for _, c := range cands {
		if at, from, to, ok := d.onset(c.id, now); ok {
			out = append(out, tlEvent{At: at, What: c.label, From: from, To: to})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	if len(out) > 8 {
		out = out[:8]
	}
	if len(out) > 0 {
		out[0].First = true
	}
	return out
}

// onset 找某指标这一轮异常"从什么时候开始"，按幅度找，不按 z。
//
// 起点 = 一直延续到现在、离"平时"至少有现在这一跳一半远的那一段的第一个点（允许中间掉回去一个点）。
// v5.21 开发中先用过"|z|≥3 的那一段"：演示机上异常前就有零星的 |z|≥3，
// 被"允许断一个点"串成一段，报出来的起点比真正开始早了两分多钟——比进程启动还早。
// 幅度才是"从 X 变到 Y"这句话本来的意思。
func (d *Diagnoser) onset(id string, now time.Time) (at time.Time, from, to string, ok bool) {
	pts := d.series.RawRange(id, now.Add(-time.Hour), now)
	if len(pts) < 3 {
		return
	}
	cur := pts[len(pts)-1].V
	base := pts[0].V // 新序列（刚出现的进程）没有"平时"，用窗口开头
	if b, _ := d.baselineN(id, now); b != nil {
		base = *b
	}
	jump := cur - base
	in := metrics.Lookup(id)
	if math.Abs(jump) < in.MinDelta || jump == 0 {
		return // 没有像样的一跳：说不上"从什么时候开始"
	}
	inState := func(v float64) bool { return (v-base)/jump >= 0.5 }
	start := -1
	for i := len(pts) - 1; i >= 0; i-- {
		if inState(pts[i].V) {
			start = i
			continue
		}
		if i > 0 && inState(pts[i-1].V) { // 掉回去一个点：跳过
			continue
		}
		break
	}
	if start < 0 {
		return
	}
	from = "—"
	if start > 0 {
		from = fmtUnit(pts[start-1].V, in.Unit)
	}
	return pts[start].TS, from, fmtUnit(cur, in.Unit), true
}

// newProcesses：最近 72 小时启动、而且现在占着资源的进程。
// 只看进程快照里的（CPU/读写/内存靠前的），不是全部新进程——那会是几千个 cron 子进程。
func newProcesses(ps []diagnosis.Proc, now time.Time) []changeLine {
	var out []changeLine
	for _, p := range ps {
		if p.Self || p.StartTS == 0 || now.Unix()-p.StartTS > int64(reportWindow/time.Second) {
			continue
		}
		if p.CPU < 5 && p.ReadBps+p.WriteBps < 1<<20 && p.RSS < 128<<20 {
			continue
		}
		t := fmt.Sprintf("新进程 %s(%d) 启动", p.Comm, p.PID)
		if p.Parent != "" {
			t += "，父进程 " + p.Parent
		}
		out = append(out, changeLine{At: time.Unix(p.StartTS, 0), Text: t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	if len(out) > 5 {
		out = out[len(out)-5:]
	}
	return out
}

// kernelNotable 是值得写进报告的内核消息。宁可少，不要把 audit、USB 插拔刷进来。
var kernelNotable = []string{
	"Out of memory", "oom-kill", "Killed process", // OOM
	"I/O error", "blk_update_request", "Buffer I/O error", "EXT4-fs error", "XFS (", "Remounting filesystem read-only",
	"Link is Down", "Link is Up", "NETDEV WATCHDOG", "tx timeout", "Tx Unit Hang", // 网卡
	"blocked for more than", "soft lockup", "hard LOCKUP", "rcu_sched self-detected stall", // 卡死
	"Machine check", "Hardware Error", "EDAC", // 硬件
	"segfault at",
}

func isKernelNotable(line string) bool {
	for _, k := range kernelNotable {
		if strings.Contains(line, k) {
			return true
		}
	}
	return false
}

// kernelEvents 把 "[秒.微秒] 消息" 换成墙钟时刻，只留最近 72 小时的。
func kernelEvents(lines []string, boot int64, now time.Time) []changeLine {
	var out []changeLine
	for _, l := range lines {
		if !isKernelNotable(l) {
			continue
		}
		at := now
		if boot > 0 && strings.HasPrefix(l, "[") {
			if i := strings.IndexByte(l, ']'); i > 1 {
				if sec, err := strconv.ParseFloat(l[1:i], 64); err == nil {
					at = time.Unix(boot+int64(sec), 0)
				}
				l = strings.TrimSpace(l[i+1:])
			}
		}
		if now.Sub(at) > reportWindow {
			continue
		}
		if len(l) > 140 {
			l = l[:140] + "…"
		}
		out = append(out, changeLine{At: at, Text: l})
	}
	if len(out) > 5 {
		out = out[len(out)-5:]
	}
	return out
}

// episodeMetrics 是"过去的事"要扫的指标：每类资源最能说明"出过事"的几个。
var episodeMetrics = []string{
	"cpu.busy_pct", "cpu.user", "cpu.core_top1", "psi.cpu.some10",
	"mem.available", "swap.out", "psi.mem.some10",
	"disk.await_w", "disk.wbytes", "disk.rbytes", "psi.io.some10", "procs_blocked",
	"net.rx_drop", "net.tx_drop", "tcp.retrans",
}

// episodes 在最近 72 小时（不含最近 15 分钟，那是"现在"）的长期层里找"出过的事"。
//
// 每个 5 分钟桶取最坏值（抽样点和稀疏峰值里更坏的那个），跟这 72 小时的中位数比；
// 偏离超过 max(2 × 最小变化量, 6 × 稳健尺度) 才算——门槛刻意定高：这一段最容易产生噪声，
// 列一堆"出过事"比不列更糟。相邻的桶连成一段，时间上重叠的不同指标并成一行，最多 5 行。
func (d *Diagnoser) episodes(now time.Time) []changeLine {
	from, to := now.Add(-reportWindow), now.Add(-15*time.Minute)
	type span struct {
		start, end int64
		parts      map[string]string // 标签 → "峰值 42ms(平时 3ms)"
		score      float64
	}
	var spans []span
	for _, id := range episodeMetrics {
		pts := d.series.worstBuckets(id, from, to)
		if len(pts) < 24 { // 不到 2 小时的历史，谈不上"平时"
			continue
		}
		vals := make([]float64, len(pts))
		for i, p := range pts {
			vals[i] = p.sample
		}
		med, scale := medianMAD(vals)
		in := metrics.Lookup(id)
		thr := math.Max(2*in.MinDelta, 6*scale)
		if thr <= 0 {
			continue
		}
		label := episodeLabel(id)
		var cur *span
		var peak float64
		flush := func() {
			if cur != nil {
				cur.parts[label] = "峰值 " + fmtUnit(peak, in.Unit) + "(平时 " + fmtUnit(med, in.Unit) + ")"
				spans = append(spans, *cur)
				cur = nil
			}
		}
		for _, p := range pts {
			dev := (p.worst - med) * in.Direction()
			if dev >= thr {
				if cur == nil || p.t-cur.end > int64(2*coarseStep/time.Second) {
					flush()
					cur = &span{start: p.t, end: p.t, parts: map[string]string{}}
					peak = p.worst
				}
				cur.end = p.t
				if dev/thr > cur.score {
					cur.score = dev / thr
				}
				if (p.worst-peak)*in.Direction() > 0 {
					peak = p.worst
				}
			}
		}
		flush()
	}
	// 时间上重叠的并成一件事
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var merged []span
	for _, s := range spans {
		if n := len(merged); n > 0 && s.start <= merged[n-1].end+int64(coarseStep/time.Second) {
			m := &merged[n-1]
			if s.end > m.end {
				m.end = s.end
			}
			for k, v := range s.parts {
				m.parts[k] = v
			}
			if s.score > m.score {
				m.score = s.score
			}
			continue
		}
		merged = append(merged, s)
	}
	// 最严重的 5 件，再按时间排
	sort.Slice(merged, func(i, j int) bool { return merged[i].score > merged[j].score })
	if len(merged) > 5 {
		merged = merged[:5]
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].start < merged[j].start })
	var out []changeLine
	for _, m := range merged {
		keys := make([]string, 0, len(m.parts))
		for k := range m.parts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var ps []string
		for _, k := range keys {
			ps = append(ps, k+" "+m.parts[k])
		}
		txt := strings.Join(ps, "，")
		if who := d.episodeCulprit(m.start, m.end+int64(coarseStep/time.Second)); who != "" {
			txt += "；当时 " + who
		}
		out = append(out, changeLine{At: time.Unix(m.start, 0), End: time.Unix(m.end, 0).Add(coarseStep), Text: txt})
	}
	return out
}

// episodeCulprit：那段时间里 CPU 或读写最高的进程（只有名字，没有 PID——PID 不存历史）。
func (d *Diagnoser) episodeCulprit(fromU, toU int64) string {
	best, bestV := "", 0.0
	from, to := time.Unix(fromU, 0), time.Unix(toU, 0)
	for _, id := range d.series.MetricIDs() {
		var floor float64
		switch {
		case strings.HasPrefix(id, "proc.io.") && !strings.HasSuffix(id, "__others__"):
			floor = 10 << 20 // 10MB/s 以上才值得提
		case strings.HasPrefix(id, "proc.cpu.") && !strings.HasSuffix(id, "__others__"):
			floor = 50 // 半个核以上
		default:
			continue
		}
		w, ok := d.series.Worst(id, from, to)
		if !ok || w < floor {
			continue
		}
		// 不同单位没法直接比：按超过门槛的倍数排
		if r := w / floor; r > bestV {
			bestV = r
			name := strings.SplitN(id, ".", 3)[2] // proc.io.rsync → rsync
			kind := "CPU"
			if strings.HasPrefix(id, "proc.io.") {
				kind = "读写"
			}
			best = name + " " + kind + " 最高 " + fmtUnit(w, metrics.Lookup(id).Unit)
		}
	}
	return best
}

func episodeLabel(id string) string {
	for _, r := range useResources {
		for _, m := range r.Metrics {
			if m.id == id {
				return m.label
			}
		}
	}
	switch id {
	case "disk.wbytes":
		return "磁盘写"
	case "disk.rbytes":
		return "磁盘读"
	}
	return id
}

func medianMAD(v []float64) (med, scale float64) {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	med = s[len(s)/2]
	dev := make([]float64, len(s))
	for i, x := range s {
		dev[i] = math.Abs(x - med)
	}
	sort.Float64s(dev)
	return med, 1.4826 * dev[len(dev)/2]
}
