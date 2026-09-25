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

// textReport 生成简报。所有入参都允许为零值，缺什么就少哪一段。
func textReport(host string, rows []UseRow, chain *diagnosis.Chain, now time.Time) string {
	var b strings.Builder

	// 头：模型没有上下文，主机名和时刻必须在第一行，否则它无从判断
	// 这是哪台机器、数据有多新。
	worst := worstUse(rows)
	fmt.Fprintf(&b, "NODEDATA host=%s at=%s state=%s\n",
		nonEmpty(host, "unknown"), now.Format(time.RFC3339), useState(worst))
	b.WriteString("基线=最近 24h 同指标中位数；z=偏离档位数，|z|≥3 才列出\n\n")

	// 严重的排前面。被截断时先丢掉的应该是"正常"那几行。
	ord := make([]UseRow, len(rows))
	copy(ord, rows)
	sort.SliceStable(ord, func(i, j int) bool { return ord[i].Level > ord[j].Level })

	for _, r := range ord {
		fmt.Fprintf(&b, "%s %s\n", r.Resource, r.State)
		for _, c := range r.Checks {
			fmt.Fprintf(&b, "  [%s] %s %s\n", c.ID, c.Name, c.Message)
		}
		for _, f := range r.Facts {
			b.WriteString("  " + factLine(f) + "\n")
		}
		if len(r.Movers) > 0 {
			b.WriteString("  谁干的: " + moversLine(r.Movers) + "\n")
		}
	}

	// 线索单独成段并标"测试中"：放进上面的资源行里，模型会当成判定。
	if chain != nil && len(chain.Items) > 0 {
		var clues []string
		for _, it := range chain.Items {
			if it.Class == "" {
				continue // 只有归因产生的条目算线索，L0 已经在上面了
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
	for _, r := range rows {
		for _, s := range r.Blind {
			blind = append(blind, r.Resource+" "+s)
		}
	}
	if len(blind) > 0 {
		b.WriteString("\n看不到(以下方面本报告没有依据，不要据此下结论):\n")
		for _, s := range blind {
			b.WriteString("  " + s + "\n")
		}
	}
	return b.String()
}

// factLine 把一条观测写成一行：`写等待 41ms (平时 2ms, z=7.2)`。
func factLine(f UseFact) string {
	s := f.Label + " " + fmtUnit(f.Value, f.Unit)
	var extra []string
	if f.Base != nil {
		extra = append(extra, "平时 "+fmtUnit(*f.Base, f.Unit))
	}
	if math.Abs(f.Z) >= useZThreshold {
		extra = append(extra, "z="+strconv.FormatFloat(f.Z, 'f', 1, 64))
	}
	if len(extra) > 0 {
		s += " (" + strings.Join(extra, ", ") + ")"
	}
	if f.Note != "" {
		s += " —— " + f.Note
	}
	return s
}

// moversLine 把归因压成一行：`rsync 620MB/s (1h前 1MB/s), mysqld 40MB/s`。
func moversLine(ms []cmpMover) string {
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		s := m.Name + " " + fmtUnit(m.Now, m.Unit)
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
