// causal.go — L4 归因：把 L3 的"哪些指标偏了"和 L1 的"谁在用"连成一句带责任方与下一步命令的结论。
//
// 早期 L4 对每个偏离指标各出一条"xxx 偏离 3σ，建议检查 CPU"。那次 nodedata 自己吃掉 1.5 核，
// 是别的程序给出"责任方 nodedata-linux- PID 459521，下一步 top -H -p 459521"——这正是 L4 该说的话。
//
// 规则：
//  1. 偏离按症状归类（CPU / IO / 内存 / 网络），只看"坏方向"（mem.available 往下、其余往上）；
//     一类出一条结论，而不是每个指标一条。
//  2. 责任方排序：自己的序列也偏了的进程（proc.cpu.<名字> / proc.io.<名字> 相对自身历史升高）
//     > 没有历史的进程（新出现、或刚开始用 CPU/IO——失控的新进程正是这种）
//     > 此刻用得最多的进程（明说"未必异常"）。常年 200% 的数据库不该因为一直最大就背锅。
//  3. 虚机上 cpu.steal 领头时，责任方是宿主机，不指认任何本机进程。
//  4. 网络只归因到接口；按进程分流量需要 eBPF，这里明确说不做。
//  5. 责任方是 nodedata 自己时明说。
package diagnosis

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Proc 是 L1 进程快照里的一个进程（与 collector.ProcTop 同构，diagnosis 不依赖 collector）。
type Proc struct {
	PID        int
	Comm, Key  string
	State      string
	CPU        float64 // 占一个核的百分比
	ReadBps    float64
	WriteBps   float64
	MajFlt     float64
	RSS        uint64
	RSSGrowth  int64
	GrowthSpan int
	Self       bool
	// Service 是该进程所属的服务名（MySQL、Nginx…）。空表示没识别出来。
	// 值班的人问的是"MySQL 是不是又卡了"，不是"pid 1200 怎么了"。
	Service string
	Ports   []int
}

// Culprit 是一条结论的责任方。
type Culprit struct {
	Kind    string `json:"kind"` // process | device | interface | host | kernel
	PID     int    `json:"pid,omitempty"`
	Name    string `json:"name"`
	Service string `json:"service,omitempty"` // 所属服务，若识别出来
	Detail  string `json:"detail"`
	Self    bool   `json:"self,omitempty"`
}

const (
	ClassCPU = "CPU"
	ClassIO  = "IO"
	ClassMem = "内存"
	ClassNet = "网络"
)

// classOf 把指标归到症状类；"" 表示不参与归因。
func classOf(id string) string {
	base := id
	if i := strings.IndexByte(id, '@'); i >= 0 {
		base = id[:i]
	}
	switch {
	case strings.HasPrefix(base, "proc.cpu."), base == "cpu.user", base == "cpu.sys", base == "cpu.softirq",
		base == "cpu.steal", strings.HasPrefix(base, "loadavg"), strings.HasPrefix(base, "psi.cpu"),
		base == "procs_running":
		return ClassCPU
	case strings.HasPrefix(base, "proc.io."), strings.HasPrefix(base, "disk."), strings.HasPrefix(base, "psi.io"),
		base == "cpu.iowait", base == "procs_blocked", base == "mem.dirty", base == "mem.writeback":
		return ClassIO
	case strings.HasPrefix(base, "mem."), strings.HasPrefix(base, "swap."), strings.HasPrefix(base, "psi.mem"),
		base == "pgfault", base == "slab":
		return ClassMem
	case strings.HasPrefix(base, "net."), strings.HasPrefix(base, "tcp."), base == "conntrack":
		return ClassNet
	}
	return ""
}

// badDirection：该指标往哪个方向偏才算劣化。+1 往上坏，-1 往下坏。
func badDirection(id string) float64 {
	switch strings.SplitN(id, "@", 2)[0] {
	case "mem.available", "mem.free", "mem.cached":
		return -1
	}
	return 1
}

