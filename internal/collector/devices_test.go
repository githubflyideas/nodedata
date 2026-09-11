package collector

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsWholeDisk(t *testing.T) {
	for name, want := range map[string]bool{
		"sda": true, "sda1": false, "vdb": true, "vdb2": false, "xvda": true,
		"nvme0n1": true, "nvme0n1p1": false, "nvme12n3": true, "nvme1n1p12": false,
		"mmcblk0": true, "mmcblk0p1": false, "dm-0": true, "md0": true, "md127": true,
		"loop0": false, "ram0": false, "zram0": false, "sr0": false,
	} {
		if got := IsWholeDisk(name); got != want {
			t.Errorf("IsWholeDisk(%q)=%v want %v", name, got, want)
		}
	}
}

// diskLine: major minor name rio rmerge rsect rtime wio wmerge wsect wtime inflight ioms weighted
func diskLine(name string, rio, rmerge, rsect, rtime, wio, wmerge, wsect, wtime, inflight, ioms, weighted uint64) string {
	return fmt.Sprintf("   8 0 %s %d %d %d %d %d %d %d %d %d %d %d 0 0 0 0\n",
		name, rio, rmerge, rsect, rtime, wio, wmerge, wsect, wtime, inflight, ioms, weighted)
}

func collectOnce(t *testing.T, c *Collector, root string, now time.Time) map[string]float64 {
	t.Helper()
	out, err := c.CollectGlobal(root, now)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]float64{}
	for _, s := range out {
		m[s.MetricID] = s.Value
	}
	return m
}

