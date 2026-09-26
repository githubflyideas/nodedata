// report.go — /api/report：给大模型看的纯文本简报。
//
// 越来越多的排障第一现场不是人，是一个在做初筛的模型。那个模型的处境很具体：
// 它没有这台机器的任何上下文，输入按 token 计费，而且它会**据实回答**——
// 你给它的东西里没提网络，它就会说网络正常。
//
// 于是有六条，每一条都对应一个实际会犯的错：
//
//  1. 只报异常，不报全量。正常指标的信息量是零，而 nodedata 已经知道什么算正常。
//     全量 dump 是让模型替你做筛选，它筛得比 L0+L3 差，还要烧掉 20k token。
//  2. 纯文本行，不用 JSON。`disk.await_w sda 41ms (平时 2ms)` 约 15 token，
//     等价 JSON 因为括号、引号、键名要 40+。一份报告里这个差距是 3 倍。
//  3. 绝对值配基线，不给百分比。"▲550%" 离开基数没有意义——
//     盘 util 从 0.08% 到 0.52% 也是 ▲550%，那台机器在睡觉。
//  4. 归因写在同一行。"谁干的"是每 token 信息量最高的一句。
//  5. **显式声明看不到的东西。** 没有这一段，模型会把"报告里没提"
//     读成"查过了没问题"。这是这份报告里最容易被省掉、后果最严重的一段。
//  6. 按严重度排序。上下文被截断时先丢掉的是"正常"那几行。
//
// 目标体量 500–1500 token。超出说明前五条有一条没做到。
package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/githubflyideas/nodedata/internal/diagnosis"
)

// reportMaxClues 限制线索条数。线索是"测试中"的推理，多给不等于多有用，
// 而模型会把列出来的东西当成已经成立的事实。
const reportMaxClues = 3

// textReport 生成简报（只有资源行和线索，给测试和旧调用方用）。
func textReport(host string, rows []UseRow, chain *diagnosis.Chain, now time.Time) string {
	return renderReport(reportData{Host: host, At: now, Rows: rows, Chain: chain})
}

