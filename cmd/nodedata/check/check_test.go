package check

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func find(cat Category, id string) Check {
	for _, c := range cat.Checks {
		if c.ID == id {
			return c
		}
	}
	return Check{ID: id, Name: "MISSING", Level: -1}
}

func TestGradeThresholds(t *testing.T) {
	cases := []struct {
		v, warn, fail float64
		want          int
	}{
		{0, 80, 95, 0}, {79.9, 80, 95, 0}, {80, 80, 95, 1},
		{94.9, 80, 95, 1}, {95, 80, 95, 2},
		{100, 80, 0, 1}, // fail=0 表示这项最高只到 warn
	}
	for _, c := range cases {
		if got := grade(c.v, c.warn, c.fail); got != c.want {
			t.Errorf("grade(%v,%v,%v)=%d want %d", c.v, c.warn, c.fail, got, c.want)
		}
	}
}

// meminfo 必须把内核的 kB 换成字节，否则所有内存判定都会差 1024 倍。
func TestMeminfoConvertsKBToBytes(t *testing.T) {
	d := t.TempDir()
	write(t, d+"/meminfo", "MemTotal:       4000000 kB\nMemAvailable:    100000 kB\nHugePages_Total:       0\n")
	old := procRoot
	procRoot = d
	defer func() { procRoot = old }()
	m := meminfo()
	if m["MemTotal"] != 4000000*1024 {
		t.Errorf("MemTotal=%v want %v", m["MemTotal"], 4000000*1024)
	}
	if m["HugePages_Total"] != 0 {
		t.Errorf("无单位的项不该被乘 1024")
	}
}

func TestSockstatAndSnmpParsing(t *testing.T) {
	d := t.TempDir()
	write(t, d+"/net/sockstat", "sockets: used 75\nTCP: inuse 1 orphan 7 tw 42 alloc 2 mem 189\n")
	write(t, d+"/net/snmp",
		"Tcp: RtoAlgorithm RtoMin OutSegs RetransSegs\nTcp: 1 200 1000 30\n")
	old := procRoot
	procRoot = d
	defer func() { procRoot = old }()

	if v, ok := sockstat("TCP:", "tw"); !ok || v != 42 {
		t.Errorf("tw=%v ok=%v want 42", v, ok)
	}
	if v, ok := sockstat("TCP:", "orphan"); !ok || v != 7 {
		t.Errorf("orphan=%v ok=%v want 7", v, ok)
	}
	if v, ok := snmpVal(d+"/net/snmp", "Tcp", "RetransSegs"); !ok || v != 30 {
		t.Errorf("RetransSegs=%v ok=%v want 30", v, ok)
	}
}

// 关键回归：内存吃紧的假 /proc 必须真的把 M01 升到 FAIL。
// 这条测试在旧实现（循环里硬编码 Level 0）下必然失败。
func TestMemoryPressureActuallyFires(t *testing.T) {
	d := t.TempDir()
	write(t, d+"/meminfo", "MemTotal: 1000000 kB\nMemAvailable: 20000 kB\n"+
		"SwapTotal: 100000 kB\nSwapFree: 10000 kB\nDirty: 200000 kB\n"+
		"Writeback: 900000 kB\nCommitLimit: 500000 kB\nCommitted_AS: 900000 kB\n"+
		"Slab: 400000 kB\n")
	write(t, d+"/vmstat", "nr_free_pages 100\noom_kill 3\n")
	old := procRoot
	procRoot = d
	defer func() { procRoot = old }()

	cat := checkMemoryCategory(nil)
	for _, tc := range []struct {
		id  string
		min int
	}{
		{"M01", 2}, // 可用内存只剩 2%
		{"M02", 2}, // swap 用了 90%
		{"M03", 2}, // 脏页 20%
		{"M04", 2}, // 回写堆积 878 MiB
		{"M05", 1}, // 有过 OOM 击杀：历史记录留 1 级，观察期内再杀才升到 2
		{"M06", 2}, // commit 180%
		{"M07", 2}, // slab 40%
	} {
		if c := find(cat, tc.id); c.Level < tc.min {
			t.Errorf("%s level=%d want >=%d（message=%q）", tc.id, c.Level, tc.min, c.Message)
		}
	}
	if cat.Level != 2 {
		t.Errorf("类级别 =%d want 2", cat.Level)
	}
}

// 读不到的内核接口必须判 Level 0 并说明原因 —— "不可知" 不等于 "有问题"。
// 旧实现里 CT01 是硬编码的 Level 1 "Not available"，这条就是为它写的。
func TestMissingKernelInterfaceIsNotAWarning(t *testing.T) {
	d := t.TempDir() // 空目录：什么都读不到
	old, oldS := procRoot, sysRoot
	procRoot, sysRoot = d, d
	defer func() { procRoot, sysRoot = old, oldS }()

	ct := checkConntrackCategory(nil)
	if ct.Level != 0 {
		t.Errorf("未启用 conntrack 时类级别 =%d want 0", ct.Level)
	}
	if c := find(ct, "CT01"); c.Level != 0 {
		t.Errorf("CT01 level=%d message=%q，want 0", c.Level, c.Message)
	}
	for _, c := range ct.Checks {
		if c.Message == "Not available" {
			t.Errorf("%s 又变回硬编码的 Not available 了", c.ID)
		}
	}
}

// 每一项都必须有真名字和非空 message，不允许再出现 "CPU 1" 这种占位。
func TestNoPlaceholderNames(t *testing.T) {
	res, _ := Run("5s")
	seen := map[string]bool{}
	n := 0
	placeholder := []string{"CPU 1", "Memory 1", "Disk 1", "Network 1",
		"Socket 1", "Error 1", "FS 1", "Check T02"}
	for _, cat := range res.Categories {
		for _, c := range cat.Checks {
			n++
			if seen[c.ID] {
				t.Errorf("ID 重复：%s", c.ID)
			}
			seen[c.ID] = true
			if c.Name == "" || c.Message == "" {
				t.Errorf("%s 名字或消息为空", c.ID)
			}
			for _, p := range placeholder {
				if c.Name == p {
					t.Errorf("%s 还是占位名字 %q", c.ID, p)
				}
			}
			if c.Message == "OK" {
				t.Errorf("%s 的 message 仍是无信息的 \"OK\"", c.ID)
			}
		}
	}
	if n != 41 {
		t.Errorf("共 %d 项，want 41", n)
	}
	if len(res.Categories) != 9 {
		t.Errorf("共 %d 类，want 9", len(res.Categories))
	}
}
