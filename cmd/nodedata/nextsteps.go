// nextsteps.go — 下一步该跑什么命令。
//
// nodedata 停在进程级：它能告诉你"是这台机器、是磁盘、是 rsync"，再往下（哪个函数、
// 哪个线程、哪个系统调用）是 perf、strace、jstack、XRay 的事。这里把观测翻译成
// 下一条命令，交给人去跑。**nodedata 自己永远不执行它们。**
//
// 三条规矩：
//   - **只填 PID，绝不填进程名。** 进程名是任何人都能随便起的（prctl 一下就行），
//     拼进 shell 命令就是注入——跟 health.txt 那次是同一类坑。进程名只用来"选哪一套命令"。
//     网卡名来自内核，但也按白名单校验后才用。
//   - **只给诊断，不给处置。** ionice、renice、kill 这类会改变系统的，要人自己决定。
//   - **标明开销。** "我们自己不能成为性能卡点"对人手里的命令同样适用：
//     strace 能让目标进程慢 10–100 倍，perf record -g 也不便宜，必须写在旁边。
package main

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/githubflyideas/nodedata/internal/diagnosis"
)

// NextCmd 是一条建议命令。
type NextCmd struct {
	Cmd  string `json:"cmd"`
	Cost string `json:"cost"` // 零开销 / 低开销 / 会拖慢目标进程
	Why  string `json:"why,omitempty"`
}

const (
	costZero  = "零开销"
	costLow   = "低开销"
	costHeavy = "会拖慢目标进程"
)

// attachPIDs 给"谁干的"补上 PID 和父进程：按名字在进程快照里找最忙的那个。
func attachPIDs(ms []cmpMover, procs []diagnosis.Proc) {
	for i := range ms {
		var best *diagnosis.Proc
		score := func(p *diagnosis.Proc) float64 {
			switch {
			case strings.HasPrefix(ms[i].Metric, "proc.io."):
				return p.ReadBps + p.WriteBps
			case strings.HasPrefix(ms[i].Metric, "proc.rss."):
				return float64(p.RSS)
			}
			return p.CPU
		}
		for j := range procs {
			p := &procs[j]
			if p.Key != ms[i].Name && p.Comm != ms[i].Name {
				continue
			}
			if best == nil || score(p) > score(best) {
				best = p
			}
		}
		if best != nil {
			ms[i].PID, ms[i].Parent = best.PID, best.Parent
		}
	}
}

// nicName 只放行正常的网卡名。
var nicName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,15}$`)

// nextSteps 按资源和责任进程给出命令。pid 取"谁干的"第一名；没有就给整机层面的命令。
func nextSteps(resource string, movers []cmpMover, nic string) []NextCmd {
	pid, name := 0, ""
	if len(movers) > 0 && movers[0].PID > 0 {
		pid, name = movers[0].PID, movers[0].Name
	}
	p := strconv.Itoa(pid)
	isJava := name == "java" || strings.HasPrefix(name, "java")
	var out []NextCmd
	add := func(cmd, cost, why string) { out = append(out, NextCmd{Cmd: cmd, Cost: cost, Why: why}) }

	switch resource {
	case "CPU":
		if pid > 0 {
			add("top -H -p "+p+" -b -n1 | head -20", costZero, "看是这个进程里的哪几个线程在烧")
			if isJava {
				add("jstack "+p+" > /tmp/jstack."+p+".txt", costLow, "JVM 会短暂停顿；配合上一条的线程号查栈")
			}
			add("perf top -p "+p, costLow, "看热点函数")
			add("perf record -g -p "+p+" -- sleep 10", costHeavy, "采 10 秒调用栈，之后 perf report 看")
			add("timeout 10 strace -c -f -p "+p, costHeavy, "系统调用统计；strace 能让目标慢 10–100 倍，只在内核态占比高时用")
		} else {
			add("mpstat -P ALL 1 3", costZero, "逐核看用户态/内核态/软中断（需装 sysstat）")
			add("top -b -n1 | head -20", costZero, "")
		}
	case "内存":
		if pid > 0 {
			add("grep -E 'VmRSS|VmSwap|Threads' /proc/"+p+"/status", costZero, "")
			if isJava {
				add("jstat -gcutil "+p+" 1000 5", costLow, "看 GC 是不是在频繁回收")
			}
			add("pmap -x "+p+" | tail -1", costLow, "")
		}
		add("grep -E 'MemAvailable|Dirty|Writeback|SwapFree' /proc/meminfo", costZero, "")
		add("vmstat 1 5", costZero, "si/so 列非零 = 正在换入换出")
	case "磁盘":
		if pid > 0 {
			add("cat /proc/"+p+"/io", costZero, "")
			add("ls -l /proc/"+p+"/fd | head -30", costZero, "看它在读写哪些文件")
			add("pidstat -d -p "+p+" 1 5", costZero, "需装 sysstat")
		}
		add("iostat -x 1 3", costZero, "逐盘的等待、队列深度（需装 sysstat）")
		add("cat /proc/pressure/io", costZero, "")
	case "网络":
		if nic != "" && nicName.MatchString(nic) {
			add("ethtool -S "+nic+" | grep -iE 'drop|miss|fifo|err'", costZero, "网卡驱动的私有计数器，/proc 里没有")
			add("ethtool -g "+nic, costZero, "收包环有多大")
		}
		add("nstat -az TcpRetransSegs TcpExtListenOverflows TcpExtListenDrops", costZero, "")
		add("ss -ti state established | head -40", costLow, "各连接的 RTT 和重传")
	case "系统":
		add("dmesg -T | tail -30", costZero, "")
	}
	return out
}

// busiestNIC 取最该看的那块网卡：有丢包的优先，否则流量最大的。
func (d *Diagnoser) busiestNIC(now time.Time) string {
	if d.series == nil {
		return ""
	}
	best, bestDrop, bestRx := "", -1.0, -1.0
	for _, id := range d.series.MetricIDs() {
		var nic string
		var isDrop bool
		switch {
		case strings.HasPrefix(id, "net.rx_drop@"):
			nic, isDrop = strings.TrimPrefix(id, "net.rx_drop@"), true
		case strings.HasPrefix(id, "net.rx@"):
			nic = strings.TrimPrefix(id, "net.rx@")
		default:
			continue
		}
		p, ok := d.series.Last(id)
		if !ok || now.Sub(p.TS) > 2*time.Minute || !nicName.MatchString(nic) {
			continue
		}
		if isDrop {
			if p.V > bestDrop && p.V > 0 {
				best, bestDrop = nic, p.V
			}
		} else if bestDrop <= 0 && p.V > bestRx {
			best, bestRx = nic, p.V
		}
	}
	return best
}
