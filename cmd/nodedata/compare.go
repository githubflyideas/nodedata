// compare.go — 关键指标对比：人话名称，当前值与 1h/6h/12h/1d/3d/7d 前的值并排，附变化百分比。
//
// 首屏不再要求读者先懂 σ：先回答"现在和平时比变了什么"，z 值热力图与诊断链在下面给出"变化是否异常、谁干的"。
// 历史值用 Series.Lookup 精确取：24 小时内来自原始层，更早来自 5 分钟长期层。
// 没有历史时返回 null（页面显示"—"），绝不用 0 冒充。
package main

import (
	"math"
	"runtime"
	"sort"
	"strings"
	"time"
)

var compareCols = []struct {
	Name string
	Ago  time.Duration
}{
	{"1h", time.Hour}, {"6h", 6 * time.Hour}, {"12h", 12 * time.Hour},
	{"1d", 24 * time.Hour}, {"3d", 72 * time.Hour}, {"7d", 7 * 24 * time.Hour},
}

type cmpRow struct {
	Label string     `json:"label"`
	Core  bool       `json:"core"` // 默认显示；其余收在"更多指标"里
	Unit  string     `json:"unit"`
	Now   *float64   `json:"now"`
	Past  []*float64 `json:"past"`
	Bad   int        `json:"bad"` // +1 往上是坏，-1 往下是坏，0 无所谓好坏
	ID    string     `json:"id"`
}

type cmpGroup struct {
	Name string   `json:"name"`
	Rows []cmpRow `json:"rows"`
}

type CompareJSON struct {
	At     int64      `json:"at"`
	Cols   []string   `json:"cols"`
	Groups []cmpGroup `json:"groups"`
}

// cmpDef：一行的定义。ids 多于一个时按 f 合成（如 CPU 忙碌 = user+sys+softirq，按核数归一）。
type cmpDef struct {
	label, unit string
	bad         int
	core        bool
	ids         []string
	f           func(v []float64) float64
}

// one 是"更多指标"里的一行；core 是默认显示的关键行。
func one(label, unit string, bad int, id string) cmpDef {
	return cmpDef{label: label, unit: unit, bad: bad, ids: []string{id}}
}

func core(label, unit string, bad int, id string) cmpDef {
	return cmpDef{label: label, unit: unit, bad: bad, core: true, ids: []string{id}}
}

func perCPU(label string, isCore bool, ids ...string) cmpDef {
	n := float64(runtime.NumCPU())
	return cmpDef{label: label, unit: "percent", bad: 1, core: isCore, ids: ids, f: func(v []float64) float64 {
		s := 0.0
		for _, x := range v {
			s += x
		}
		return s / n // cpu.* 是"占一个核的百分比"之和，除以核数得整机百分比
	}}
}

