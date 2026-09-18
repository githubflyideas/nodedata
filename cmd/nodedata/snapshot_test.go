package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/diagnosis"
)

// 一次事故留的是一条时间线，不是一个瞬间：单个瞬间分不清"尖峰路过"和"持续劣化"。
func TestSnapshotTimeline(t *testing.T) {
	dir := t.TempDir()
	var chain diagnosis.Chain
	item := diagnosis.Item{Level: diagnosis.Warning, Class: diagnosis.ClassCPU, Title: "CPU 劣化",
		Culprits: []diagnosis.Culprit{{Kind: "process", PID: 1, Name: "hog"}}}
	r, err := NewRecorder(dir, "/proc", func() *diagnosis.Chain { return &chain }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.kmsgPath = dir + "/nokmsg"
	r.before = func(class string, from, to time.Time) []beforePoint {
		return []beforePoint{{TS: from.Unix(), V: map[string]float64{"cpu.user": 3}}}
	}
	t0 := time.Now()

	chain = diagnosis.Chain{Items: []diagnosis.Item{item}}
	ids := r.Tick(t0)
	if len(ids) != 1 {
		t.Fatalf("应留一份证据：%v", ids)
	}
	// 故障持续：+5s / +15s / +30s 各补一格
	for _, off := range []time.Duration{6 * time.Second, 16 * time.Second, 31 * time.Second} {
		r.advanceSessions(&chain, t0.Add(off))
	}
	// 故障消失：连续两轮后收尾
	empty := diagnosis.Chain{}
	r.advanceSessions(&empty, t0.Add(45*time.Second))
	r.advanceSessions(&empty, t0.Add(60*time.Second))

	b, err := os.ReadFile(filepath.Join(dir, ids[0]+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var ev struct {
		Timeline []phaseSample `json:"timeline"`
	}
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatal(err)
	}
	phases := make([]string, 0, len(ev.Timeline))
	for _, s := range ev.Timeline {
		phases = append(phases, s.Phase)
	}
	got := strings.Join(phases, ",")
	// +5s 与 +15s 若落在同一轮只抓一次，所以 during 的格数取决于推进节奏
	want := "before,onset,during,during,during,recovery"
	if got != want {
		t.Errorf("时间线应是 %q，实际 %q", want, got)
	}
	// 按类裁剪：CPU 异常不该抓一堆磁盘指标
	for _, s := range ev.Timeline {
		if s.Phase == "before" {
			continue
		}
		if _, bad := s.System["diskstats"]; bad {
			t.Errorf("CPU 类不该抓 diskstats（%s 格）", s.Phase)
		}
		if _, ok := s.System["loadavg"]; !ok {
			t.Errorf("CPU 类每格都该有 loadavg（%s 格缺）", s.Phase)
		}
	}
	// 长故障封顶：不能一直挂着不落盘
	chain = diagnosis.Chain{Items: []diagnosis.Item{item}}
	ids2 := r.Tick(t0.Add(30 * time.Minute))
	if len(ids2) == 1 {
		r.advanceSessions(&chain, t0.Add(30*time.Minute+snapshotMaxDuration+time.Second))
		r.mu.Lock()
		open := len(r.sessions)
		r.mu.Unlock()
		if open != 0 {
			t.Errorf("超过封顶时长后会话该收尾，仍有 %d 个开着", open)
		}
	}
}

// 网卡只读白名单：/sys/class/net 下有几百个节点，部分读取会触发驱动交互。
func TestNetSnapshotWhitelist(t *testing.T) {
	root := t.TempDir()
	iface := filepath.Join(root, "class", "net", "eth0")
	os.MkdirAll(filepath.Join(iface, "statistics"), 0o755)
	os.MkdirAll(filepath.Join(iface, "device"), 0o755) // 有 device 才算物理网卡
	os.WriteFile(filepath.Join(iface, "operstate"), []byte("up\n"), 0o644)
	os.WriteFile(filepath.Join(iface, "carrier_changes"), []byte("7\n"), 0o644)
	os.WriteFile(filepath.Join(iface, "statistics", "rx_dropped"), []byte("42\n"), 0o644)
	// 白名单之外的节点，即使存在也不该被读
	os.WriteFile(filepath.Join(iface, "tx_queue_len"), []byte("1000\n"), 0o644)
	// 虚拟口（无 device）不该被抓
	lo := filepath.Join(root, "class", "net", "lo")
	os.MkdirAll(lo, 0o755)
	os.WriteFile(filepath.Join(lo, "operstate"), []byte("unknown\n"), 0o644)

	got := netSnapshot(root)
	if got["eth0/operstate"] != "up" || got["eth0/carrier_changes"] != "7" ||
		got["eth0/statistics/rx_dropped"] != "42" {
		t.Fatalf("白名单字段没抓全：%+v", got)
	}
	if _, bad := got["eth0/tx_queue_len"]; bad {
		t.Error("白名单之外的节点不该被读")
	}
	for k := range got {
		if strings.HasPrefix(k, "lo/") {
			t.Error("无 device 的虚拟口不该被抓")
		}
	}
}
