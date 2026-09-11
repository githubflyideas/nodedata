// devices.go — /proc/diskstats 与 /proc/net/dev：按设备 / 按接口出序列，整机聚合不重复计数。
//
// v3.0.10 及之前这两处的问题（全部是数值错误，不是风格问题）：
//   - diskstats 字段错位一格：inflight 读成"累计 IO 毫秒数"（单调递增计数器），
//     util 读成加权时间，且 prev.ioutil 从不保存 —— disk.util 实际上几乎从不输出；
//   - 整机 util 是各盘相加，两块盘各 60% 就超过 1 被丢弃；
//   - "名字以数字结尾就是分区"：nvme0n1、dm-0、md0 全被当分区跳过，NVMe 服务器没有任何磁盘指标；
//   - merged 用本次合并数减去上次的 IO 数；
//   - net.tx_drop 取的是 nums[11%10]=nums[1]，即接收包数；
//   - 只读前 8 个接口，bond、其成员口、VLAN 全部相加，绑定网卡的物理机流量翻倍甚至三倍。
//
// 现在：
//   - 磁盘：只看整盘（sdX/vdX/xvdX/hdX 以数字结尾才是分区；nvme/mmcblk 以 pN 结尾才是分区；
//     dm-*、md* 算整盘），从未有过 IO 的设备不出序列；
//     整机 IOPS/吞吐/await 只汇总"叶子"设备（排除 dm-*、md*，否则 LVM/RAID 上层与底层重复），
//     整机 util 取最忙那块盘；util 统一为 0–100（与 cpu.*、psi.* 同单位，旧版是 0–1 却标成百分比）；
//   - 网络：整机聚合只算有 /sys/class/net/<if>/device 的物理/虚拟网卡（bond、VLAN、bridge 不计入，
//     它们的流量已在成员口上）；每个接口单独出序列；容器/隧道类虚拟口跳过。
package collector

import (
	"bytes"
	"os"
	"strings"
	"time"
	"unsafe"
)

const (
	maxDisks  = 64
	maxIfaces = 32
)

type diskPrev struct {
	rio, rmerge, rsect, rtime, wio, wmerge, wsect, wtime, ioms uint64
}

type netPrev struct {
	rxB, rxP, rxErr, rxDrop, txB, txP, txErr, txDrop uint64
}

// 每个设备的序列 ID 只在首次见到时拼一次。
type diskIDs struct{ riops, wiops, rbytes, wbytes, awaitR, awaitW, util, inflight string }
type netIDs struct{ rx, tx, rxDrop, txDrop, rxErrs, txErrs string }

