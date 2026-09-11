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
	ids         []string
	f           func(v []float64) float64
}

func one(label, unit string, bad int, id string) cmpDef {
	return cmpDef{label: label, unit: unit, bad: bad, ids: []string{id}}
}

func perCPU(label string, ids ...string) cmpDef {
	n := float64(runtime.NumCPU())
	return cmpDef{label: label, unit: "percent", bad: 1, ids: ids, f: func(v []float64) float64 {
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
		perCPU("CPU 忙碌 %", "cpu.user", "cpu.sys", "cpu.softirq"),
		perCPU("iowait %", "cpu.iowait"),
		perCPU("steal %（虚机被宿主机抢占）", "cpu.steal"),
		one("1 分钟负载", "load", 1, "loadavg.1m"),
		one("运行队列", "count", 1, "procs_running"),
		one("CPU 压力 PSI %", "percent", 1, "psi.cpu.some10"),
		one("上下文切换/秒", "/s", 0, "ctxt"),
		one("中断/秒", "/s", 0, "intr"),
	}},
	{"内存", []cmpDef{
		one("可用内存", "bytes", -1, "mem.available"),
		one("页缓存", "bytes", 0, "mem.cached"),
		one("脏页", "bytes", 1, "mem.dirty"),
		one("内核 slab", "bytes", 1, "slab"),
		one("swap 使用", "bytes", 1, "swap.used"),
		one("内存压力 PSI %", "percent", 1, "psi.mem.some10"),
		one("主缺页/秒", "/s", 1, "pgmajfault"),
	}},
	{"磁盘", []cmpDef{
		one("最忙盘 util %", "percent", 1, "disk.util"),
		one("读延迟", "ms", 1, "disk.await_r"),
		one("写延迟", "ms", 1, "disk.await_w"),
		one("读 IOPS", "/s", 0, "disk.riops"),
		one("写 IOPS", "/s", 0, "disk.wiops"),
		one("读吞吐", "bytes/s", 0, "disk.rbytes"),
		one("写吞吐", "bytes/s", 0, "disk.wbytes"),
		one("IO 压力 PSI %", "percent", 1, "psi.io.some10"),
		one("D 状态进程", "count", 1, "procs_blocked"),
	}},
	{"网络", []cmpDef{
		one("网卡入向", "bytes/s", 0, "net.rx"),
		one("网卡出向", "bytes/s", 0, "net.tx"),
		one("收包/秒", "/s", 0, "net.rx_pps"),
		one("发包/秒", "/s", 0, "net.tx_pps"),
		one("收丢包/秒", "/s", 1, "net.rx_drop"),
		one("发丢包/秒", "/s", 1, "net.tx_drop"),
		one("TCP 重传/秒", "/s", 1, "tcp.retrans"),
		one("conntrack 条目", "count", 1, "conntrack"),
	}},
}

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
	for _, g := range compareDefs {
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
			row := cmpRow{Label: d.label, Unit: d.unit, Bad: d.bad, ID: strings.Join(d.ids, "+")}
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