var compareDefs = []struct {
	name string
	rows []cmpDef
}{
	{"CPU", []cmpDef{
		perCPU("CPU 忙碌 %", true, "cpu.user", "cpu.sys", "cpu.softirq"),
		core("1 分钟负载", "load", 1, "loadavg.1m"),
		perCPU("iowait %", true, "cpu.iowait"),
		perCPU("steal %（虚机被宿主机抢占）", true, "cpu.steal"),
		perCPU("软中断 %", false, "cpu.softirq"),
		one("运行队列", "count", 1, "procs_running"),
		one("CPU 压力 PSI %", "percent", 1, "psi.cpu.some10"),
		one("上下文切换/秒", "/s", 0, "ctxt"),
		one("中断/秒", "/s", 0, "intr"),
	}},
	{"内存", []cmpDef{
		core("内存使用 %", "percent", 1, "mem.used_pct"),
		core("可用内存", "bytes", -1, "mem.available"),
		core("swap 使用", "bytes", 1, "swap.used"),
		one("内核 slab", "bytes", 1, "slab"),
		one("页缓存", "bytes", 0, "mem.cached"),
		one("脏页", "bytes", 1, "mem.dirty"),
		one("内存压力 PSI %", "percent", 1, "psi.mem.some10"),
		one("主缺页/秒", "/s", 1, "pgmajfault"),
	}},
	{"磁盘", []cmpDef{
		core("根分区使用 %", "percent", 1, "fs.used_pct"),
		core("最忙盘 util %", "percent", 1, "disk.util"),
		core("写延迟", "ms", 1, "disk.await_w"),
		core("D 状态进程（卡在 IO）", "count", 1, "procs_blocked"),
		one("根分区可用", "bytes", -1, "fs.avail"),
		one("inode 使用 %", "percent", 1, "fs.inode_used_pct"),
		one("读延迟", "ms", 1, "disk.await_r"),
		one("读 IOPS", "/s", 0, "disk.riops"),
		one("写 IOPS", "/s", 0, "disk.wiops"),
		one("读吞吐", "bytes/s", 0, "disk.rbytes"),
		one("写吞吐", "bytes/s", 0, "disk.wbytes"),
		one("IO 压力 PSI %", "percent", 1, "psi.io.some10"),
	}},
	{"网络", []cmpDef{
		core("网卡入向", "bytes/s", 0, "net.rx"),
		core("网卡出向", "bytes/s", 0, "net.tx"),
		core("收包/秒", "/s", 0, "net.rx_pps"),
		core("发包/秒", "/s", 0, "net.tx_pps"),
		core("网卡丢包/秒", "/s", 1, "net.rx_drop"),
		// UDP 缓冲区溢出：应用来不及收包，内核直接丢。网卡层 rx_drop 一个不涨，
		// 业务侧已经在超时 —— DNS/NTP/syslog 这类 UDP 服务必须单列。
		core("UDP 缓冲区溢出/秒", "/s", 1, "udp.rcvbuf_errors"),
		core("TCP 重传/秒", "/s", 1, "tcp.retrans"),
		one("UDP 收包错误/秒", "/s", 1, "udp.in_errors"),
		one("UDP 收包/秒", "/s", 0, "udp.in_pps"),
		one("UDP 发包/秒", "/s", 0, "udp.out_pps"),
		one("UDP 端口不可达/秒", "/s", 1, "udp.no_ports"),
		one("发丢包/秒", "/s", 1, "net.tx_drop"),
		one("收包错误/秒", "/s", 1, "net.rx_errs"),
	}},
	{"套接字", []cmpDef{
		core("TCP 已建立", "count", 0, "tcp.estab"),
		core("TCP TIME_WAIT", "count", 1, "sock.tcp_tw"),
		core("UDP socket", "count", 0, "sock.udp_inuse"),
		core("conntrack 使用 %", "percent", 1, "conntrack.used_pct"),
		one("socket 总数", "count", 1, "sock.used"),
		one("TCP inuse", "count", 0, "sock.tcp_inuse"),
		one("TCP orphan", "count", 1, "sock.tcp_orphan"),
		one("TCP alloc", "count", 1, "sock.tcp_alloc"),
		one("conntrack 条目", "count", 1, "conntrack"),
		one("新建连接/秒（被动）", "/s", 0, "tcp.passive_opens"),
		one("连接失败/秒", "/s", 1, "tcp.attempt_fails"),
	}},
}

// procRows 生成"进程"组：占用最高的几个进程，各给 CPU 与内存两行。
// 这是对比表里最该有的东西 —— "DNS 进程内存比昨天多了 5 倍"比任何整机指标都直接。
func (b *HeatmapBuilder) procRows(now time.Time, ids []string) []cmpDef {
	type cand struct {
		key string
		rss float64
		cpu float64
	}
	byKey := map[string]*cand{}
	get := func(k string) *cand {
		if c, ok := byKey[k]; ok {
			return c
		}
		c := &cand{key: k}
		byKey[k] = c
		return c
	}
	last := func(id string) float64 {
		if p, ok := b.series.Last(id); ok && now.Sub(p.TS) <= 2*time.Minute {
			return p.V
		}
		return 0
	}
	for _, id := range ids {
		switch {
		case strings.HasPrefix(id, "proc.rss."):
			get(strings.TrimPrefix(id, "proc.rss.")).rss = last(id)
		case strings.HasPrefix(id, "proc.cpu."):
			if k := strings.TrimPrefix(id, "proc.cpu."); k != "__others__" {
				get(k).cpu = last(id)
			}
		}
	}
	cs := make([]*cand, 0, len(byKey))
	for _, c := range byKey {
		if c.rss > 0 || c.cpu > 0 {
			cs = append(cs, c)
		}
	}
	// 内存大的优先（泄漏是主要场景），同级按 CPU
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].rss != cs[j].rss {
			return cs[i].rss > cs[j].rss
		}
		return cs[i].cpu > cs[j].cpu
	})
	if len(cs) > procCmpTop {
		cs = cs[:procCmpTop]
	}
	var out []cmpDef
	for _, c := range cs {
		if c.rss > 0 {
			out = append(out, core(c.key+" 内存", "bytes", 1, "proc.rss."+c.key))
		}
		if c.cpu > 0 {
			out = append(out, core(c.key+" CPU", "percent", 1, "proc.cpu."+c.key))
		}
	}
	return out
}

