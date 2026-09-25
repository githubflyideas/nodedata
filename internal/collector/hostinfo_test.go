package collector

import (
	"os"
	"path/filepath"
	"testing"
)

func fakeSys(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, c := range files {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(c), 0o644)
	}
	return root
}

func TestDiskKind(t *testing.T) {
	sys := fakeSys(t, map[string]string{
		"block/sda/queue/rotational": "1\n",
		"block/sdb/queue/rotational": "0\n",
		"block/vda/queue/rotational": "1\n", // virtio 盘报 1，不可信
	})
	cases := []struct{ name, virt, want string }{
		{"sda", "", DiskHDD},
		{"sdb", "", DiskSSD},
		{"nvme0n1", "", DiskSSD},
		{"vda", "", DiskVirtual},    // 物理机上不会有 vd*，有就当虚拟盘
		{"sda", "KVM", DiskVirtual}, // 虚拟机里的 sda：rotational=1 也不当真
		{"nvme0n1", "AWS EC2", DiskSSD},
		{"sdz", "", DiskUnknown}, // 读不到 sysfs
	}
	for _, c := range cases {
		if got := DiskKind(sys, c.name, c.virt); got != c.want {
			t.Errorf("DiskKind(%s, virt=%q) = %s，期望 %s", c.name, c.virt, got, c.want)
		}
	}
}

func TestDetectVirt(t *testing.T) {
	proc := fakeSys(t, map[string]string{"cpuinfo": "processor\t: 0\nflags\t\t: fpu vme hypervisor lm\n"})
	if v := DetectVirt(proc, t.TempDir()); v != "虚拟机" {
		t.Errorf("只有 hypervisor 标志时应认出是虚拟机，实得 %q", v)
	}
	kvm := fakeSys(t, map[string]string{"class/dmi/id/sys_vendor": "QEMU\n", "class/dmi/id/product_name": "Standard PC\n"})
	if v := DetectVirt(t.TempDir(), kvm); v != "KVM" {
		t.Errorf("DMI 厂商 QEMU 应认成 KVM，实得 %q", v)
	}
	bare := fakeSys(t, map[string]string{"cpuinfo": "flags\t\t: fpu vme lm\n"})
	if v := DetectVirt(bare, t.TempDir()); v != "" {
		t.Errorf("物理机应返回空，实得 %q", v)
	}
}
