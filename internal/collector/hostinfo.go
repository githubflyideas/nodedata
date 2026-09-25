// hostinfo.go — 机器背景：是不是虚拟机、盘是什么类型。
//
// 这些都是"读数的前提"：不知道盘是 SSD，就会把 util 100% 当成盘满了；
// 不知道是虚拟机，就会把 steal 和 virtio 盘的 rotational 当真。
// 只在启动时和发现新盘时读一次，不在采集热路径上。
package collector

import (
	"os"
	"strings"
)

// 盘的类型。只分"机械盘"和"别的"就够用：util 只在机械盘上是可靠的"满没满"。
const (
	DiskHDD     = "hdd"     // 机械盘：一次只能服务一个请求，util 100% 就是满了
	DiskSSD     = "ssd"     // SSD/NVMe：能并行处理请求，util 100% 可能才用了一小部分能力
	DiskVirtual = "virtual" // 虚拟盘：底层是什么由宿主机决定，rotational 字段不可信
	DiskUnknown = "unknown"
)

// DetectVirt 返回虚拟化类型；物理机返回 ""。依次看 /sys/hypervisor、DMI、CPU 标志。
// 认得出厂商就给名字，认不出但确实是虚拟机就给"虚拟机"。
func DetectVirt(procRoot, sysRoot string) string {
	if b, err := os.ReadFile(sysRoot + "/hypervisor/type"); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return strings.ToUpper(t[:1]) + t[1:] // "xen" → "Xen"
		}
	}
	vendor := readTrimmed(sysRoot + "/class/dmi/id/sys_vendor")
	product := readTrimmed(sysRoot + "/class/dmi/id/product_name")
	for _, k := range []struct{ needle, name string }{
		{"KVM", "KVM"}, {"QEMU", "KVM"}, {"VMware", "VMware"}, {"VirtualBox", "VirtualBox"},
		{"innotek", "VirtualBox"}, {"Microsoft Corporation", "Hyper-V"}, {"Xen", "Xen"},
		{"Amazon EC2", "AWS EC2"}, {"Google", "GCE"}, {"Alibaba Cloud", "阿里云 ECS"},
		{"OpenStack", "OpenStack"}, {"Parallels", "Parallels"},
	} {
		if strings.Contains(vendor, k.needle) || strings.Contains(product, k.needle) {
			return k.name
		}
	}
	// 云厂商的裸金属也会带 DMI 厂商名，但不会有 hypervisor 标志——所以最后才看这个
	if b, err := os.ReadFile(procRoot + "/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "flags") {
				if strings.Contains(" "+line+" ", " hypervisor ") {
					return "虚拟机"
				}
				break
			}
		}
	}
	return ""
}

// DiskKind 判断整盘的类型。virt 非空表示跑在虚拟机里。
func DiskKind(sysRoot, name, virt string) string {
	switch {
	case strings.HasPrefix(name, "nvme"):
		return DiskSSD
	case strings.HasPrefix(name, "vd"), strings.HasPrefix(name, "xvd"):
		return DiskVirtual // virtio / xen 盘：rotational 是宿主机随便填的
	}
	rot := readTrimmed(sysRoot + "/block/" + name + "/queue/rotational")
	switch {
	case virt != "":
		// 虚拟机里的 sda 也可能报 rotational=1，底下其实是 SSD 或网络存储
		return DiskVirtual
	case rot == "1":
		return DiskHDD
	case rot == "0":
		return DiskSSD
	}
	return DiskUnknown
}

// DiskKindName 是给人看的名字。
func DiskKindName(k string) string {
	switch k {
	case DiskHDD:
		return "机械盘"
	case DiskSSD:
		return "SSD"
	case DiskVirtual:
		return "虚拟盘"
	}
	return "类型未知"
}

func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
