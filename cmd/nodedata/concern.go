// concern.go — "这个读数有没有可能是瓶颈"：USE 判定的绝对那一半（v5.25）。
//
// z 只回答"跟平时像不像"，回答不了"有没有问题"。Brendan Gregg 的 USE 方法里，
// 资源只有三种情况算问题：利用率接近打满、出现排队（饱和）、出现错误。
// 一台 2 核机器运行队列从 8 掉到 1、CPU 从 1.1% 掉到 0.5%，z 可以是 -3.4，
// 但它是变闲了——报"偏离"是在骂人。
//
// 所以 USE 的"偏离"必须同时满足三条：
//  1. 往坏的方向变（方向来自指标注册表）；
//  2. 变化量够大（|z| ≥ 3；z 本身已经过最小变化量门槛，不到 minDelta 的变化 z 记 0）；
//  3. 读数本身落在"可能是瓶颈"的区间——就是这个文件，而且过去 60 秒一直在里面。
//
// 只满足 1、2 或者往好的方向变的，不改变状态，只标"比平时高/低"。
// 这张表里的每个数都是一个判断，写清楚来历，改的时候改这里。
package main

import (
	"github.com/githubflyideas/nodedata/internal/collector"
	"github.com/githubflyideas/nodedata/internal/metrics"
)

const concernMiB = 1 << 20

// concernCtx 是判断"是不是瓶颈"时需要的机器背景。
type concernCtx struct {
	cores    int
	diskKind string                          // 最忙那块盘的类型
	last     func(id string) (float64, bool) // 取别的指标的当前值
}

// inConcern 判断读数 v 是否落在"可能是瓶颈"的区间。
// 返回 known=false 表示这个指标没有定义区间：它的偏离只作为"有变化"展示，不改变状态——
// 宁可不报，也不拿一个没想清楚的指标去点亮墙上的方块。
func inConcern(id string, v float64, c concernCtx) (in, known bool) {
	cores := float64(c.cores)
	if cores <= 0 {
		cores = 1
	}
	switch metrics.Base(id) {
	// ── CPU ──
	case "cpu.busy_pct": // 整机忙碌 0–100。一半以上才谈得上 CPU 不够用
		return v >= 50, true
	case "cpu.core_top1", "cpu.core_top2": // 单核。60% 以上就要看（单线程服务钉在一个核上时，整机数字看不出来）
		return v >= 60, true
	case "cpu.core_softirq_max": // 一个核 30% 花在软中断上，网卡中断/RPS 已经在挤这个核
		return v >= 30, true
	case "cpu.user", "cpu.sys": // 各核之和（100% = 1 核）。占掉半台机器
		return v >= 50*cores, true
	case "cpu.softirq":
		return v >= 30, true
	case "psi.cpu.some10": // 过去 10 秒有 5% 的时间有任务在等 CPU
		return v >= 5, true
	case "procs_running", "loadavg.1m": // 饱和的定义：可运行的比核多
		return v > cores, true
	case "cpu.steal": // 虚拟机被宿主机拿走 5% 以上
		return v >= 5, true

	// ── 内存 ──
	case "mem.used_pct":
		return v >= 80, true
	case "mem.available": // 字节数，跟已用比例是同一件事：看已用比例
		if u, ok := c.last("mem.used_pct"); ok {
			return u >= 80, true
		}
		return false, false
	case "swap.out": // 在往外换页就是内存饱和（Gregg）；1 MiB/s 以下的零星换出不算
		return v >= concernMiB, true
	case "swap.in":
		return v >= concernMiB, true
	case "psi.mem.some10":
		return v >= 5, true
	case "pgmajfault": // 每秒 100 次以上要去读盘的缺页
		return v >= 100, true
	case "mem.dirty", "mem.writeback": // 脏页/回写占到总内存 10%（内核默认 dirty_background_ratio 就是 10%）
		return v >= 0.1*totalMem(c), totalMem(c) > 0

	// ── 磁盘 ──
	case "psi.io.some10":
		return v >= 5, true
	case "disk.await_w", "disk.await_r": // 延迟按盘的类型：机械盘 50ms、SSD 10ms、虚拟盘/不知道 20ms
		switch c.diskKind {
		case collector.DiskHDD:
			return v >= 50, true
		case collector.DiskSSD:
			return v >= 10, true
		}
		return v >= 20, true
	case "disk.util": // 只对机械盘有意义；SSD/虚拟盘 util 满不等于盘满（iostat 手册）
		if c.diskKind == collector.DiskHDD {
			return v >= 60, true
		}
		return false, true
	case "disk.inflight": // 在途 I/O：机械盘 4 个就在排队，SSD 32 个
		if c.diskKind == collector.DiskHDD {
			return v >= 4, true
		}
		return v >= 32, true
	case "procs_blocked": // 两个以上进程卡在不可中断睡眠（多半在等 IO）
		return v >= 2, true
	case "fs.avail": // 容量交给 L0 的绝对判定，这里不另立一条线
		return false, true

	// ── 网络 ──
	case "net.rx", "net.tx": // 吞吐只有跟链路速率比才知道满没满，这里拿不到速率：只展示
		return false, true
	case "net.softnet_squeeze", "net.softnet_drop", "net.rx_drop", "net.tx_drop", "net.rx_errs", "net.tx_errs":
		return v >= 1, true // 错误：每秒 1 个就是有
	case "tcp.retrans": // 繁忙机器每秒几个重传是常态
		return v >= 10, true
	}
	return false, false
}

// totalMem 由"可用"和"已用比例"反推总内存；拿不到返回 0。
func totalMem(c concernCtx) float64 {
	a, ok1 := c.last("mem.available")
	u, ok2 := c.last("mem.used_pct")
	if !ok1 || !ok2 || u >= 100 {
		return 0
	}
	return a / (1 - u/100)
}

// hardLine 是"绝对线"：读数到了这里，不管历史、不管 z 有没有就绪，持续 60 秒就算偏离。
//
// 瓶颈区间（inConcern）回答"有没有可能是瓶颈"，还要 z 来证明"跟平时不一样"；
// 但有些读数本身就是结论：一个核被钉死在 100%、一半时间有任务在等 CPU、每秒在往外换 10MB 内存、
// 网卡在丢包——这时候"平时也这样"不是安慰，是说它平时就有问题。
// 这些线定得很高，正常忙碌的机器碰不到；碰到了就是 USE 意义上的饱和或错误。
// 注意这里跟 L0 不重叠：L0 管的是配置和容量类的硬错误（只读挂载、分区满、OOM），这里管运行时的饱和。
func hardLine(id string, v float64, c concernCtx) (in bool, why string) {
	cores := float64(c.cores)
	if cores <= 0 {
		cores = 1
	}
	switch metrics.Base(id) {
	case "cpu.core_top1":
		return v >= 95, "单核打满"
	case "psi.cpu.some10":
		return v >= 20, "两成时间有任务在等 CPU"
	case "procs_running":
		return v > 2*cores, "排队的任务超过核数两倍"
	case "psi.mem.some10":
		return v >= 10, "一成时间在等内存"
	case "swap.out":
		return v >= 10*concernMiB, "每秒换出 10MB 以上"
	case "psi.io.some10":
		return v >= 20, "两成时间在等 IO"
	case "net.softnet_drop", "net.rx_drop", "net.tx_drop", "net.rx_errs", "net.tx_errs":
		return v >= 1, "持续丢包/错包"
	}
	return false, ""
}