// renderReport 按固定顺序写：身份 → 背景 → 先后 → 最近变化 → 各资源 → 线索 → 看不到。
// 缺哪个来源就少哪一段，不写"未知"占位——占位也是 token。
func renderReport(rd reportData) string {
	var b strings.Builder
	w := func(format string, a ...interface{}) { fmt.Fprintf(&b, format, a...) }

	// 头：模型没有上下文，主机名和时刻必须在第一行，否则它无从判断
	// 这是哪台机器、数据有多新。
	w("NODEDATA host=%s at=%s state=%s\n", nonEmpty(rd.Host, "unknown"), rd.At.Format(time.RFC3339), useState(worstUse(rd.Rows)))
	if m := rd.Machine; m != nil {
		parts := []string{strconv.Itoa(m.Cores) + "核"}
		if m.MemTotal > 0 {
			parts = append(parts, siBytes(float64(m.MemTotal), "")+"内存")
		}
		if len(m.Disks) > 0 {
			parts = append(parts, "盘 "+strings.Join(m.Disks, " "))
		}
		if m.Kernel != "" {
			parts = append(parts, "内核 "+m.Kernel)
		}
		if m.Virt != "" {
			parts = append(parts, m.Virt)
		} else {
			parts = append(parts, "物理机")
		}
		if m.Uptime > 0 {
			parts = append(parts, "开机 "+humanDur(m.Uptime))
		}
		w("机器 %s\n", strings.Join(parts, " · "))
	}
	if l := rd.Learn; l != nil && l.Span > 0 {
		line := "nodedata 已攒 " + humanDur(l.Span) + " 历史"
		switch {
		case l.Next == "":
			line += "：九个时间尺度都能比"
		case len(l.Usable) > 0:
			line += "：最长能跟" + l.Usable[len(l.Usable)-1] + "前比"
			if l.NextWait > 0 {
				line += "，跟" + l.Next + "前比还要 " + humanDur(l.NextWait)
			}
		}
		w("%s\n", line)
	}
	w("口径 平时=过去24h同指标中位数(不含最近10分钟)；z=超出平时波动的倍数，|z|≥3才列\n")

	// 先后：只摆顺序，不说谁导致谁。读者自己会推。
	if tl := timelineLines(rd); len(tl) > 0 {
		w("\n先后（本次从 %s 开始；10秒内的算同时）\n", rd.Timeline[0].At.Format("15:04:05"))
		for _, l := range tl {
			w("  %s\n", l)
		}
	}

	// 最近变化：写"没有"跟写"有"一样重要——它把发版、重启、配置变更排除掉了。
	if cl := changeLines(rd); len(cl) > 0 {
		w("\n最近变化（72小时）\n")
		for _, l := range cl {
			w("  %s\n", l)
		}
	}

	// 严重的排前面。被截断时先丢掉的应该是"正常"那几行。
	ord := make([]UseRow, len(rd.Rows))
	copy(ord, rd.Rows)
	sort.SliceStable(ord, func(i, j int) bool { return ord[i].Level > ord[j].Level })
	b.WriteString("\n")
	for _, r := range ord {
		var facts, notes []string
		for _, f := range r.Facts {
			facts = append(facts, factLine(f))
			if f.Note != "" {
				notes = append(notes, f.Note)
			}
		}
		line := r.Resource + " " + r.State
		if len(facts) > 0 {
			line += "  " + strings.Join(facts, " · ")
		}
		var checked []string
		if len(r.Checked) > 0 {
			checked = append(checked, "已查 "+strings.Join(r.Checked, "、"))
		}
		if r.L0Total > 0 {
			checked = append(checked, fmt.Sprintf("硬阈值 %d/%d 通过", r.L0Pass, r.L0Total))
		}
		if len(checked) > 0 {
			line += " · " + strings.Join(checked, "；")
		}
		b.WriteString(line + "\n")
		for _, n := range notes {
			w("  注  %s\n", n)
		}
		for _, c := range r.Checks {
			w("  [%s] %s %s\n", c.ID, c.Name, c.Message)
		}
		if len(r.Movers) > 0 {
			w("  谁干的  %s\n", moversLine(r.Movers))
		}
		for i, c := range r.Commands {
			lead := "        "
			if i == 0 {
				lead = "  下一步"
			}
			why := c.Cost
			if c.Why != "" {
				why += "，" + c.Why
			}
			w("%s  %s    # %s\n", lead, c.Cmd, why)
		}
	}

	// 线索单独成段并标"测试中"：放进上面的资源行里，模型会当成判定。
	if rd.Chain != nil && len(rd.Chain.Items) > 0 {
		var clues []string
		for _, it := range rd.Chain.Items {
			if it.Class == "" {
				continue // 只有归因产生的条目算线索，硬阈值已经在上面了
			}
			clues = append(clues, it.Title)
			if len(clues) >= reportMaxClues {
				break
			}
		}
		if len(clues) > 0 {
			b.WriteString("\n线索(测试中，未验证的推测):\n")
			for _, c := range clues {
				b.WriteString("  " + c + "\n")
			}
		}
	}

	// 最后一段，也是最重要的一段：没有它，"网络 正常"会被读成"网络查过了"。
	var blind []string
	for _, r := range rd.Rows {
		for _, s := range r.Blind {
			blind = append(blind, r.Resource+" "+s)
		}
	}
	if rd.KernelErr != "" && rd.HaveChanges {
		blind = append(blind, "内核日志读不到（"+rd.KernelErr+"）：OOM、IO 错误这类事件无从确认")
	}
	if len(blind) > 0 {
		b.WriteString("\n看不到(以下方面本报告没有依据，不要据此下结论):\n")
		for _, s := range blind {
			b.WriteString("  " + s + "\n")
		}
	}
	return b.String()
}

// humanDur：5.2天 / 13小时 / 40分钟。
func humanDur(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return trimNum(d.Hours()/24, 1) + "天"
	case d >= time.Hour:
		return trimNum(d.Hours(), 1) + "小时"
	default:
		return trimNum(d.Minutes(), 0) + "分钟"
	}
}

// factLine 把一条观测写成一行：`写等待 41ms (平时 2ms, z=7.2)`。
func factLine(f UseFact) string {
	s := f.Label + " " + fmtUnit(f.Value, f.Unit)
	var extra []string
	if f.Base != nil {
		extra = append(extra, "平时 "+fmtUnit(*f.Base, f.Unit))
	}
	if f.Notable {
		extra = append(extra, "z="+strconv.FormatFloat(f.Z, 'f', 1, 64))
	} else if f.Change != "" {
		extra = append(extra, f.Change) // 显著但不是问题：只说方向，不给 z，免得读的人（或模型）当成告警
	}
	if len(extra) > 0 {
		s += " (" + strings.Join(extra, ", ") + ")"
	}
	return s
}

