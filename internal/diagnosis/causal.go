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

	"github.com/githubflyideas/nodedata/internal/deviation"
	"github.com/githubflyideas/nodedata/internal/metrics"
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
	// cgroup CPU 配额限流：被限流的进程是受害者，不是元凶
	ThrottledFrac  float64
	ThrottledRatio float64
	ThrottledPerS  float64
	CGroup         string
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
	ClassCPU = metrics.ClassCPU
	ClassIO  = metrics.ClassIO
	ClassMem = metrics.ClassMem
	ClassNet = metrics.ClassNet
)

// classOf 返回指标的诊断归类（登记在 internal/metrics）。
func classOf(id string) string { return metrics.Lookup(id).Class }

// badDirection：+1 升高是坏事，-1 降低是坏事（登记在 internal/metrics）。
func badDirection(id string) float64 { return metrics.Lookup(id).Direction() }

// absGate 是"绝对值闸门"：指标变了不等于出问题，还得确实变坏了。
//
// z 只回答"和平时比变了没有"，不回答"变得好不好"。一台还剩 15GB 内存的机器，
// 可用内存掉 6σ 也只是缓存在动；一块 util 3% 的盘，延迟涨 6σ 仍然是空闲的。
// 实测截图里满屏的 Critical 大多是这种：现象为真，问题不存在。
//
// 返回 false 表示"绝对水位还好，不出结论"。查不到配套指标时返回 true（宁可报，不漏）。
func absGate(id string, byID map[string]Deviation) bool {
	v := func(k string) (float64, bool) {
		d, ok := byID[k]
		return d.Value, ok
	}
	switch strings.SplitN(id, "@", 2)[0] {
	case "mem.available", "mem.free", "swap.used":
		if used, ok := v("mem.used_pct"); ok {
			return used >= 80 // 还剩两成以上内存，涨跌都不值一提
		}
	case "disk.await_w", "disk.await_r", "disk.riops", "disk.wiops":
		if u, ok := v("disk.util"); ok {
			return u >= 30 // 盘本身不忙时，延迟的相对变化没有意义
		}
	case "fs.avail":
		if used, ok := v("fs.used_pct"); ok {
			return used >= 75
		}
	case "cpu.user", "cpu.sys", "cpu.iowait":
		busy := 0.0
		for _, k := range []string{"cpu.user", "cpu.sys", "cpu.iowait", "cpu.softirq"} {
			if x, ok := v(k); ok {
				busy += x
			}
		}
		if busy > 0 {
			return busy >= 50
		}
	}
	return true
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
		// 进程序列、单设备序列、以及"变化本身不代表坏事"的指标都只作证据，不领头。
		head, found := trig{}, false
		for _, t := range ts {
			if strings.HasPrefix(t.d.MetricID, "proc.") || strings.Contains(t.d.MetricID, "@") ||
				isEvidenceOnly(t.d.MetricID) {
				continue
			}
			// 绝对水位还好就不领头。但有一个例外必须放行：进程正被 cgroup 配额限流。
			// 被限流的进程 CPU 天然上不去（配额就那么点），整机忙碌度也不高，
			// 闸门会把它一起挡掉——而"为什么这个服务慢"恰恰就是配额造成的。
			if !absGate(t.d.MetricID, byID) && !anyThrottled(opt.Procs) {
				continue
			}
			head, found = t, true
			break
		}
		if !found {
			// 整类都只是"吞吐变了""缓存被回收了"这种——不出结论，避免把正常行为说成故障
			continue
		}
		// 同一族的指标说的是同一件事，领头要固定一个代表，否则标题会在它们之间来回跳。
		// 实机注入时就是这样：内存那次的领头在 mem.available → mem.free → mem.used_pct
		// 之间轮换，看起来像三件事，其实是一件。
		if canon, ok := familyHead(ts, head); ok {
			head = canon
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
		it.Evidence = append(it.Evidence, fmt.Sprintf("领头指标最早在和 %s前比时就显著偏离", deviation.LagNameByID(onset)))
		var also []string
		seenFam := map[string]bool{familyOf(head.d.MetricID): true}
		for _, t := range ts {
			if t.d.MetricID == head.d.MetricID || len(also) >= 6 {
				continue
			}
			// 同族的其余成员不再罗列：它们是同一件事的不同说法
			if f := familyOf(t.d.MetricID); f != "" {
				if seenFam[f] {
					continue
				}
				seenFam[f] = true
			}
			also = append(also, fmt.Sprintf("%s %+.1fσ", t.d.MetricID, t.peak))
		}
		it.Description = fmt.Sprintf("当前 %s = %s；十个尺度中有 %d 个超过 |z|≥%.1f。", head.d.MetricID,
			fmtVal(head.d.Value, head.d.Unit), head.d.Breadth, opt.ZThreshold)
		if len(also) > 0 {
			it.Description += "同时偏离：" + strings.Join(also, "、") + "。"
		}
		for _, t := range ts {
			it.Metrics = append(it.Metrics, t.d.MetricID)
			it.Evidence = append(it.Evidence, fmt.Sprintf("%s = %s，峰值 z %+.2f（和 %s前比），%d 个时间尺度上异常",
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
		case procC != nil && procC.Kind == "cgroup":
			it.Title += fmt.Sprintf(" — %s（PID %d）正被 cgroup 配额限流，是受害者", procC.Name, procC.PID)
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
	// 先看有没有进程正被 cgroup 配额限流。被限流的进程 CPU 看着高，但它是受害者：
	// 真正的原因是配额。实机验证过——把进程关进 0.2 核的 cgroup，旧逻辑报"责任方 该进程"，
	// 照这条去 kill 进程，方向完全反了。
	for _, p := range procs {
		if p.ThrottledFrac < 0.2 || p.CPU < 5 {
			continue
		}
		it.Culprits = append(it.Culprits, Culprit{Kind: "cgroup", PID: p.PID, Name: p.Comm, Service: p.Service,
			Detail: fmt.Sprintf("正被 cgroup 配额限流：%.0f%% 的调度周期被掐断（每秒 %.0f 次），"+
				"被限流掉的时间占 %.0f%%。它是受害者不是元凶——CPU 高是配额造成的，"+
				"杀进程解决不了问题，要改配额 %s",
				p.ThrottledFrac*100, p.ThrottledPerS, p.ThrottledRatio*100, p.CGroup)})
		it.Commands = []string{
			fmt.Sprintf("cat /sys/fs/cgroup/cpu%s/cpu.stat", p.CGroup),
			fmt.Sprintf("cat /sys/fs/cgroup/cpu%s/cpu.cfs_quota_us", p.CGroup),
			fmt.Sprintf("systemctl show -p CPUQuota $(systemctl status %d | head -1 | awk '{print $2}')", p.PID),
		}
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

// familyCanon 是每个族的固定代表。
var familyCanon = map[string]string{
	"内存余量": "mem.available", "盘延迟": "disk.await_w", "网卡吞吐": "net.rx",
	"丢包": "net.rx_drop", "根分区": "fs.used_pct", "CPU 压力": "psi.cpu.some10",
	"单核": "cpu.core_top1",
}

// familyOf：同族指标一次只报一条（登记在 internal/metrics）。
func familyOf(id string) string { return metrics.Lookup(id).Family }

// familyHead：若领头指标属于某个族，且该族的代表也在偏离列表里，就改用代表领头。
func familyHead(ts []trig, head trig) (trig, bool) {
	fam := familyOf(head.d.MetricID)
	if fam == "" {
		return head, false
	}
	canon := familyCanon[fam]
	for _, t := range ts {
		if t.d.MetricID == canon {
			return t, true
		}
	}
	return head, false
}

// isEvidenceOnly：只作旁证、不单独成条（登记在 internal/metrics）。
func isEvidenceOnly(id string) bool { return metrics.Lookup(id).EvidenceOnly }

// anyThrottled 判断快照里是否有进程正被 cgroup 配额明显限流。
func anyThrottled(procs []Proc) bool {
	for _, p := range procs {
		if p.ThrottledFrac >= 0.2 {
			return true
		}
	}
	return false
}