type trig struct {
	d    Deviation
	peak float64
	lag  string
}

// causal 生成按类归因的结论；返回被消化掉的指标 ID，这些不再出通用条目。
func causal(devs []Deviation, opt Options) ([]Item, map[string]bool) {
	byID := make(map[string]Deviation, len(devs))
	groups := map[string][]trig{}
	for _, d := range devs {
		byID[d.MetricID] = d
		cls := classOf(d.MetricID)
		if cls == "" {
			continue
		}
		peak, lag, ready := peakZ(d.Z)
		if !ready || math.Abs(peak) < opt.ZThreshold || peak*badDirection(d.MetricID) <= 0 {
			continue
		}
		groups[cls] = append(groups[cls], trig{d, peak, lag})
	}
	used := map[string]bool{}
	var items []Item
	for _, cls := range []string{ClassCPU, ClassIO, ClassMem, ClassNet} {
		ts := groups[cls]
		if len(ts) == 0 {
			continue
		}
		sort.Slice(ts, func(i, j int) bool { return math.Abs(ts[i].peak) > math.Abs(ts[j].peak) })
		for _, t := range ts {
			used[t.d.MetricID] = true
		}
		// 进程序列与单设备序列只作证据，不单独领头（整机序列在就用整机的）。
		head := ts[0]
		for _, t := range ts {
			if !strings.HasPrefix(t.d.MetricID, "proc.") && !strings.Contains(t.d.MetricID, "@") {
				head = t
				break
			}
		}
		it := Item{Level: Warning, Category: cls, Class: cls, Timestamp: opt.Now}
		for _, t := range ts {
			if t.d.Breadth >= opt.BreadthCritical {
				it.Level = Critical
			}
		}
		onset := head.d.OnsetLag
		if onset == "" {
			onset = head.lag
		}
		dir := "上升"
		if head.peak < 0 {
			dir = "下降"
		}
		// 起始档位不进标题：σ 会随故障持续而被污染，短档先被掩盖，"起始 L6"会让人误读成三小时前开始。
		it.Title = fmt.Sprintf("%s 劣化：%s %s %.1fσ", cls, head.d.MetricID, dir, math.Abs(head.peak))
		it.Evidence = append(it.Evidence, fmt.Sprintf("领头指标最短显著档位 %s", onset))
		var also []string
		for _, t := range ts {
			if t.d.MetricID != head.d.MetricID && len(also) < 6 {
				also = append(also, fmt.Sprintf("%s %+.1fσ", t.d.MetricID, t.peak))
			}
		}
		it.Description = fmt.Sprintf("当前 %s = %s；十四档中 %d 档超过 |z|≥%.1f。", head.d.MetricID,
			fmtVal(head.d.Value, head.d.Unit), head.d.Breadth, opt.ZThreshold)
		if len(also) > 0 {
			it.Description += "同时偏离：" + strings.Join(also, "、") + "。"
		}
		for _, t := range ts {
			it.Metrics = append(it.Metrics, t.d.MetricID)
			it.Evidence = append(it.Evidence, fmt.Sprintf("L3 %s = %s，peak z %+.2f @ %s，breadth %d",
				t.d.MetricID, fmtVal(t.d.Value, t.d.Unit), t.peak, t.lag, t.d.Breadth))
		}
		switch cls {
		case ClassCPU:
			attributeCPU(&it, ts, byID, opt.Procs, headLags(head, opt.ZThreshold), opt.ZThreshold)
		case ClassIO:
			attributeIO(&it, ts, byID, opt.Procs, headLags(head, opt.ZThreshold), opt.ZThreshold)
		case ClassMem:
			attributeMem(&it, ts, opt.Procs)
		case ClassNet:
			attributeNet(&it, ts)
		}
		var procC, place *Culprit
		for i := range it.Culprits {
			c := &it.Culprits[i]
			if c.PID > 0 && procC == nil {
				procC = c
			}
			if c.PID == 0 && place == nil {
				place = c
			}
		}
		switch {
		case procC != nil && procC.Self:
			it.Title += fmt.Sprintf(" — 责任方 nodedata 自身（PID %d）", procC.PID)
			it.Description += "这是监控程序自身的开销，请把本条连同事故留证反馈给 nodedata 维护方。"
		case procC != nil && procC.Service != "" && procC.Service != procC.Name:
			// 服务名优先：值班的人认的是 MySQL，不是 mysqld
			it.Title += fmt.Sprintf(" — 责任方 %s（%s，PID %d）", procC.Service, procC.Name, procC.PID)
		case procC != nil:
			it.Title += fmt.Sprintf(" — 责任方 %s（PID %d）", procC.Name, procC.PID)
		case place != nil:
			it.Title += " — 责任方 " + place.Name
		}
		if procC != nil && place != nil && (place.Kind == "device" || place.Kind == "interface") {
			it.Title += "，" + map[string]string{"device": "设备 ", "interface": "接口 "}[place.Kind] + place.Name
		}
		it.Impact = impactForDomain(domainForClass(cls), it.Level)
		if len(it.Commands) > 0 {
			it.Action = "下一步：" + strings.Join(it.Commands, "；")
		}
		items = append(items, it)
	}
	return items, used
}