// moversLine 把归因压成一行：`rsync 620MB/s (1h前 1MB/s), mysqld 40MB/s`。
func moversLine(ms []cmpMover) string {
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		who := m.Name
		switch {
		case m.PID > 0 && m.Parent != "":
			who += "(" + strconv.Itoa(m.PID) + ",父进程" + m.Parent + ")"
		case m.PID > 0:
			who += "(" + strconv.Itoa(m.PID) + ")"
		}
		s := who + " " + fmtUnit(m.Now, m.Unit)
		if m.Past != nil {
			s += " (" + colName(m.Col) + "前 " + fmtUnit(*m.Past, m.Unit) + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

// fmtUnit 按单位格式化。刻意短：这份报告按 token 计费，
// "620MB/s" 和 "620.00 MB/s" 信息量一样，后者贵 3 个 token。
func fmtUnit(v float64, unit string) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "?"
	}
	switch unit {
	case "percent":
		return trimNum(v, 1) + "%"
	case "ms":
		return trimNum(v, 1) + "ms"
	case "s":
		return trimNum(v, 1) + "s"
	case "bytes":
		return siBytes(v, "")
	case "bytes/s":
		return siBytes(v, "/s")
	case "ops/s", "/s":
		return siCount(v) + "/s"
	case "load":
		return trimNum(v, 2)
	default:
		return siCount(v)
	}
}

func siBytes(v float64, suffix string) string {
	neg := v < 0
	if neg {
		v = -v
	}
	u := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	i := 0
	for v >= 1024 && i < len(u)-1 {
		v /= 1024
		i++
	}
	s := trimNum(v, 1) + u[i] + suffix
	if neg {
		s = "-" + s
	}
	return s
}

func siCount(v float64) string {
	a := math.Abs(v)
	switch {
	case a >= 1e9:
		return trimNum(v/1e9, 1) + "G"
	case a >= 1e6:
		return trimNum(v/1e6, 1) + "M"
	case a >= 1e4:
		return trimNum(v/1e3, 1) + "k"
	default:
		return trimNum(v, 1)
	}
}

// trimNum 去掉没有信息量的尾零：12.0 -> 12，0.50 -> 0.5。
func trimNum(v float64, prec int) string {
	s := strconv.FormatFloat(v, 'f', prec, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	if s == "-0" {
		return "0"
	}
	return s
}

func nonEmpty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// colName 把对比列编号 "1h"/"3d" 写成 "1小时"/"3天"，跟页面同一套说法。
func colName(c string) string {
	if n := len(c); n >= 2 {
		switch c[n-1] {
		case 'm':
			return c[:n-1] + "分钟"
		case 'h':
			return c[:n-1] + "小时"
		case 'd':
			return c[:n-1] + "天"
		}
	}
	return c
}

// timelineLines 是"先后"一段的每一行（报告和页面共用同一套写法）。
func timelineLines(rd reportData) []string {
	var out []string
	for _, e := range rd.Timeline {
		mark := ""
		if e.First {
			mark = "   ← 最先越线"
		}
		out = append(out, fmt.Sprintf("%s  %s %s → %s%s", e.At.Format("15:04:05"), e.What, e.From, e.To, mark))
	}
	return out
}

// changeLines 是"最近变化"一段的每一行。
func changeLines(rd reportData) []string {
	if !rd.HaveChanges {
		return nil
	}
	var out []string
	if len(rd.Services) == 0 {
		out = append(out, fmt.Sprintf("服务：没有出现、消失或重启（%d 个服务在跑）", rd.SvcCount))
	}
	for _, c := range rd.Services {
		out = append(out, c.At.Format("01-02 15:04")+"  "+c.Text)
	}
	if len(rd.NewProcs) == 0 {
		out = append(out, "进程：没有新进程进入 CPU/读写/内存前列")
	}
	for _, c := range rd.NewProcs {
		out = append(out, c.At.Format("01-02 15:04")+"  "+c.Text)
	}
	switch {
	case rd.KernelErr != "":
		// 读不到放进"看不到"那一段
	case len(rd.Kernel) == 0:
		out = append(out, "内核日志：没有 OOM、IO 错误、网卡状态变化、进程卡死")
	default:
		for _, c := range rd.Kernel {
			out = append(out, c.At.Format("01-02 15:04")+"  内核："+c.Text)
		}
	}
	if len(rd.Episodes) > 0 {
		out = append(out, "过去的事（按每5分钟桶的最坏值）")
		for _, c := range rd.Episodes {
			out = append(out, "  "+c.At.Format("01-02 15:04")+"–"+c.End.Format("15:04")+"  "+c.Text)
		}
	}
	return out
}

// ContextJSON 是页面上"先后 / 最近变化"两块的数据：直接给写好的行，页面不再重新拼。
type ContextJSON struct {
	TimelineFrom int64    `json:"timeline_from,omitempty"`
	Timeline     []string `json:"timeline"`
	Changes      []string `json:"changes"`
	Machine      string   `json:"machine,omitempty"`
	Learn        string   `json:"learn,omitempty"`
}
