package check

import (
	"os"
	"testing"
)

func resetBaselineState() {
	SetBaseline(nil)
	blMu.Lock()
	first = map[string]float64{}
	blMu.Unlock()
}

// 累计计数器非零但没有新增时，不该报警 —— "曾经发生过" 不是 "现在有问题"。
func TestHistoricalCounterIsNotAWarning(t *testing.T) {
	resetBaselineState()
	c1 := counterCheck("CT02", "conntrack 丢弃", 3, 1, "丢弃 3", "丢弃")
	if c1.Level != 0 {
		t.Fatalf("首次观测就报警了: %+v", c1)
	}
	// 同一个值再判多少次都不该报警
	for i := 0; i < 5; i++ {
		if c := counterCheck("CT02", "conntrack 丢弃", 3, 1, "丢弃 3", "丢弃"); c.Level != 0 {
			t.Fatalf("第 %d 次复判报警: %+v", i, c)
		}
	}
	// 真的又涨了才报警，并且要说清新增多少
	c2 := counterCheck("CT02", "conntrack 丢弃", 5, 1, "丢弃 5", "丢弃")
	if c2.Level != 1 {
		t.Fatalf("新增之后没报警: %+v", c2)
	}
	if want := "新增 丢弃 2"; !contains(c2.Message, want) {
		t.Fatalf("消息没说明新增量: %q", c2.Message)
	}
}

// 计数器归零（重启/回绕）后重新起算，不应算出负增量。
func TestCounterResetDoesNotAlert(t *testing.T) {
	resetBaselineState()
	counterCheck("N05", "listen 溢出", 100, 1, "溢出 100", "溢出")
	c := counterCheck("N05", "listen 溢出", 0, 1, "溢出 0", "溢出")
	if c.Level != 0 {
		t.Fatalf("归零后报警: %+v", c)
	}
	if c2 := counterCheck("N05", "listen 溢出", 2, 1, "溢出 2", "溢出"); c2.Level != 1 {
		t.Fatalf("重新起算后的新增没报警: %+v", c2)
	}
}

// 人工基线：把当前值定成"标准状态"，之后只对超出基线的部分报警。
func TestManualBaselineSuppressesUntilItGrows(t *testing.T) {
	resetBaselineState()
	d := t.TempDir()
	SetBaseline(&Baseline{Vals: map[string]float64{"CT02": 10}})
	if c := counterCheck("CT02", "x", 10, 1, "10", "丢弃"); c.Level != 0 {
		t.Fatalf("等于基线还报警: %+v", c)
	}
	if c := counterCheck("CT02", "x", 11, 1, "11", "丢弃"); c.Level != 1 {
		t.Fatalf("超过基线没报警: %+v", c)
	}
	// 落盘 / 载入 / 清除
	if _, err := SaveBaseline(d, "验收前标定"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(BaselinePath(d)); err != nil {
		t.Fatalf("基线没落盘: %v", err)
	}
	SetBaseline(nil)
	got, err := LoadBaseline(d)
	if err != nil || got == nil {
		t.Fatalf("载入失败: %v %v", got, err)
	}
	if got.Note != "验收前标定" {
		t.Fatalf("备注丢了: %q", got.Note)
	}
	if CurrentBaseline() == nil {
		t.Fatal("载入后没生效")
	}
	if err := ClearBaseline(d); err != nil {
		t.Fatal(err)
	}
	if CurrentBaseline() != nil {
		t.Fatal("清除后仍然生效")
	}
	if err := ClearBaseline(d); err != nil {
		t.Fatalf("重复清除应当无害: %v", err)
	}
}

// taint 是位图：基线里已有的位只作说明，只有新出现的位才报警。
func TestTaintOnlyAlertsOnNewBits(t *testing.T) {
	resetBaselineState()
	dec := func(v int64) string { return "位" }
	if c := taintCheck(12288, dec); c.Level != 0 { // 12288 = 外部编译模块 + 未签名模块
		t.Fatalf("首次观测到既有污染位就报警: %+v", c)
	}
	if c := taintCheck(12288|1<<7, dec); c.Level != 1 {
		t.Fatalf("新增 oops 位没报警: %+v", c)
	}
	resetBaselineState()
	if c := taintCheck(0, dec); c.Level != 0 || c.Message != "0（干净）" {
		t.Fatalf("干净内核判错: %+v", c)
	}
}

// 基线快照的键必须和判定用的键一致，否则基线等于没生效。
func TestSnapshotKeysMatchCheckIDs(t *testing.T) {
	resetBaselineState()
	d := t.TempDir()
	write(t, d+"/vmstat", "oom_kill 4\n")
	write(t, d+"/sys/kernel/tainted", "512\n")
	write(t, d+"/net/stat/nf_conntrack", "entries drop insert_failed\n1 a 2\n")
	write(t, d+"/net/netstat", "TcpExt: ListenOverflows ListenDrops\nTcpExt: 3 4\n")
	old := procRoot
	procRoot = d
	defer func() { procRoot = old }()

	snap := CounterSnapshot()
	for k, want := range map[string]float64{"CT02": 12, "N05": 7, "M05": 4, "kernel.tainted": 512} {
		if got, ok := snap[k]; !ok || got != want {
			t.Errorf("snapshot[%q] = %v (ok=%v)，期望 %v", k, got, ok, want)
		}
	}
	// 用这份快照当基线，对应的检查都不该报警
	SetBaseline(&Baseline{Vals: snap})
	if c := find(checkConntrackCategory(nil), "CT02"); c.Level != 0 {
		t.Errorf("CT02 在基线下仍报警: %+v", c)
	}
	if c := find(checkErrorsCategory(nil), "E01"); c.Level != 0 {
		t.Errorf("E01 在基线下仍报警: %+v", c)
	}
	if c := find(checkNetworkCategory(nil), "N05"); c.Level != 0 {
		t.Errorf("N05 在基线下仍报警: %+v", c)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