// procCmpTop 对比表里最多列几个进程。
const procCmpTop = 5

// lookupTol：历史值的时间容差，约为回看时长的 1%，夹在 30 秒到 10 分钟之间。
func lookupTol(ago time.Duration) time.Duration {
	t := ago / 100
	if t < 30*time.Second {
		t = 30 * time.Second
	}
	if t > 10*time.Minute {
		t = 10 * time.Minute
	}
	return t
}

func (b *HeatmapBuilder) valueAt(d cmpDef, at time.Time, tol time.Duration, current bool) *float64 {
	vals := make([]float64, 0, len(d.ids))
	for _, id := range d.ids {
		var v float64
		var ok bool
		if current {
			var p point
			if p, ok = b.series.Last(id); ok && at.Sub(p.TS) > 2*time.Minute {
				ok = false // 最后一个点太旧：该指标已停止上报
			}
			v = p.V
		} else {
			v, ok = b.series.Lookup(id, at, tol)
		}
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil
		}
		vals = append(vals, v)
	}
	x := vals[0]
	if d.f != nil {
		x = d.f(vals)
	}
	return &x
}

// Compare 生成对比表。多块盘 / 多张网卡时追加逐设备的行。
func (b *HeatmapBuilder) Compare(now time.Time) *CompareJSON {
	out := &CompareJSON{At: now.Unix()}
	for _, c := range compareCols {
		out.Cols = append(out.Cols, c.Name)
	}
	ids := b.series.MetricIDs()
	perDev := func(prefix, label, unit string, bad int) []cmpDef {
		var devs []string
		for _, id := range ids {
			if strings.HasPrefix(id, prefix) {
				devs = append(devs, strings.TrimPrefix(id, prefix))
			}
		}
		if len(devs) < 2 { // 只有一块盘/一张网卡时整机行已经说明一切
			return nil
		}
		sort.Strings(devs)
		var rows []cmpDef
		for _, d := range devs {
			rows = append(rows, one(label+" · "+d, unit, bad, prefix+d))
		}
		return rows
	}
	groups := make([]struct {
		name string
		rows []cmpDef
	}, len(compareDefs))
	copy(groups, compareDefs)
	if pr := b.procRows(now, ids); len(pr) > 0 {
		groups = append(groups, struct {
			name string
			rows []cmpDef
		}{"进程", pr})
	}
	for _, g := range groups {
		defs := g.rows
		switch g.name {
		case "磁盘":
			defs = append(append([]cmpDef(nil), defs...), perDev("disk.util@", "util %", "percent", 1)...)
			defs = append(defs, perDev("disk.await_w@", "写延迟", "ms", 1)...)
		case "网络":
			defs = append(append([]cmpDef(nil), defs...), perDev("net.rx@", "入向", "bytes/s", 0)...)
			defs = append(defs, perDev("net.tx@", "出向", "bytes/s", 0)...)
			defs = append(defs, perDev("net.rx_drop@", "收丢包/秒", "/s", 1)...)
		}
		grp := cmpGroup{Name: g.name}
		for _, d := range defs {
			row := cmpRow{Label: d.label, Unit: d.unit, Bad: d.bad, Core: d.core, ID: strings.Join(d.ids, "+")}
			if row.Now = b.valueAt(d, now, 0, true); row.Now == nil {
				continue // 这台机器没有这项（比如没有 PSI、没有 conntrack）：整行不显示
			}
			active := *row.Now != 0
			for _, c := range compareCols {
				p := b.valueAt(d, now.Add(-c.Ago), lookupTol(c.Ago), false)
				row.Past = append(row.Past, p)
				active = active || (p != nil && *p != 0)
			}
			if strings.Contains(row.ID, "@") && !active {
				continue // 逐设备行：一直空闲的盘/网卡不占行
			}
			grp.Rows = append(grp.Rows, row)
		}
		if len(grp.Rows) > 0 {
			out.Groups = append(out.Groups, grp)
		}
	}
	return out
}

func nan() float64         { return math.NaN() }
func isNaN(v float64) bool { return math.IsNaN(v) || math.IsInf(v, 0) }