// IsWholeDisk 判断 /proc/diskstats 里的名字是否是整盘（而非分区或伪设备）。
func IsWholeDisk(name string) bool {
	for _, p := range []string{"loop", "ram", "zram", "sr", "fd", "nbd"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	if strings.HasPrefix(name, "dm-") || strings.HasPrefix(name, "md") {
		return true
	}
	if strings.HasPrefix(name, "nvme") || strings.HasPrefix(name, "mmcblk") {
		// nvme0n1p2 / mmcblk0p1 是分区：末尾是 p+数字
		i := len(name)
		for i > 0 && name[i-1] >= '0' && name[i-1] <= '9' {
			i--
		}
		return !(i < len(name) && i > 0 && name[i-1] == 'p')
	}
	last := name[len(name)-1]
	return !(last >= '0' && last <= '9')
}

func isStackedDisk(name string) bool {
	return strings.HasPrefix(name, "dm-") || strings.HasPrefix(name, "md")
}

// skipIface：回环、容器/隧道类虚拟口不出序列。
func skipIface(name string) bool {
	if name == "lo" {
		return true
	}
	for _, p := range []string{"veth", "docker", "br-", "virbr", "cni", "flannel", "cali", "vxlan",
		"tunl", "gre", "gretap", "erspan", "sit", "ip6tnl", "ip6gre", "ip_vti", "ip6_vti",
		"ifb", "dummy", "kube", "nodelocaldns", "tap", "vnet", "tun"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (c *Collector) internDev(b []byte) string {
	//nolint:gosec
	if s, ok := c.devNames[*(*string)(unsafe.Pointer(&b))]; ok {
		return s
	}
	s := string(b)
	c.devNames[s] = s
	return s
}

func udiff(a, b uint64) float64 {
	if a < b {
		return 0 // 计数器回绕 / 设备重置
	}
	return float64(a - b)
}

func (c *Collector) parseDiskstats(data []byte, now time.Time, dt float64, hasPrev bool, out *[]Sample) {
	var (
		aRio, aWio, aRt, aWt, aRsect, aWsect, aMerged, aInflight, maxUtil float64
		nLeaf, nDisks                                                     int
	)
	for len(data) > 0 {
		var line []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			line, data = data, nil
		}
		f := c.splitFieldsBuf(line)
		if len(f) < 14 {
			continue
		}
		name := c.internDev(f[2])
		if !IsWholeDisk(name) {
			continue
		}
		var n [11]uint64 // 字段 4..14
		ok := true
		for i := range n {
			v, err := parseUint64(f[3+i])
			if err != nil {
				ok = false
				break
			}
			n[i] = v
		}
		if !ok || (n[0] == 0 && n[4] == 0) { // 从未有过 IO
			continue
		}
		if nDisks++; nDisks > maxDisks {
			break
		}
		cur := diskPrev{rio: n[0], rmerge: n[1], rsect: n[2], rtime: n[3],
			wio: n[4], wmerge: n[5], wsect: n[6], wtime: n[7], ioms: n[9]}
		inflight := float64(n[8])
		prev, seen := c.prevDisks[name]
		c.prevDisks[name] = cur
		leaf := !isStackedDisk(name)
		if leaf {
			aInflight += inflight
		}
		if !hasPrev || !seen || dt <= 0 {
			continue
		}
		drio, dwio := udiff(cur.rio, prev.rio), udiff(cur.wio, prev.wio)
		util := udiff(cur.ioms, prev.ioms) / (dt * 10) // 忙碌毫秒 / 经过毫秒 × 100
		if util > 100 {
			util = 100 // 采样边界抖动
		}
		ids := c.diskID(name)
		*out = append(*out,
			Sample{MetricID: ids.riops, TS: now, Value: drio / dt},
			Sample{MetricID: ids.wiops, TS: now, Value: dwio / dt},
			Sample{MetricID: ids.rbytes, TS: now, Value: udiff(cur.rsect, prev.rsect) * 512 / dt},
			Sample{MetricID: ids.wbytes, TS: now, Value: udiff(cur.wsect, prev.wsect) * 512 / dt},
			Sample{MetricID: ids.util, TS: now, Value: util},
			Sample{MetricID: ids.inflight, TS: now, Value: inflight})
		if drio > 0 {
			*out = append(*out, Sample{MetricID: ids.awaitR, TS: now, Value: udiff(cur.rtime, prev.rtime) / drio})
		}
		if dwio > 0 {
			*out = append(*out, Sample{MetricID: ids.awaitW, TS: now, Value: udiff(cur.wtime, prev.wtime) / dwio})
		}
		if leaf {
			nLeaf++
			aRio += drio
			aWio += dwio
			aRt += udiff(cur.rtime, prev.rtime)
			aWt += udiff(cur.wtime, prev.wtime)
			aRsect += udiff(cur.rsect, prev.rsect)
			aWsect += udiff(cur.wsect, prev.wsect)
			aMerged += udiff(cur.rmerge, prev.rmerge) + udiff(cur.wmerge, prev.wmerge)
			if util > maxUtil {
				maxUtil = util
			}
		}
	}
	if !hasPrev || nLeaf == 0 || dt <= 0 {
		return
	}
	*out = append(*out,
		Sample{MetricID: "disk.riops", TS: now, Value: aRio / dt},
		Sample{MetricID: "disk.wiops", TS: now, Value: aWio / dt},
		Sample{MetricID: "disk.rbytes", TS: now, Value: aRsect * 512 / dt},
		Sample{MetricID: "disk.wbytes", TS: now, Value: aWsect * 512 / dt},
		Sample{MetricID: "disk.merged", TS: now, Value: aMerged / dt},
		Sample{MetricID: "disk.inflight", TS: now, Value: aInflight},
		Sample{MetricID: "disk.util", TS: now, Value: maxUtil}) // 最忙那块盘
	if aRio > 0 {
		*out = append(*out, Sample{MetricID: "disk.await_r", TS: now, Value: aRt / aRio})
	}
	if aWio > 0 {
		*out = append(*out, Sample{MetricID: "disk.await_w", TS: now, Value: aWt / aWio})
	}
}

func (c *Collector) diskID(name string) *diskIDs {
	if ids, ok := c.diskIDMap[name]; ok {
		return ids
	}
	ids := &diskIDs{
		riops: "disk.riops@" + name, wiops: "disk.wiops@" + name,
		rbytes: "disk.rbytes@" + name, wbytes: "disk.wbytes@" + name,
		awaitR: "disk.await_r@" + name, awaitW: "disk.await_w@" + name,
		util: "disk.util@" + name, inflight: "disk.inflight@" + name,
	}
	c.diskIDMap[name] = ids
	return ids
}

func (c *Collector) netID(name string) *netIDs {
	if ids, ok := c.netIDMap[name]; ok {
		return ids
	}
	ids := &netIDs{rx: "net.rx@" + name, tx: "net.tx@" + name,
		rxDrop: "net.rx_drop@" + name, txDrop: "net.tx_drop@" + name,
		rxErrs: "net.rx_errs@" + name, txErrs: "net.tx_errs@" + name}
	c.netIDMap[name] = ids
	return ids
}

// ifaceIsPhysical：/sys/class/net/<if>/device 存在 = 真实网卡或 virtio 网卡。
// 结果缓存 60 秒（热插拔、bond 重建）。sysfs 不可用时返回 (false, false)。
func (c *Collector) ifaceIsPhysical(name string, now time.Time) (phys, known bool) {
	if now.Sub(c.ifaceClassAt) > time.Minute {
		c.ifaceClass = make(map[string]bool, 16)
		c.ifaceClassAt = now
		_, err := os.Stat(c.cfg.SysRoot + "/class/net")
		c.sysNetOK = err == nil
	}
	if !c.sysNetOK {
		return false, false
	}
	if v, ok := c.ifaceClass[name]; ok {
		return v, true
	}
	_, err := os.Stat(c.cfg.SysRoot + "/class/net/" + name + "/device")
	c.ifaceClass[name] = err == nil
	return err == nil, true
}

func (c *Collector) parseNetDev(data []byte, now time.Time, dt float64, hasPrev bool, out *[]Sample) {
	var phys, all [8]float64 // rxB rxP rxErr rxDrop txB txP txErr txDrop
	nPhys, nAll, lineNo := 0, 0, 0
	for len(data) > 0 {
		var line []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			line, data = data, nil
		}
		if lineNo++; lineNo <= 2 {
			continue
		}
		k, rest, ok := bytes.Cut(line, []byte(":"))
		if !ok {
			continue
		}
		name := c.internDev(bytes.TrimSpace(k))
		if skipIface(name) {
			continue
		}
		f := c.splitFieldsBuf(rest)
		if len(f) < 12 {
			continue
		}
		var v [8]uint64
		good := true
		for i, idx := range [8]int{0, 1, 2, 3, 8, 9, 10, 11} {
			x, err := parseUint64(f[idx])
			if err != nil {
				good = false
				break
			}
			v[i] = x
		}
		if !good || (v[0] == 0 && v[4] == 0) { // 从未收发过
			continue
		}
		if nAll >= maxIfaces {
			break
		}
		cur := netPrev{v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7]}
		prev, seen := c.prevNets[name]
		c.prevNets[name] = cur
		if !hasPrev || !seen || dt <= 0 {
			continue
		}
		d := [8]float64{udiff(cur.rxB, prev.rxB), udiff(cur.rxP, prev.rxP), udiff(cur.rxErr, prev.rxErr),
			udiff(cur.rxDrop, prev.rxDrop), udiff(cur.txB, prev.txB), udiff(cur.txP, prev.txP),
			udiff(cur.txErr, prev.txErr), udiff(cur.txDrop, prev.txDrop)}
		ids := c.netID(name)
		*out = append(*out,
			Sample{MetricID: ids.rx, TS: now, Value: d[0] / dt},
			Sample{MetricID: ids.tx, TS: now, Value: d[4] / dt},
			Sample{MetricID: ids.rxDrop, TS: now, Value: d[3] / dt},
			Sample{MetricID: ids.txDrop, TS: now, Value: d[7] / dt},
			Sample{MetricID: ids.rxErrs, TS: now, Value: d[2] / dt},
			Sample{MetricID: ids.txErrs, TS: now, Value: d[6] / dt})
		nAll++
		for i := range d {
			all[i] += d[i]
		}
		if p, known := c.ifaceIsPhysical(name, now); p || !known {
			nPhys++
			for i := range d {
				phys[i] += d[i]
			}
		}
	}
	if !hasPrev || dt <= 0 || nAll == 0 {
		return
	}
	agg := phys
	if nPhys == 0 { // 没有识别出物理口（比如全是 bond 且 sysfs 不全）：退回全部相加
		agg = all
	}
	*out = append(*out,
		Sample{MetricID: "net.rx", TS: now, Value: agg[0] / dt},
		Sample{MetricID: "net.tx", TS: now, Value: agg[4] / dt},
		Sample{MetricID: "net.rx_pps", TS: now, Value: agg[1] / dt},
		Sample{MetricID: "net.tx_pps", TS: now, Value: agg[5] / dt},
		Sample{MetricID: "net.rx_drop", TS: now, Value: agg[3] / dt},
		Sample{MetricID: "net.tx_drop", TS: now, Value: agg[7] / dt},
		Sample{MetricID: "net.rx_errs", TS: now, Value: agg[2] / dt},
		Sample{MetricID: "net.tx_errs", TS: now, Value: agg[6] / dt})
}
