// use.go — USE 五行：每个资源一行，回答"是不是它"。
//
// 名字来自 Brendan Gregg 的 USE 方法：对每个资源问三个问题——
// Utilization（用得多满）、Saturation（有没有人在排队等它）、Errors（有没有出错）。
//
// 它的价值不在"又多了几个指标"，而在**这张清单会终结**：逐个资源走一遍，
// 要么找到问题，要么证明不是它。热力图给不了这个——49 个指标 × 10 档 = 490 个
// 格子，每一屏都在要求用户自己做综合。
//
// 两条自我约束，跟项目其余部分一致：
//
//   - **不发明阈值。** State 只由 L0 那 37 项既有判定和 L3 的 z 决定，
//     两者都已经有各自的门槛（绝对阈值 / minDeltaFor）。这里再加一套
//     "我觉得 80% 算高"只会多一处可错的地方，而且会跟 L0 打架。
//     指标本身只摆"现在多少、平时多少"，判断交给用户。
//   - **采不到就说采不到。** Blind 里的项必须显式出现。一行"网络 正常"
//     会让人以为网络已经查过了，而实际可能是容器里根本读不到 softnet。
//     Gregg 管这个叫 known unknowns，它比多一个指标值钱。
package main

import (
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
	"github.com/githubflyideas/nodedata/internal/deviation"
	"github.com/githubflyideas/nodedata/internal/diagnosis"
	"github.com/githubflyideas/nodedata/internal/metrics"
)

// UseFact 是一条带基线的观测。Base 为 nil 表示历史还不够，说不出"平时多少"。
type UseFact struct {
	Kind  string   `json:"kind"` // U / S / E
	Label string   `json:"label"`
	ID    string   `json:"id"`
	Value float64  `json:"value"`
	Unit  string   `json:"unit"`
	Base  *float64 `json:"base,omitempty"`
	// Z 是该指标此刻最大的 |z|（已排除低置信档）。0 表示没有就绪的档位。
	Z float64 `json:"z,omitempty"`
	// Note 是读这个数之前必须知道的前提，例如"vda 是虚拟盘，util 满不等于盘满"。
	Note string `json:"note,omitempty"`
	// 来源（v5.21）：页面悬停时说清"平时"和 z 是怎么来的。
	BaseN int `json:"base_n,omitempty"` // "平时"用了多少个点（过去 24 小时，不含最近 10 分钟）
	// v5.25：Notable=这条读数让这一行变成"偏离"（坏方向 + 显著 + 持续落在瓶颈区间，见 concern.go）。
	// Change 是不改变状态的变化："高于平时"/"低于平时"——显著，但往好的方向，或还没到可能成为瓶颈的程度。
	Notable bool   `json:"notable,omitempty"`
	Change  string `json:"change,omitempty"`
	ZLag    string `json:"z_lag,omitempty"` // |z| 最大的那一档："1小时"= 跟 1 小时前比
}

// UseCheck 是 L0 里越线的一条判定。它决定 State，UseFact 不决定。
type UseCheck struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Level   int    `json:"level"`
	Message string `json:"message"`
}

// UseRow 是一个资源的一行。
type UseRow struct {
	Resource string     `json:"resource"`
	State    string     `json:"state"`
	Level    int        `json:"level"`
	Facts    []UseFact  `json:"facts,omitempty"`
	Checks   []UseCheck `json:"checks,omitempty"`
	Blind    []string   `json:"blind,omitempty"`
	Movers   []cmpMover `json:"movers,omitempty"`
	// Checked：看过、没问题、所以没列出来的指标（v5.21）。
	// "内存 正常"只写一个数时，看的人（和模型）不知道换出、内存压力查没查过。
	Checked []string `json:"checked,omitempty"`
	L0Total int      `json:"l0_total,omitempty"` // 这个资源的硬阈值检查项数
	L0Pass  int      `json:"l0_pass,omitempty"`  // 其中通过的
	// Commands：下一步该跑的诊断命令。只填 PID，不填进程名（见 nextsteps.go）。
	Commands []NextCmd `json:"commands,omitempty"`
}

