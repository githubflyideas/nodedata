// diagnosis.go — L4 诊断链。
// 输入 L0 检查结果与 L3 偏离度快照，输出按严重度排序的诊断条目。
// 本文件不含任何硬编码结论：每条诊断的数值与证据均来自入参。
package diagnosis

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Level 诊断级别。与 L0 的 level 语义一致：0=info/pass, 1=warn, 2=critical/fail。
type Level int

const (
	Info     Level = 0
	Warning  Level = 1
	Critical Level = 2
)

func (l Level) String() string {
	switch l {
	case Critical:
		return "CRITICAL"
	case Warning:
		return "WARN"
	default:
		return "INFO"
	}
}

// L0Check 是一条 L0 判断（与 check.Check 字段对应，避免包循环依赖）。
type L0Check struct {
	ID      string
	Name    string
	Level   int
	Message string
}

// L0Category 是一组 L0 判断。
type L0Category struct {
	Name   string
	Level  int
	Checks []L0Check
}

// Deviation 是某指标在某时刻的偏离度观测。
type Deviation struct {
	MetricID string
	Domain   string
	Unit     string
	Value    float64
	// Z 十四档 z 值；NaN 表示该档未就绪。
	Z        []float64
	OnsetLag string
	Breadth  int
}

// Item 是一条诊断。
type Item struct {
	Level       Level     `json:"level"`
	Category    string    `json:"category"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Impact      string    `json:"impact"`
	Action      string    `json:"action"`
	Evidence    []string  `json:"evidence"`
	Timestamp   time.Time `json:"timestamp"`

	// 归因（v3.1.0）：Class 非空的条目由 causal 生成。
	Class    string    `json:"class,omitempty"`
	Culprits []Culprit `json:"culprits,omitempty"`
	Commands []string  `json:"commands,omitempty"`
	Metrics  []string  `json:"metrics,omitempty"`
}

// Chain 是诊断链。
type Chain struct {
	Items     []Item    `json:"items"`
	Priority  Level     `json:"priority"`
	Count     int       `json:"count"`
	Timestamp time.Time `json:"timestamp"`
	// Notes 记录诊断过程本身的局限，例如某些档位尚未就绪。
	Notes []string `json:"notes"`
}

// Options 控制诊断阈值。
type Options struct {
	// ZThreshold 触发偏离诊断的 |z| 阈值，默认 3.0。
	ZThreshold float64
	// BreadthCritical 达到该档位广度时升级为 critical，默认 5。
	BreadthCritical int
	// MaxItems 输出上限，0 表示不限。
	MaxItems int
	Now      time.Time
	// Procs 是 L1 最近一轮进程快照；为空时归因只能到设备/接口。
	Procs []Proc
}

func (o *Options) fill() {
	if o.ZThreshold <= 0 {
		o.ZThreshold = 3.0
	}
	if o.BreadthCritical <= 0 {
		o.BreadthCritical = 5
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
}

// Diagnose 依据 L0 分类结果与 L3 偏离度构造诊断链。
// 两个入参都可以为空：为空则相应来源不产出诊断。
func Diagnose(l0 []L0Category, devs []Deviation, opt Options) *Chain {
	opt.fill()
	c := &Chain{Timestamp: opt.Now, Items: []Item{}, Notes: []string{}}

	c.Items = append(c.Items, fromL0(l0, opt)...)
	causalItems, used := causal(devs, opt)
	c.Items = append(c.Items, causalItems...)
	rest := make([]Deviation, 0, len(devs))
	for _, d := range devs {
		// 已被归因消化的、进程序列与单设备序列（它们只作证据）不再单独出条目
		if used[d.MetricID] || strings.HasPrefix(d.MetricID, "proc.") || strings.Contains(d.MetricID, "@") {
			continue
		}
		rest = append(rest, d)
	}
	devItems, notes := fromDeviations(rest, opt)
	c.Items = append(c.Items, devItems...)
	c.Notes = append(c.Notes, notes...)

	// 严重度降序；同级按 category 再按 title，保证输出稳定
	sort.SliceStable(c.Items, func(i, j int) bool {
		a, b := c.Items[i], c.Items[j]
		if a.Level != b.Level {
			return a.Level > b.Level
		}
		if a.Category != b.Category {
			return a.Category < b.Category
		}
		return a.Title < b.Title
	})

	if opt.MaxItems > 0 && len(c.Items) > opt.MaxItems {
		c.Notes = append(c.Notes,
			fmt.Sprintf("truncated: %d of %d items shown", opt.MaxItems, len(c.Items)))
		c.Items = c.Items[:opt.MaxItems]
	}

	c.Priority = Info
	for _, it := range c.Items {
		if it.Level > c.Priority {
			c.Priority = it.Level
		}
	}
	c.Count = len(c.Items)
	if c.Count == 0 {
		c.Notes = append(c.Notes, "no findings: L0 all pass and no metric exceeded the z threshold")
	}
	return c
}

// fromL0 把每个未通过的 L0 判断转成一条诊断。
func fromL0(cats []L0Category, opt Options) []Item {
	var out []Item
	for _, cat := range cats {
		for _, ck := range cat.Checks {
			if ck.Level <= 0 {
				continue // pass
			}
			lvl := Warning
			if ck.Level >= 2 {
				lvl = Critical
			}
			ev := []string{fmt.Sprintf("L0 %s/%s level=%d", cat.Name, ck.ID, ck.Level)}
			if ck.Message != "" {
				ev = append(ev, "message: "+ck.Message)
			}
			out = append(out, Item{
				Level:       lvl,
				Category:    cat.Name,
				Title:       ck.Name,
				Description: ck.Message,
				Impact:      impactForDomain(cat.Name, lvl),
				Action:      actionForCheck(cat.Name, ck.ID),
				Evidence:    ev,
				Timestamp:   opt.Now,
			})
		}
	}
	return out
}

// fromDeviations 把超阈值的偏离度转成诊断。
func fromDeviations(devs []Deviation, opt Options) ([]Item, []string) {
	var out []Item
	var notes []string
	notReady := 0

	for _, d := range devs {
		peak, peakLag, ready := peakZ(d.Z)
		if !ready {
			notReady++
			continue
		}
		if math.Abs(peak) < opt.ZThreshold {
			continue
		}
		lvl := Warning
		if d.Breadth >= opt.BreadthCritical {
			lvl = Critical
		}
		dir := "上升"
		if peak < 0 {
			dir = "下降"
		}
		onset := d.OnsetLag
		if onset == "" {
			onset = peakLag
		}
		out = append(out, Item{
			Level:    lvl,
			Category: d.Domain,
			Title:    fmt.Sprintf("%s 偏离基线 %.1fσ（%s）", d.MetricID, math.Abs(peak), dir),
			Description: fmt.Sprintf(
				"当前值 %.3f %s；峰值偏离出现在 %s 档，十四档中有 %d 档超过 |z|≥%.1f。",
				d.Value, d.Unit, peakLag, d.Breadth, opt.ZThreshold),
			Impact: impactForDomain(d.Domain, lvl),
			Action: actionForDomain(d.Domain, d.MetricID),
			Evidence: []string{
				fmt.Sprintf("L1 %s = %.3f %s", d.MetricID, d.Value, d.Unit),
				fmt.Sprintf("L3 peak z = %+.2f at %s", peak, peakLag),
				fmt.Sprintf("L3 onset_lag = %s, breadth = %d", onset, d.Breadth),
			},
			Timestamp: opt.Now,
		})
	}
	if notReady > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d metrics skipped: no lag bucket ready yet (needs longer history)", notReady))
	}
	return out, notes
}

// peakZ 返回绝对值最大的 z 及其档位名；全部 NaN 时 ready=false。
func peakZ(z []float64) (peak float64, lag string, ready bool) {
	idx := -1
	for i, v := range z {
		if math.IsNaN(v) {
			continue
		}
		ready = true
		if idx < 0 || math.Abs(v) > math.Abs(peak) {
			peak, idx = v, i
		}
	}
	if idx >= 0 {
		lag = fmt.Sprintf("L%d", idx+1)
	}
	return
}

func impactForDomain(domain string, lvl Level) string {
	sev := "可能影响"
	if lvl == Critical {
		sev = "正在影响"
	}
	switch strings.ToLower(domain) {
	case "cpu", "load":
		return sev + "请求处理延迟与队列积压"
	case "mem", "memory":
		return sev + "进程存活（OOM 风险）与页回收开销"
	case "disk", "disk/io", "io":
		return sev + "落盘延迟，进而放大上游超时"
	case "net", "network":
		return sev + "连接建立与重传，表现为间歇性超时"
	case "conntrack", "socket":
		return sev + "新建连接被丢弃"
	case "fs", "filesystem":
		return sev + "写入失败与日志中断"
	case "time", "time/sync":
		return sev + "跨节点日志排序与指标对齐"
	case "psi":
		return sev + "整机资源争抢导致的停顿"
	default:
		return sev + "该子系统的正常服务"
	}
}

func actionForDomain(domain, metricID string) string {
	switch strings.ToLower(domain) {
	case "cpu", "load":
		return "top -H -o %CPU；确认是否单核饱和或 steal 偏高"
	case "mem", "memory":
		return "查看 /proc/meminfo 与 dmesg -T | grep -i oom"
	case "disk", "io":
		return "iostat -x 1 3；定位高 await 的设备与写入来源"
	case "net", "network":
		return "ss -s；对照 net.snmp 重传与丢包计数"
	case "conntrack":
		return "对比 nf_conntrack_count 与 nf_conntrack_max"
	case "psi":
		return "cat /proc/pressure/{cpu,io,memory} 确认压力来源"
	default:
		return "查看指标 " + metricID + " 的原始时间序列：/api/raw?metric=" + metricID
	}
}

func actionForCheck(category, id string) string {
	switch strings.ToLower(category) {
	case "time", "time/sync":
		return "timedatectl status；必要时重启时间同步服务"
	case "conntrack":
		return "对比 nf_conntrack_count 与 nf_conntrack_max"
	case "fs", "filesystem":
		return "df -h 与 df -i，确认空间与 inode"
	case "socket":
		return "ss -s 查看 socket 与 TIME_WAIT 规模"
	case "errors":
		return "dmesg -T | tail -50"
	default:
		return "复核 L0 判断 " + id + " 的采集来源"
	}
}

// String 返回可读摘要。
func (c *Chain) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "L4 diagnosis: %d item(s), priority=%s\n", c.Count, c.Priority)
	for _, it := range c.Items {
		fmt.Fprintf(&b, "[%s] %s — %s\n", it.Level, it.Category, it.Title)
		if it.Action != "" {
			fmt.Fprintf(&b, "    action: %s\n", it.Action)
		}
		for _, e := range it.Evidence {
			fmt.Fprintf(&b, "    evidence: %s\n", e)
		}
	}
	for _, n := range c.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	return b.String()
}