func domainForClass(cls string) string {
	switch cls {
	case ClassCPU:
		return "cpu"
	case ClassIO:
		return "disk"
	case ClassMem:
		return "mem"
	}
	return "net"
}

// procVerdict 在"领头指标偏离的那些档位"上看某进程序列：
//
//	2 = 它自己也向上偏离（z 取这些档位里最大的）；
//	1 = 这些档位上它大多没有历史 —— 它在这次变化开始之后才出现（新进程、或刚开始用这项资源）；
//	0 = 它在变化之前就是这样，未必是原因。
//
// 必须按档位比较：失控的新进程跑了 5 分钟后，自己的 L1 就绪且平稳（z≈0），
// 若只看"有没有就绪档位"，归因会在 5 分钟后翻回到常年最大的那个进程。
func procVerdict(byID map[string]Deviation, id string, headLags []int, thr float64) (tier int, z float64) {
	d, ok := byID[id]
	if !ok || len(headLags) == 0 {
		return 0, 0
	}
	nan := 0
	for _, i := range headLags {
		if i >= len(d.Z) || math.IsNaN(d.Z[i]) {
			nan++
			continue
		}
		if d.Z[i] >= thr && d.Z[i] > z {
			z = d.Z[i]
		}
	}
	switch {
	case z > 0:
		return 2, z
	case nan*2 > len(headLags):
		return 1, 0
	}
	return 0, 0
}

// headLags：领头指标 |z|≥阈值 的档位。
func headLags(head trig, thr float64) []int {
	var out []int
	for i, v := range head.d.Z {
		if !math.IsNaN(v) && math.Abs(v) >= thr {
			out = append(out, i)
		}
	}
	return out
}

type verdict struct {
	p    Proc
	tier int
	z    float64
}

// rankProcs：自身偏离（按 z）> 变化后才出现 > 其余；同级按 metric 降序。
func rankProcs(procs []Proc, byID map[string]Deviation, prefix string, lags []int, thr float64, metric func(Proc) float64) []verdict {
	out := make([]verdict, 0, len(procs))
	for _, p := range procs {
		t, z := procVerdict(byID, prefix+p.Key, lags, thr)
		out = append(out, verdict{p, t, z})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].tier != out[j].tier {
			return out[i].tier > out[j].tier
		}
		if out[i].z != out[j].z {
			return out[i].z > out[j].z
		}
		return metric(out[i].p) > metric(out[j].p)
	})
	return out
}

func has(ts []trig, id string) (trig, bool) {
	for _, t := range ts {
		if t.d.MetricID == id {
			return t, true
		}
	}
	return trig{}, false
}

