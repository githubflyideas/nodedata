package main

import "testing"

// 实测截图里的一屏红，几乎全是"统计上显著、实际上不值一提"：
// 每秒 202 字节的流量、2.8 个包、0.1 次重传、7 个连接，全都是 6σ。
func TestMinDeltaFloors(t *testing.T) {
	cases := []struct {
		id        string
		delta     float64
		wantBelow bool // true = 该被当成"变化太小"
		why       string
	}{
		{"net.tx", 200, true, "每秒 200 字节的流量变化"},
		{"net.tx", 5 << 20, false, "每秒 5MB 的流量变化"},
		{"net.tx_pps", 2.8, true, "每秒 2.8 个包"},
		{"net.tx_pps", 5000, false, "每秒 5000 个包"},
		{"tcp.estab", 7, true, "7 个连接"},
		{"tcp.estab", 500, false, "500 个连接"},
		{"mem.available", 50 << 20, true, "50MB 的内存波动（缓存在动）"},
		{"mem.available", 2 << 30, false, "2GB 的内存变化"},
		{"mem.used_pct", 1.5, true, "1.5 个百分点"},
		{"mem.used_pct", 20, false, "20 个百分点"},
		{"cpu.user", 2, true, "2 个百分点的 CPU"},
		{"cpu.user", 60, false, "60 个百分点的 CPU"},
		{"disk.await_w", 0.5, true, "0.5ms 的延迟变化"},
		{"disk.await_w", 50, false, "50ms 的延迟变化"},
		// 错误类门槛要低：它们本该是 0
		{"tcp.retrans", 0.1, true, "每秒 0.1 次重传"},
		{"tcp.retrans", 20, false, "每秒 20 次重传"},
		{"net.rx_drop@eth0", 3, false, "每秒 3 个丢包（逐网卡也适用）"},
		{"udp.rcvbuf_errors", 5, false, "每秒 5 次 UDP 缓冲区溢出"},
	}
	for _, c := range cases {
		floor := minDeltaFor(c.id)
		below := floor > 0 && c.delta < floor
		if below != c.wantBelow {
			t.Errorf("%s（%s）：门槛 %.0f，delta %.0f → 判为%s，期望%s",
				c.id, c.why, floor, c.delta,
				map[bool]string{true: "太小", false: "值得报"}[below],
				map[bool]string{true: "太小", false: "值得报"}[c.wantBelow])
		}
	}
}