func TestDiskAndNetParsing(t *testing.T) {
	proc, sys := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(proc, "net"), 0o755)
	// 物理口 eth0/eth1 组成 bond0，bond0.100 是 VLAN：只有 eth0/eth1 有 device
	for _, ifc := range []string{"eth0", "eth1"} {
		os.MkdirAll(filepath.Join(sys, "class/net", ifc, "device"), 0o755)
	}
	for _, ifc := range []string{"bond0", "bond0.100"} {
		os.MkdirAll(filepath.Join(sys, "class/net", ifc), 0o755)
	}
	write := func(disk, net string) {
		os.WriteFile(filepath.Join(proc, "diskstats"), []byte(disk), 0o644)
		os.WriteFile(filepath.Join(proc, "net/dev"), []byte("Inter-| hdr\n face | hdr\n"+net), 0o644)
	}
	netLine := func(name string, rxB, rxP, rxErr, rxDrop, txB, txP, txErr, txDrop uint64) string {
		return fmt.Sprintf("%6s: %d %d %d %d 0 0 0 0 %d %d %d %d 0 0 0 0\n", name, rxB, rxP, rxErr, rxDrop, txB, txP, txErr, txDrop)
	}
	c := New(Config{ProcRoot: proc, SysRoot: sys, Interval: 5 * time.Second})
	t0 := time.Unix(1_800_000_000, 0)

	write(
		diskLine("sda", 100, 10, 800, 50, 200, 20, 1600, 400, 0, 1000, 9000)+
			diskLine("sda1", 100, 10, 800, 50, 200, 20, 1600, 400, 0, 1000, 9000)+
			diskLine("nvme0n1", 1000, 0, 8000, 100, 1000, 0, 8000, 100, 0, 2000, 3000)+
			diskLine("nvme0n1p1", 1000, 0, 8000, 100, 1000, 0, 8000, 100, 0, 2000, 3000)+
			diskLine("dm-0", 1100, 0, 8800, 150, 1200, 0, 9600, 500, 0, 2500, 9999)+
			diskLine("loop0", 5, 0, 10, 1, 0, 0, 0, 0, 0, 1, 1),
		netLine("lo", 9, 9, 0, 0, 9, 9, 0, 0)+
			netLine("eth0", 1000, 10, 0, 0, 2000, 20, 0, 0)+
			netLine("eth1", 1000, 10, 0, 0, 2000, 20, 0, 0)+
			netLine("bond0", 2000, 20, 0, 0, 4000, 40, 0, 0)+
			netLine("bond0.100", 2000, 20, 0, 0, 4000, 40, 0, 0)+
			netLine("veth12ab", 5, 5, 0, 0, 5, 5, 0, 0))
	collectOnce(t, c, proc, t0)

	// 5 秒后：sda 写 500 次、写耗时 5000ms（await 10ms）、忙 2500ms（util 0.5）、队列 7；
	//         nvme 读 5000 次、忙 4000ms（util 0.8）；dm-0 是 LVM，不能进整机汇总。
	//         eth0 收 5MB、丢 50 包；eth1 发 5MB、发送丢 10 包（旧代码的 tx_drop 会读成收包数）。
	write(
		diskLine("sda", 100, 10, 800, 50, 700, 120, 1600+8000, 5400, 7, 3500, 99999)+
			diskLine("sda1", 100, 10, 800, 50, 700, 120, 1600+8000, 5400, 7, 3500, 99999)+
			diskLine("nvme0n1", 6000, 0, 8000+40000, 600, 1000, 0, 8000, 100, 2, 6000, 99999)+
			diskLine("nvme0n1p1", 6000, 0, 48000, 600, 1000, 0, 8000, 100, 2, 6000, 99999)+
			diskLine("dm-0", 6100, 0, 48800, 650, 1700, 0, 17600, 5500, 9, 7000, 99999)+
			diskLine("loop0", 5, 0, 10, 1, 0, 0, 0, 0, 0, 1, 1),
		netLine("lo", 9, 9, 0, 0, 9, 9, 0, 0)+
			netLine("eth0", 1000+5_000_000, 10+4000, 0, 50, 2000, 20, 0, 0)+
			netLine("eth1", 1000, 10, 0, 0, 2000+5_000_000, 20+4000, 0, 10)+
			netLine("bond0", 2000+5_000_000, 20+4000, 0, 50, 4000+5_000_000, 40+4000, 0, 10)+
			netLine("bond0.100", 2000+5_000_000, 20+4000, 0, 0, 4000+5_000_000, 40+4000, 0, 0)+
			netLine("veth12ab", 5, 5, 0, 0, 5, 5, 0, 0))
	m := collectOnce(t, c, proc, t0.Add(5*time.Second))

	near := func(id string, want float64) {
		t.Helper()
		got, ok := m[id]
		if !ok {
			t.Errorf("%s missing", id)
			return
		}
		if math.Abs(got-want) > 1e-6*math.Max(1, math.Abs(want)) {
			t.Errorf("%s = %v, want %v", id, got, want)
		}
	}
	near("disk.util@sda", 50)
	near("disk.util@nvme0n1", 80) // 旧代码：nvme0n1 被当分区跳过
	near("disk.await_w@sda", 10)
	near("disk.wiops@sda", 100)
	near("disk.wbytes@sda", 8000*512/5.0)
	near("disk.inflight@sda", 7) // 旧代码：这里是累计 IO 毫秒数
	near("disk.util", 80)        // 最忙那块，不是相加
	near("disk.riops", 1000)     // 只算 sda+nvme0n1，dm-0 不重复计入
	near("disk.wiops", 100)
	near("disk.merged", 100.0/5)
	near("disk.inflight", 9)
	near("net.rx", 1_000_000) // eth0+eth1，bond0 与 VLAN 不重复
	near("net.tx", 1_000_000)
	near("net.rx_drop", 10)
	near("net.tx_drop", 2) // 旧代码：这里是收包数
	near("net.rx_drop@eth0", 10)
	near("net.tx_drop@eth1", 2)
	for id := range m {
		for _, bad := range []string{"@sda1", "@nvme0n1p1", "@loop0", "@lo", "@veth12ab"} {
			if len(id) >= len(bad) && id[len(id)-len(bad):] == bad {
				t.Errorf("unexpected series %s", id)
			}
		}
	}
}