type useMetric struct {
	kind, label, id string
	// always：不管偏不偏离都显示。给那些"稳态本身就是问题"的指标用——
	// 一个被单线程钉死在 100% 的核，天天如此，z 永远是 0，
	// 只在偏离时才显示的话，这个最典型的单核瓶颈永远不会出现在页面上。
	always bool
}

// useResources 定义五行。每组第一个指标是"主指标"，无论正不正常都显示——
// "CPU 正常" 远不如 "CPU 正常（用户态 12%，平时 10%）" 有用：后者顺带
// 告诉你基线在哪、采集是通的。其余指标只在越线或显著偏离时才出现。
var useResources = []struct {
	Name       string
	MoverGroup string
	Metrics    []useMetric
	Blind      []string
}{
	{
		Name: "CPU", MoverGroup: "CPU",
		Metrics: []useMetric{
			// 主指标用 0–100 的整机忙碌率，跟下一行的"最忙单核"同口径：
			// 整机 7%、单核 100%，一眼就知道是一个核被钉死了。
			{"U", "整机忙碌", "cpu.busy_pct", false},
			// 整机汇总会把单核打满稀释掉：16 核上一个核 100%，汇总只多 6.25%。
			{"U", "最忙单核", "cpu.core_top1", true},
			{"U", "用户态（各核之和）", "cpu.user", false},
			{"U", "第二忙的核", "cpu.core_top2", false},
			{"U", "内核态（各核之和）", "cpu.sys", false},
			{"U", "软中断（各核之和）", "cpu.softirq", false},
			{"U", "软中断最重的核", "cpu.core_softirq_max", false},
			{"S", "CPU 压力", "psi.cpu.some10", false},
			{"S", "1 分钟负载", "loadavg.1m", false},
			{"S", "运行队列", "procs_running", false},
			{"E", "被偷走的时间", "cpu.steal", false},
		},
	},
	{
		Name: "内存", MoverGroup: "内存",
		Metrics: []useMetric{
			{"U", "已用", "mem.used_pct", false},
			{"U", "可用", "mem.available", false},
			// SwapFree 是存量，不疼；换入换出的速率才疼。一台机器可以
			// swap 用掉一半而毫无感觉，也可以只用 2% 但每秒换出 50MB。
			{"S", "换出", "swap.out", false},
			{"S", "换入", "swap.in", false},
			{"S", "内存压力", "psi.mem.some10", false},
			{"S", "主缺页", "pgmajfault", false},
			{"S", "回写堆积", "mem.writeback", false},
			{"S", "脏页", "mem.dirty", false},
		},
	},
	{
		Name: "磁盘", MoverGroup: "磁盘",
		Metrics: []useMetric{
			// 主指标是 IO 压力，不是 util（v5.20）：util 统计的是"有请求在处理的时间占比"，
			// SSD/NVMe 能并行处理请求，显示 100% 时可能才用了一小部分能力——iostat 手册自己写着。
			// 也不是写等待：整机写等待只在这一轮有写的时候才有值，空闲的盘会被误报成"采不到"。
			{"S", "IO 压力", "psi.io.some10", false},
			{"S", "写等待", "disk.await_w", true},
			{"U", "最忙盘 util", "disk.util", true}, // 两套都显示，附上盘的类型
			{"U", "根分区可用", "fs.avail", false},
			{"S", "读等待", "disk.await_r", false},
			{"S", "在途 I/O", "disk.inflight", false},
			{"S", "阻塞进程", "procs_blocked", false},
		},
		// 实测确认的一个坑，值得写在脸上：子进程被 wait() 回收时，内核把它的
		// I/O 累加进父进程的 signal->ioac，而 /proc/PID/io 读的正是这个。
		// 于是 `bash -c 'dd ...'` 写掉的 3GB，会原封不动记在 bash 头上——
		// 这是内核的记账口径，不是这里算错了，但看的人会以为 bash 在写盘。
		// （CPU 不受影响：采的是 utime/stime，不含 cutime/cstime。）
		Blind: []string{"已退出的短命进程，写入被内核记到父进程头上：" +
			"归因指向 bash/sshd 这类进程时，通常是它派生的某个已退出的子进程干的"},
	},
	{
		Name: "网络",
		Metrics: []useMetric{
			{"U", "入向", "net.rx", false},
			{"U", "出向", "net.tx", false},
			{"S", "softnet 挤压", "net.softnet_squeeze", false},
			{"E", "softnet 丢弃", "net.softnet_drop", false},
			{"E", "收包丢弃", "net.rx_drop", false},
			{"E", "发包丢弃", "net.tx_drop", false},
			{"E", "收包错误", "net.rx_errs", false},
			{"E", "TCP 重传", "tcp.retrans", false},
		},
		// 这一条必须说出来：CPU/磁盘/内存 三行都能给"谁干的"，网络给不了，
		// 用户会默认这里也有。/proc/PID/ 下没有每进程网络计数，要拿到
		// 得走 netlink 的 socket diag 再按 inode 反查，那是另一个量级的活。
		Blind: []string{"流量归因不到进程：/proc 不提供每进程网络计数"},
	},
	{
		// 这一行没有指标，全部内容来自 L0：内核 taint、PID 水位、fd 水位、
		// 开机时长、时钟源。硬塞一个 procs_running 进来只是跟 CPU 行重复，
		// 而"系统槽位"这件事本来就是判定型的——要么够用，要么快用完了。
		Name: "系统",
	},
}

