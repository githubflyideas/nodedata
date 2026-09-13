package main

import "testing"

// 单位判错会直接把数字显示成另一个量级：实测 fs.avail 67 GiB 显示成 "67046.88M"、
// mem.used_pct 14.49% 显示成 "14.49 B"。这些都是页面上一眼能看见的错。
func TestUnitOf(t *testing.T) {
	for id, want := range map[string]string{
		// 百分比必须压过 mem./swap. 前缀
		"mem.used_pct": "percent", "fs.used_pct": "percent", "fs.inode_used_pct": "percent",
		"conntrack.used_pct": "percent", "disk.util": "percent", "disk.util@nvme0n1": "percent",
		"cpu.user": "percent", "psi.io.some10": "percent", "proc.cpu.mysqld": "percent",
		// 字节量：这两个都不含 "bytes" 字样
		"fs.avail": "bytes", "proc.rss.firefox": "bytes",
		"mem.available": "bytes", "swap.used": "bytes", "slab": "bytes", "disk.rbytes@sda": "bytes/s",
		// 速率
		"net.rx": "bytes/s", "net.tx@eth0": "bytes/s", "proc.io.rsync": "bytes/s",
		"udp.rcvbuf_errors": "/s", "udp.in_errors": "/s", "net.rx_drop@eth1": "/s",
		"tcp.retrans": "/s", "tcp.passive_opens": "/s", "tcp.attempt_fails": "/s",
		"udp.no_ports": "/s", "pgmajfault": "/s",
		// 延迟、计数
		"disk.await_w": "ms", "disk.await_w@sda": "ms",
		"sock.tcp_tw": "count", "tcp.estab": "count", "conntrack": "count", "procs_blocked": "count",
		"loadavg.1m": "load",
	} {
		if got := unitOf(id); got != want {
			t.Errorf("unitOf(%q) = %q, want %q", id, got, want)
		}
	}
}