func procCulprit(p Proc, detail string) Culprit {
	c := Culprit{Kind: "process", PID: p.PID, Name: p.Comm, Service: p.Service, Detail: detail, Self: p.Self}
	if p.Service != "" && len(p.Ports) > 0 {
		c.Detail += fmt.Sprintf("；属于 %s（监听 %s）", p.Service, joinInts(p.Ports))
	} else if p.Service != "" {
		c.Detail += "；属于 " + p.Service
	}
	return c
}

func joinInts(v []int) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ",")
}

func attributeCPU(it *Item, ts []trig, byID map[string]Deviation, procs []Proc, lags []int, thr float64) {
	// 虚机：steal 是 CPU 类里偏离最大的 → 宿主机抢占，不指认本机进程。
	if st, ok := has(ts, "cpu.steal"); ok && math.Abs(st.peak) >= math.Abs(ts[0].peak)-1e-9 {
		it.Culprits = append(it.Culprits, Culprit{Kind: "host", Name: "宿主机（hypervisor 抢占）",
			Detail: fmt.Sprintf("steal %.1f%%（%+.1fσ）：CPU 时间被宿主机拿走，本机进程不是原因；找云厂商或宿主机管理员核实同宿主机负载",
				st.d.Value, st.peak)})
		it.Commands = []string{"vmstat 1 5", "mpstat -P ALL 1 3", "cat /proc/pressure/cpu"}
		return
	}
	busy := byID["cpu.user"].Value + byID["cpu.sys"].Value
	share := func(p Proc) string {
		if busy <= 0 {
			return ""
		}
		return fmt.Sprintf("，占整机忙碌的 %.0f%%", 100*math.Min(p.CPU/busy, 1))
	}
	var picked []Proc
	for _, v := range rankProcs(procs, byID, "proc.cpu.", lags, thr, func(p Proc) float64 { return p.CPU }) {
		p := v.p
		base := fmt.Sprintf("占 %.0f%% 核%s", p.CPU, share(p))
		onlyBiggest := false
		var why string
		switch {
		case v.tier == 2 && p.CPU >= 5:
			why = fmt.Sprintf("%s；该进程 CPU 相对自身历史 %+.1fσ", base, v.z)
		case v.tier == 1 && p.CPU >= 20:
			why = base + "；在这次变化开始之后才出现（新进程或刚开始用 CPU），是首要嫌疑"
		case len(picked) == 0 && p.CPU >= 20:
			why, onlyBiggest = base+"；此刻 CPU 最高，但其自身历史未见异常，未必是变化的原因", true
		default:
			continue
		}
		it.Culprits = append(it.Culprits, procCulprit(p, why))
		picked = append(picked, p)
		if onlyBiggest || len(picked) >= 3 { // 只是"此刻最大"时只列一个，不制造更多嫌疑人
			break
		}
	}
	if len(picked) > 0 {
		pid := picked[0].PID
		it.Commands = []string{fmt.Sprintf("top -H -p %d", pid), fmt.Sprintf("pidstat -u -t -p %d 1 5", pid),
			fmt.Sprintf("cat /proc/%d/status", pid), fmt.Sprintf("perf top -p %d", pid)}
		return
	}
	if _, ok := has(ts, "cpu.softirq"); ok {
		it.Culprits = append(it.Culprits, Culprit{Kind: "kernel", Name: "软中断", Detail: "CPU 花在软中断（网络收包、定时器、块设备完成）上，不归属任何进程"})
		it.Commands = []string{"cat /proc/softirqs", "mpstat -I SCPU -P ALL 1 3", "top -H（看 ksoftirqd/N）"}
		return
	}
	it.Commands = []string{"top -H", "pidstat -u 1 5", "vmstat 1 5"}
}