// l0Resource 把 L0 检查号归到资源。首字母够用的地方按首字母，
// 不够用的按号显式指定：F01/F02 是盘上的容量，归磁盘；
// F03 是文件描述符，那是系统槽位不是盘。
func l0Resource(id string) string {
	switch id {
	case "F01", "F02":
		return "磁盘"
	case "F03":
		return "系统"
	}
	if id == "" {
		return ""
	}
	switch id[0] {
	case 'C':
		return "CPU"
	case 'M':
		return "内存"
	case 'D':
		return "磁盘"
	case 'N', 'S': // N=网卡/TCP，S=socket 水位
		return "网络"
	case 'E', 'T', 'F': // E=内核/PID，T=时钟，F=文件系统兜底
		return "系统"
	}
	return ""
}

// useState 把数值等级翻成一个词。
//
// 只有三档，而且刻意不含因果：说"磁盘 异常"，不说"磁盘是瓶颈"。
// 后者是推理，错了比不给更糟。
func useState(level int) string {
	switch {
	case level >= 2:
		return "异常"
	case level == 1:
		return "偏离"
	case level < 0:
		return "无数据"
	default:
		return "正常"
	}
}

// useZThreshold 是"显著偏离基线"的判定线，跟 L4 下结论用的是同一个值。
const useZThreshold = 3.0

