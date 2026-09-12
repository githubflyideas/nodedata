package collector

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSockstatAndSnmp(t *testing.T) {
	proc := t.TempDir()
	os.MkdirAll(filepath.Join(proc, "net"), 0o755)
	os.MkdirAll(filepath.Join(proc, "sys/net/netfilter"), 0o755)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(proc, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("net/sockstat", "sockets: used 431\nTCP: inuse 60 orphan 3 tw 812 alloc 71 mem 9\nUDP: inuse 12 mem 4\nUDPLITE: inuse 0\nRAW: inuse 0\nFRAG: inuse 0 memory 0\n")
	// 故意用与本机不同的列顺序：字段位置随内核版本变化，必须按表头名取
	write("net/snmp", "Tcp: RtoAlgorithm CurrEstab RetransSegs PassiveOpens AttemptFails\nTcp: 1 30 100 500 7\n"+
		"Udp: InDatagrams RcvbufErrors InErrors NoPorts OutDatagrams SndbufErrors\nUdp: 1000 40 20 5 900 0\n")
	write("sys/net/netfilter/nf_conntrack_count", "16384\n")
	write("sys/net/netfilter/nf_conntrack_max", "65536\n")
	write("stat", "cpu  1 0 1 1 0 0 0 0 0 0\nctxt 100\nintr 100\nprocs_running 1\nprocs_blocked 0\n")
	write("meminfo", "MemTotal:       16000000 kB\nMemAvailable:    4000000 kB\nMemFree:  1000 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n")
	write("loadavg", "0.1 0.2 0.3 1/100 123\n")

	c := New(Config{ProcRoot: proc, SysRoot: t.TempDir(), RootFS: proc, Interval: 5 * time.Second})
	t0 := time.Unix(1_800_000_000, 0)
	collectOnce(t, c, proc, t0)
	m := collectOnce(t, c, proc, t0.Add(5*time.Second))

	// 瞬时量
	for id, want := range map[string]float64{
		"sock.used": 431, "sock.tcp_inuse": 60, "sock.tcp_tw": 812, "sock.tcp_orphan": 3,
		"sock.tcp_alloc": 71, "sock.udp_inuse": 12, "tcp.estab": 30,
		"conntrack": 16384, "conntrack.used_pct": 25,
	} {
		if got, ok := m[id]; !ok || got != want {
			t.Errorf("%s = %v (ok=%v), want %v", id, got, ok, want)
		}
	}
	// 速率类：第二轮与第一轮相同 ⇒ 差分为 0，但字段必须存在（而不是缺失）
	for _, id := range []string{"udp.rcvbuf_errors", "udp.in_errors", "tcp.retrans", "udp.in_pps"} {
		if _, ok := m[id]; !ok {
			t.Errorf("%s 缺失", id)
		}
	}
	// 内存使用率按 MemAvailable 算：1 - 4/16 = 75%
	if got := m["mem.used_pct"]; got != 75 {
		t.Errorf("mem.used_pct = %v, want 75（必须按 MemAvailable，不能按 MemFree）", got)
	}
	// 文件系统容量
	if got, ok := m["fs.used_pct"]; !ok || got < 0 || got > 100 {
		t.Errorf("fs.used_pct = %v ok=%v", got, ok)
	}

	// 第三轮：UDP 溢出涨了 500，5 秒 ⇒ 100/s
	write("net/snmp", "Tcp: RtoAlgorithm CurrEstab RetransSegs PassiveOpens AttemptFails\nTcp: 1 31 100 500 7\n"+
		"Udp: InDatagrams RcvbufErrors InErrors NoPorts OutDatagrams SndbufErrors\nUdp: 1000 540 20 5 900 0\n")
	m = collectOnce(t, c, proc, t0.Add(10*time.Second))
	if got := m["udp.rcvbuf_errors"]; got != 100 {
		t.Errorf("udp.rcvbuf_errors = %v, want 100/s", got)
	}
}

// 表头与数值行不配对时不能错位取值（内核升级、文件被截断都可能出现）。
func TestSnmpMismatchedHeader(t *testing.T) {
	proc := t.TempDir()
	os.MkdirAll(filepath.Join(proc, "net"), 0o755)
	c := New(Config{ProcRoot: proc, Interval: 5 * time.Second})
	var out []Sample
	c.parseNetSnmpV2([]byte("Udp: InDatagrams RcvbufErrors InErrors\nUdp: 1000 40\n"), time.Now(), 5, true, &out)
	if len(out) != 0 {
		t.Fatalf("字段数不匹配时必须整行跳过，got %+v", out)
	}
}