func attributeIO(it *Item, ts []trig, byID map[string]Deviation, procs []Proc, lags []int, thr float64) {
	// 设备：单设备序列里向上偏离最大的；没有就取此刻 util 最高的盘
	dev, devDetail := "", ""
	for _, t := range ts {
		if i := strings.IndexByte(t.d.MetricID, '@'); i > 0 && strings.HasPrefix(t.d.MetricID, "disk.") {
			dev = t.d.MetricID[i+1:]
			devDetail = fmt.Sprintf("%s = %s（%+.1fσ）", t.d.MetricID[:i], fmtVal(t.d.Value, t.d.Unit), t.peak)
			break
		}
	}
	if dev == "" {
		best := -1.0
		for id, d := range byID {
			if strings.HasPrefix(id, "disk.util@") && d.Value > best {
				best, dev = d.Value, strings.TrimPrefix(id, "disk.util@")
				devDetail = fmt.Sprintf("此刻最忙的盘，util %.0f%%", d.Value)
			}
		}
	}
	if dev != "" {
		if u, ok := byID["disk.util@"+dev]; ok && !strings.HasPrefix(devDetail, "util") {
			devDetail += fmt.Sprintf("；util %.0f%%", u.Value)
		}
		if w, ok := byID["disk.await_w@"+dev]; ok {
			devDetail += fmt.Sprintf("，写延迟 %.1f ms", w.Value)
		}
		it.Culprits = append(it.Culprits, Culprit{Kind: "device", Name: dev, Detail: devDetail})
	}
	// 进程：自身 IO 序列偏离 > 变化后才开始读写 > 此刻读写最多（≥1MiB/s）
	var pid int
	for _, v := range rankProcs(procs, byID, "proc.io.", lags, thr, func(p Proc) float64 { return p.ReadBps + p.WriteBps }) {
		p := v.p
		if p.ReadBps+p.WriteBps < 1<<20 || len(it.Culprits) >= 3 {
			continue
		}
		detail := fmt.Sprintf("读 %s/s，写 %s/s", fmtBytes(p.ReadBps), fmtBytes(p.WriteBps))
		switch v.tier {
		case 2:
			detail += fmt.Sprintf("；该进程 IO 相对自身历史 %+.1fσ", v.z)
		case 1:
			detail += "；在这次变化开始之后才开始读写，是首要嫌疑"
		default:
			detail += "；其自身历史未见异常，未必是变化的原因"
		}
		it.Culprits = append(it.Culprits, procCulprit(p, detail))
		if pid == 0 {
			pid = p.PID
		}
	}
	// D 状态：卡在 IO 上的进程（受害者或元凶都可能，列出来给人判断）
	var dnames []string
	var dpid int
	for _, p := range procs {
		if p.State == "D" && len(dnames) < 5 {
			dnames = append(dnames, fmt.Sprintf("%s(%d)", p.Comm, p.PID))
			if dpid == 0 {
				dpid = p.PID
			}
		}
	}
	if len(dnames) > 0 {
		it.Evidence = append(it.Evidence, "D 状态（不可中断，等 IO）："+strings.Join(dnames, "、"))
	}
	if dev != "" {
		it.Commands = append(it.Commands, fmt.Sprintf("iostat -dx /dev/%s 1 5", dev))
	}
	if pid > 0 {
		it.Commands = append(it.Commands, fmt.Sprintf("pidstat -d -p %d 1 5", pid), fmt.Sprintf("cat /proc/%d/io", pid))
	}
	if dpid > 0 {
		it.Commands = append(it.Commands, fmt.Sprintf("cat /proc/%d/stack", dpid))
	}
	if len(it.Commands) == 0 {
		it.Commands = []string{"iostat -dx 1 5", "pidstat -d 1 5"}
	}
}