// Use 生成五行。now 之外不依赖任何外部状态，可直接在测试里驱动。
func (d *Diagnoser) Use(now time.Time) []UseRow {
	// L0 判定按资源分桶。只有越线的（Level>0）才进 Checks——
	// 把 37 项全列出来就又变成一张网格了。
	checksBy := map[string][]UseCheck{}
	seen := map[string]bool{}
	l0Total, l0Pass := map[string]int{}, map[string]int{}
	for _, cat := range d.loadL0() {
		for _, c := range cat.Checks {
			res := l0Resource(c.ID)
			if res == "" {
				continue
			}
			seen[res] = true
			l0Total[res]++
			if c.Level == 0 {
				l0Pass[res]++
			}
			if c.Level > 0 {
				checksBy[res] = append(checksBy[res], UseCheck{
					ID: c.ID, Name: c.Name, Level: c.Level, Message: c.Message})
			}
		}
	}

	// L3 的 z 已经过了低置信档过滤与 minDeltaFor 门槛，直接用，不再加门槛。
	zBy := map[string]float64{}
	zLag := map[string]string{}
	for _, dev := range d.latestDeviationsAt(now) {
		peak, at := 0.0, -1
		for i, z := range dev.Z {
			if !math.IsNaN(z) && math.Abs(z) > math.Abs(peak) {
				peak, at = z, i
			}
		}
		zBy[dev.MetricID] = peak
		if at >= 0 {
			zLag[dev.MetricID] = deviation.LagName(at)
		}
	}

	var ids []string
	if d.series != nil {
		ids = d.series.MetricIDs()
	}

	cctx := d.concernCtx()
	out := make([]UseRow, 0, len(useResources))
	for _, res := range useResources {
		row := UseRow{Resource: res.Name, Blind: append([]string(nil), res.Blind...)}
		level := 0
		haveAny := false

		for i, m := range res.Metrics {
			f, ok := d.useFact(m, now)
			if !ok {
				// 主指标采不到要说；次要指标采不到是常态（没装 swap、
				// 容器里没有 softnet），逐条列出来只会刷屏。
				if i == 0 {
					row.Blind = append(row.Blind, m.label+" 采不到")
				}
				continue
			}
			haveAny = true
			f.Z = zBy[m.id]
			f.Note = d.noteFor(m.id, now)
			// 显著 ≠ 有问题：往坏的方向、且过去 60 秒一直落在可能成为瓶颈的区间，才算偏离。
			significant := math.Abs(f.Z) >= useZThreshold
			notable := significant && f.Z*metrics.Lookup(m.id).Direction() > 0 &&
				d.sustainedConcern(m.id, f.Value, now, cctx)
			if notable {
				f.Notable, f.ZLag = true, zLag[m.id]
			} else if why := d.sustainedHard(m.id, f.Value, now, cctx); why != "" {
				// 绝对线：不看历史。z 不重要，写清楚是哪条线
				notable, f.Notable = true, true
				if f.Note != "" {
					f.Note += "；"
				}
				f.Note += why + "（绝对线，持续 1 分钟）"
			} else if significant {
				f.Change = "低于平时"
				if f.Z > 0 {
					f.Change = "高于平时"
				}
			}
			if notable && level < 1 {
				level = 1
			}
			// 显著的变化照样展示（这是展示工具），只是不改变这一行的状态。
			if i == 0 || m.always || notable || f.Change != "" {
				row.Facts = append(row.Facts, f)
			} else {
				row.Checked = append(row.Checked, m.label)
			}
		}

		row.Checks = checksBy[res.Name]
		row.L0Total, row.L0Pass = l0Total[res.Name], l0Pass[res.Name]
		for _, c := range row.Checks {
			if c.Level > level {
				level = c.Level
			}
		}
		// 既没有指标也没有 L0 判定 = 这个资源根本没被看过，不能报"正常"。
		if !haveAny && !seen[res.Name] {
			level = -1
		}
		row.Level, row.State = level, useState(level)

		// 谁干的：只在这一行有事时才算。一台安静机器上列一串进程名是噪音。
		var procs []diagnosis.Proc
		if level > 0 && d.procs != nil {
			procs = d.procs()
		}
		if level > 0 && res.MoverGroup != "" && d.builder != nil {
			row.Movers = d.builder.movers(res.MoverGroup, now, ids)
			if len(row.Movers) > 3 {
				row.Movers = row.Movers[:3]
			}
			attachPIDs(row.Movers, procs)
		}
		if level > 0 {
			row.Commands = nextSteps(res.Name, row.Movers, d.busiestNIC(now))
		}
		out = append(out, row)
	}
	return out
}

// useFact 取某指标此刻的值与基线。超过 2 分钟没更新就算采不到——
// 跟 healthValues 用同一个新鲜度标准。
func (d *Diagnoser) useFact(m useMetric, now time.Time) (UseFact, bool) {
	if d.series == nil {
		return UseFact{}, false
	}
	p, ok := d.series.Last(m.id)
	if !ok || now.Sub(p.TS) > 2*time.Minute || math.IsNaN(p.V) {
		return UseFact{}, false
	}
	base, n := d.baselineN(m.id, now)
	return UseFact{
		Kind: m.kind, Label: m.label, ID: m.id,
		Value: p.V, Unit: unitOf(m.id), Base: base, BaseN: n,
	}, true
}

// baselineOf 取该指标"平时"的值：最近 24 小时长期层的中位数。
//
// 掐掉最后 10 分钟，否则正在发生的尖峰会把自己的基线抬上去——
// 一个持续 20 分钟的故障会让"平时"看起来也很糟，于是没人觉得不对。
// 用中位数不用均值：一次 dd 就能把均值拉到没法看。
func (d *Diagnoser) baselineOf(id string, now time.Time) *float64 {
	b, _ := d.baselineN(id, now)
	return b
}

