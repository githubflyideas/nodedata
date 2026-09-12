// health.go — /health.txt：一行文本，给已有监控系统 grep。
//
// 参考 host-dns-perf 的做法。这是"接进现有告警体系"的最短路径：
// 运维不需要为 nodedata 学一套 JSON 结构，一条 curl + grep 就能接上。
//
//	NODEDATA host=jp02-dns-01 status=WARN cpu=182% load=9.1 mem_avail=1.2GiB disk=96% \
//	  culprit=fl-io/236 reason="IO 劣化：disk.await_w 上升 6.0σ；根分区空间 已用 96.2%" ts=2026-09-12T03:10:51Z
//
// 三条约定，抄自那份文档、实践中确实重要：
//   - 关键字 NODEDATA 放行首，grep 不会误匹配别的日志；
//   - 主机名在同一行，告警时不用再查是哪台机器；
//   - 异常时带 reason=，值班的人看告警正文就知道发生了什么。
//
// status 取 L0 绝对判定与 L4 归因结论中较严重者：
//
//	DOWN   L0 有 fail（硬阈值越界，如根分区满、OOM）
//	WARN   L0 有 warn，或 L4 给出了带责任方的结论
//	NORMAL 其余
package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/githubflyideas/nodedata/internal/diagnosis"
)

const healthMaxReasons = 3

// healthLine 生成 /health.txt 的内容（单行，以 \n 结尾）。
func healthLine(host string, l0 []diagnosis.L0Category, chain *diagnosis.Chain, vals map[string]float64, now time.Time) string {
	status, reasons := "NORMAL", []string{}
	{
		for _, cat := range l0 {
			for _, c := range cat.Checks {
				if c.Level >= 2 {
					status = "DOWN"
					reasons = append(reasons, c.Name+" "+c.Message)
				} else if c.Level == 1 && status == "NORMAL" {
					status = "WARN"
				}
			}
		}
	}
	// L4 的结论带责任方，比 L0 的阈值更有信息量：culprit= 单独给出，方便直接派单。
	culprit := ""
	if chain != nil {
		for _, it := range chain.Items {
			if it.Class == "" {
				continue
			}
			if status == "NORMAL" {
				status = "WARN"
			}
			reasons = append(reasons, it.Title)
			if culprit == "" {
				for _, c := range it.Culprits {
					if c.PID > 0 {
						culprit = c.Name + "/" + strconv.Itoa(c.PID)
						if c.Service != "" && c.Service != c.Name {
							culprit = c.Service + "(" + c.Name + ")/" + strconv.Itoa(c.PID)
						}
						break
					}
				}
			}
		}
	}

	var b strings.Builder
	b.WriteString("NODEDATA host=" + sanitizeHealth(host) + " status=" + status)
	for _, kv := range []struct {
		k string
		v string
	}{
		{"cpu", healthPct(vals, "cpu.user", "cpu.sys", "cpu.softirq")},
		{"load", healthNum(vals, "loadavg.1m", 2)},
		{"mem_avail", healthBytes(vals, "mem.available")},
		{"disk_util", healthNum(vals, "disk.util", 0)},
		{"net_drop", healthNum(vals, "net.rx_drop", 0)},
	} {
		if kv.v != "" {
			b.WriteString(" " + kv.k + "=" + kv.v)
		}
	}
	if culprit != "" {
		b.WriteString(" culprit=" + sanitizeHealth(culprit))
	}
	if len(reasons) > 0 {
		if len(reasons) > healthMaxReasons {
			reasons = append(reasons[:healthMaxReasons], fmt.Sprintf("等 %d 项", len(reasons)))
		}
		b.WriteString(` reason="` + sanitizeHealth(strings.Join(reasons, "；")) + `"`)
	}
	b.WriteString(" ts=" + now.UTC().Format(time.RFC3339) + "\n")
	return b.String()
}

// sanitizeHealth 去掉会破坏"一行、可 grep"这个约定的内容。
//
// 除了换行与引号，还必须中和 NODEDATA 与 status= 这两个记号：自由文本里含有进程名，
// 而进程名是攻击者可控的。一个叫 `NODEDATA status=NORMAL` 的进程会被写进 reason=，
// 让监控侧的 grep 'status=NORMAL' 在真的告警时反而匹配成功 —— 告警就这么被吃掉了。
func sanitizeHealth(s string) string {
	s = strings.NewReplacer(
		"\n", " ", "\r", " ", "\t", " ", `"`, "'",
		"NODEDATA", "nodedata", "status=", "status:",
	).Replace(s)
	if len(s) > 300 {
		s = s[:300]
	}
	return strings.TrimSpace(s)
}

func healthNum(v map[string]float64, id string, prec int) string {
	x, ok := v[id]
	if !ok || math.IsNaN(x) || math.IsInf(x, 0) {
		return ""
	}
	return strconv.FormatFloat(x, 'f', prec, 64)
}

func healthPct(v map[string]float64, ids ...string) string {
	sum, any := 0.0, false
	for _, id := range ids {
		if x, ok := v[id]; ok {
			sum, any = sum+x, true
		}
	}
	if !any {
		return ""
	}
	return strconv.FormatFloat(sum, 'f', 0, 64) + "%"
}

func healthBytes(v map[string]float64, id string) string {
	x, ok := v[id]
	if !ok {
		return ""
	}
	u := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for x >= 1024 && i < len(u)-1 {
		x /= 1024
		i++
	}
	return strconv.FormatFloat(x, 'f', 1, 64) + u[i]
}

// healthValues 取各指标最后一个点，供 health 行使用。
func (b *HeatmapBuilder) healthValues(now time.Time) map[string]float64 {
	out := map[string]float64{}
	for _, id := range []string{"cpu.user", "cpu.sys", "cpu.softirq", "loadavg.1m", "mem.available", "disk.util", "net.rx_drop"} {
		if p, ok := b.series.Last(id); ok && now.Sub(p.TS) <= 2*time.Minute {
			out[id] = p.V
		}
	}
	return out
}