func attributeMem(it *Item, ts []trig, procs []Proc) {
	if sl, ok := has(ts, "slab"); ok && math.Abs(sl.peak) >= math.Abs(ts[0].peak)-1e-9 {
		it.Culprits = append(it.Culprits, Culprit{Kind: "kernel", Name: "内核 slab",
			Detail: fmt.Sprintf("slab = %s（%+.1fσ）：内存被内核对象占用（dentry/inode 缓存、网络缓冲等），不属于任何进程", fmtBytes(sl.d.Value), sl.peak)})
		it.Commands = []string{"slabtop -o -s c | head -20", "grep -iE 'slab|sreclaim|sunreclaim' /proc/meminfo"}
		return
	}
	cands := append([]Proc(nil), procs...)
	sort.Slice(cands, func(i, j int) bool { return cands[i].RSSGrowth > cands[j].RSSGrowth })
	var pid int
	for _, p := range cands {
		if p.RSSGrowth < 64<<20 || len(it.Culprits) >= 3 {
			break
		}
		it.Culprits = append(it.Culprits, procCulprit(p, fmt.Sprintf("RSS %s，%d 分钟内增长 %s", fmtBytes(float64(p.RSS)),
			p.GrowthSpan/60, fmtBytes(float64(p.RSSGrowth)))))
		if pid == 0 {
			pid = p.PID
		}
	}
	if pid == 0 {
		sort.Slice(cands, func(i, j int) bool { return cands[i].MajFlt > cands[j].MajFlt })
		if len(cands) > 0 && cands[0].MajFlt >= 10 {
			p := cands[0]
			it.Culprits = append(it.Culprits, procCulprit(p, fmt.Sprintf("主缺页 %.0f/s：它的页正被换出/回收后再读回，是内存压力的主要受害者", p.MajFlt)))
			pid = p.PID
		}
	}
	if pid == 0 {
		sort.Slice(cands, func(i, j int) bool { return cands[i].RSS > cands[j].RSS })
		if len(cands) > 0 {
			p := cands[0]
			it.Culprits = append(it.Culprits, procCulprit(p, fmt.Sprintf("RSS 最大（%s），但近 1 小时未见明显增长，未必是变化的原因", fmtBytes(float64(p.RSS)))))
			pid = p.PID
		}
	}
	if pid > 0 {
		it.Commands = []string{fmt.Sprintf("grep -E 'VmRSS|RssAnon|VmSwap' /proc/%d/status", pid),
			fmt.Sprintf("cat /proc/%d/smaps_rollup", pid), fmt.Sprintf("pmap -x %d | sort -k3 -n | tail -5", pid)}
	} else {
		it.Commands = []string{"free -m", "vmstat 1 5", "cat /proc/meminfo"}
	}
}

func attributeNet(it *Item, ts []trig) {
	var ifc string
	for _, t := range ts {
		if i := strings.IndexByte(t.d.MetricID, '@'); i > 0 {
			ifc = t.d.MetricID[i+1:]
			it.Culprits = append(it.Culprits, Culprit{Kind: "interface", Name: ifc,
				Detail: fmt.Sprintf("%s = %s（%+.1fσ）", t.d.MetricID[:i], fmtVal(t.d.Value, t.d.Unit), t.peak)})
			break
		}
	}
	it.Evidence = append(it.Evidence, "按进程拆分网络流量需要 eBPF，当前版本只归因到接口")
	if ifc != "" {
		it.Commands = append(it.Commands, "ip -s link show "+ifc, "ethtool -S "+ifc+" | grep -iE 'drop|err|miss|fifo'")
	}
	it.Commands = append(it.Commands, "nstat -az | grep -iE 'retrans|drop|overflow'", "ss -s")
	if _, ok := has(ts, "conntrack"); ok {
		it.Commands = append(it.Commands, "conntrack -S", "sysctl net.netfilter.nf_conntrack_max")
	}
}

func fmtBytes(v float64) string {
	u := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for math.Abs(v) >= 1024 && i < len(u)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f B", v)
	}
	return fmt.Sprintf("%.1f %s", v, u[i])
}

func fmtVal(v float64, unit string) string {
	switch unit {
	case "bytes":
		return fmtBytes(v)
	case "bytes/s":
		return fmtBytes(v) + "/s"
	case "percent":
		return fmt.Sprintf("%.1f%%", v)
	case "ms":
		return fmt.Sprintf("%.1f ms", v)
	case "/s":
		return fmt.Sprintf("%.1f/s", v)
	}
	return fmt.Sprintf("%.3g", v)
}