// baselineN 同 baselineOf，另外返回用了多少个点（页面悬停时说明"平时"的来历）。
func (d *Diagnoser) baselineN(id string, now time.Time) (*float64, int) {
	if d.series == nil {
		return nil, 0
	}
	pts := d.series.CoarseRange(id, now.Add(-24*time.Hour), now.Add(-10*time.Minute))
	v := make([]float64, 0, len(pts))
	for _, p := range pts {
		if !math.IsNaN(p.V) {
			v = append(v, p.V)
		}
	}
	// 少于 12 个点（长期层 5 分钟一点 = 1 小时）说不出"平时"，
	// 宁可不给也不要拿 3 个点编一个出来。
	if len(v) < 12 {
		return nil, len(v)
	}
	sort.Float64s(v)
	m := v[len(v)/2]
	return &m, len(v)
}

// worstUse 返回五行里最严重的等级，供 /health.txt 与页面标题用。
func worstUse(rows []UseRow) int {
	w := 0
	for _, r := range rows {
		if r.Level > w {
			w = r.Level
		}
	}
	return w
}

// noteFor 给个别指标附上读数的前提。只写事实，不写结论。
func (d *Diagnoser) noteFor(id string, now time.Time) string {
	switch id {
	case "disk.util":
		if d.disks == nil {
			return ""
		}
		di := d.disks()
		if di.Busiest == "" {
			return ""
		}
		if di.BusiestKind == collector.DiskHDD {
			return di.Busiest + " 是机械盘"
		}
		return di.Busiest + " 是" + collector.DiskKindName(di.BusiestKind) + "：util 满不等于盘满，仅供参考"
	case "loadavg.1m":
		// Linux 的负载把在等 IO 的进程（D 状态）也算进去了：负载高不一定是 CPU 忙
		note := ""
		if d.cores > 0 {
			note = "本机 " + strconv.Itoa(d.cores) + " 核"
		}
		if d.series != nil {
			if p, ok := d.series.Last("procs_blocked"); ok && now.Sub(p.TS) <= 2*time.Minute && p.V >= 1 {
				if note != "" {
					note += "；"
				}
				note += "其中 " + strconv.Itoa(int(p.V+0.5)) + " 个是在等 IO 的进程，不是 CPU 忙"
			}
		}
		return note
	}
	return ""
}

// concernCtx 收集判断"是不是瓶颈"要用的机器背景。
func (d *Diagnoser) concernCtx() concernCtx {
	c := concernCtx{cores: d.cores, last: func(id string) (float64, bool) {
		if d.series == nil {
			return 0, false
		}
		p, ok := d.series.Last(id)
		return p.V, ok && !math.IsNaN(p.V)
	}}
	if d.disks != nil {
		c.diskKind = d.disks().BusiestKind
	}
	return c
}

// concernHold 是"落在瓶颈区间"要持续多久才算数：一闪而过的尖峰不点亮墙上的方块。
// 按数据判断（过去这段时间的原始点全部在区间里），不靠调用之间记状态——结果可重复、可测。
const concernHold = 60 * time.Second

func (d *Diagnoser) sustainedConcern(id string, cur float64, now time.Time, c concernCtx) bool {
	if in, known := inConcern(id, cur, c); !known || !in {
		return false
	}
	if d.series == nil {
		return false
	}
	pts := d.series.RawRange(id, now.Add(-concernHold), now)
	if len(pts) < 2 {
		return false // 刚开始采：没有 60 秒的依据，先不报
	}
	if now.Sub(pts[0].TS) < concernHold*3/4 {
		return false // 窗口里的点没覆盖够 60 秒（中间断过采）
	}
	for _, p := range pts {
		if in, _ := inConcern(id, p.V, c); !in {
			return false
		}
	}
	return true
}

// sustainedHard：过去 60 秒一直在绝对线以上，返回那条线的说明；否则返回空。
func (d *Diagnoser) sustainedHard(id string, cur float64, now time.Time, c concernCtx) string {
	in, why := hardLine(id, cur, c)
	if !in || d.series == nil {
		return ""
	}
	pts := d.series.RawRange(id, now.Add(-concernHold), now)
	if len(pts) < 2 || now.Sub(pts[0].TS) < concernHold*3/4 {
		return ""
	}
	for _, p := range pts {
		if ok, _ := hardLine(id, p.V, c); !ok {
			return ""
		}
	}
	return why
}
