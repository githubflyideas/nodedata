// mindelta.go — "值得一提的最小变化量"。
//
// z 是尺度无关的：一台安静的机器 σ 趋近于 0，于是每秒 202 字节的流量、
// 每秒 2.8 个包、0.1 次重传全都是 6σ，整屏红。**统计上显著不等于实际上要紧。**
//
// 这些门槛不是拍脑袋，取的是"在单机排查里低于这个量就不会有人多看一眼"的水平：
// 网卡流量按 1 MB/s、包速率按 100 包/秒、内存按 128 MB、CPU 按 3 个百分点。
// 错误类（丢包、重传、UDP 溢出）门槛低得多——1 次/秒 就值得看，因为它们本该是 0。
package main

import "strings"

func minDeltaFor(id string) float64 {
	base := strings.SplitN(id, "@", 2)[0]

	// 错误类：本该是 0，出现就值得看，但也不能 0.001 就报
	switch {
	case strings.Contains(base, "_drop"), strings.Contains(base, "_errs"),
		strings.Contains(base, "_errors"), base == "tcp.retrans",
		base == "udp.no_ports", base == "tcp.attempt_fails":
		return 1 // 次/秒
	case base == "procs_blocked", base == "procs_running":
		return 2
	case base == "swap.used":
		return 32 << 20
	// 逐核：最忙那个核天然抖得厉害——"N 个核里的最大值"是极值统计，
	// 一台闲机器上它也会在 3% 和 30% 之间来回跳（一次 ls、一次 GC 就够了）。
	// 按整机的 3 个百分点算，每一次小抖动都会变成"显著偏离"。
	// 20 个百分点以下的单核变化不值得一提；真打满是从几十涨到 100，远超这个数。
	case strings.HasPrefix(base, "cpu.core_"):
		return 20
	}

	switch unitOf(id) {
	case "percent":
		return 3 // 百分点
	case "bytes":
		return 128 << 20 // 内存类：128 MB 以下的波动是缓存在动
	case "bytes/s":
		return 1 << 20 // 1 MB/s
	case "ops/s":
		return 100 // 包/秒、IOPS
	case "/s":
		return 5
	case "ms":
		return 2
	case "load":
		return 0.5
	case "count":
		return 10
	}
	return 0
}
