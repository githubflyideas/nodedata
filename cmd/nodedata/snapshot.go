// snapshot.go — 异常现场时间线。
//
// 单个瞬间分不清"尖峰路过"和"持续劣化"。所以一次事故留的是一条时间线：
//
//	BEFORE    从内存时序切片——零成本，而且是唯一能拿到的真·事前数据
//	ONSET     T0 全量抓
//	DURING    T+5s / T+15s / T+30s 轻量抓（只抓会变的那几项）
//	RECOVERY  结论消失后抓一次，或 5 分钟封顶
//
// 两条硬约束，都来自"我们自己不能成为性能卡点"：
//
//  1. **按类裁剪**。CPU 异常没必要抓五十个磁盘指标。不裁剪的话，五次采样就是
//     五倍的体积——上一轮刚把磁盘写入从 21GB/天压到 10MB/天，一次就还回去了。
//  2. **网卡只读白名单**。/sys/class/net/<if>/ 下有几百个节点，其中一些读取会触发
//     驱动交互（ethtool 类节点在故障时可能很慢甚至卡住）。故障机上最容易卡死的，
//     恰恰是最想读的那个文件。
package main

import (
	"encoding/json"

	"github.com/githubflyideas/nodedata/internal/diagnosis"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 时间线的采样点（相对 T0）。
var snapshotOffsets = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}

const (
	snapshotMaxDuration = 5 * time.Minute // 长故障不能一直挂着不落盘
	snapshotGoneRounds  = 2               // 结论连续消失这么多轮算恢复
	snapshotMaxOpen     = 3               // 同时进行的采样会话上限
)

// phaseSample 是时间线上的一格。
type phaseSample struct {
	Phase  string            `json:"phase"` // onset | during | recovery
	At     time.Time         `json:"at"`
	Offset string            `json:"offset"` // 相对 T0
	System map[string]string `json:"system"`
	Procs  []procLite        `json:"procs,omitempty"`
	DState []dRow            `json:"d_state,omitempty"`
}

// procLite 是后续采样里的进程行：只留会变的那几项，不重复 T0 已有的静态信息。
type procLite struct {
	PID   int     `json:"pid"`
	Comm  string  `json:"comm"`
	CPU   float64 `json:"cpu"`
	RSS   uint64  `json:"rss"`
	State string  `json:"state"`
	RBps  float64 `json:"read_bps,omitempty"`
	WBps  float64 `json:"write_bps,omitempty"`
}

// beforePoint 是 BEFORE 段的一个时序点（从内存序列切出来的）。
type beforePoint struct {
	TS int64              `json:"ts"`
	V  map[string]float64 `json:"v"`
}

// classFiles 按异常类别决定抓哪些 /proc 文件。
// 第一组是 T0 全量，第二组是后续轻量采样——后者只保留"会变且与该类相关"的。
var classFiles = map[string][2][]string{
	"CPU": {
		{"stat", "loadavg", "uptime", "pressure/cpu", "pressure/io", "meminfo", "vmstat", "schedstat"},
		{"stat", "loadavg", "pressure/cpu"},
	},
	"IO": {
		{"diskstats", "loadavg", "pressure/io", "pressure/cpu", "meminfo", "vmstat"},
		{"diskstats", "pressure/io", "loadavg"},
	},
	"内存": {
		{"meminfo", "vmstat", "pressure/memory", "loadavg", "zoneinfo", "buddyinfo"},
		{"meminfo", "vmstat", "pressure/memory"},
	},
	"网络": {
		{"net/dev", "net/snmp", "net/netstat", "net/sockstat", "net/softnet_stat", "loadavg"},
		{"net/dev", "net/snmp", "net/sockstat", "net/softnet_stat"},
	},
}

// defaultFiles 用于没有专门裁剪表的类别。
var defaultFiles = [2][]string{
	{"stat", "loadavg", "meminfo", "vmstat", "pressure/cpu", "pressure/io", "pressure/memory", "diskstats", "net/dev"},
	{"loadavg", "meminfo", "pressure/cpu"},
}

func filesFor(class string, light bool) []string {
	set, ok := classFiles[class]
	if !ok {
		set = defaultFiles
	}
	if light {
		return set[1]
	}
	return set[0]
}

// netIfaceFields 是 /sys/class/net/<if>/ 下允许读取的白名单。
//
// 整目录遍历是不可接受的：那下面有几百个节点，部分读取会触发驱动交互。
// 这几个都是普通 sysfs 属性，读的是内核里的变量，不下发到硬件。
var netIfaceFields = []string{
	"operstate", "carrier", "carrier_changes", "speed", "mtu", "flags",
	"statistics/rx_bytes", "statistics/tx_bytes",
	"statistics/rx_dropped", "statistics/tx_dropped",
	"statistics/rx_errors", "statistics/tx_errors",
}

// netSnapshot 只看物理网卡，且只读白名单字段。
func netSnapshot(sysRoot string) map[string]string {
	out := map[string]string{}
	base := filepath.Join(sysRoot, "class", "net")
	ents, err := os.ReadDir(base)
	if err != nil {
		return out
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		// 只要有 device 软链的才是物理网卡：lo、bond、vlan、veth 不抓
		if _, err := os.Stat(filepath.Join(base, e.Name(), "device")); err != nil {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if len(names) > 8 { // 网卡再多也不值得全抓
		names = names[:8]
	}
	for _, n := range names {
		for _, f := range netIfaceFields {
			b, err := os.ReadFile(filepath.Join(base, n, f))
			if err != nil {
				continue
			}
			out[n+"/"+f] = strings.TrimSpace(string(b))
		}
	}
	return out
}

// sample 抓一格现场。light=true 时只抓该类"会变"的那几项。
func (r *Recorder) sample(class, phase string, t0, now time.Time, light bool) phaseSample {
	ps := phaseSample{Phase: phase, At: now, System: map[string]string{},
		Offset: now.Sub(t0).Round(time.Second).String()}
	for _, f := range filesFor(class, light) {
		if b, err := os.ReadFile(filepath.Join(r.procRoot, f)); err == nil {
			ps.System[f] = string(b)
		}
	}
	if class == "网络" {
		for k, v := range netSnapshot(r.sysRoot) {
			ps.System["net:"+k] = v
		}
	}
	if r.procs != nil {
		all := r.procs()
		sort.Slice(all, func(i, j int) bool { return all[i].CPU > all[j].CPU })
		if len(all) > 10 {
			all = all[:10]
		}
		for _, p := range all {
			ps.Procs = append(ps.Procs, procLite{PID: p.PID, Comm: p.Comm, CPU: p.CPU,
				RSS: p.RSS, State: p.State, RBps: p.ReadBps, WBps: p.WriteBps})
		}
	}
	if class == "IO" || class == "CPU" {
		ps.DState = r.dstate()
	}
	return ps
}

// advanceSessions 推进所有进行中的时间线：到点就抓一格，结论消失或超时就收尾。
// 由 Tick 每轮调用（默认 15 秒一轮），所以 +5s 那一格实际落在下一轮——
// 这是刻意的：为了抓一格而单开定时器，等于在故障机上多起一个唤醒源。
func (r *Recorder) advanceSessions(chain *diagnosis.Chain, now time.Time) {
	active := map[string]bool{}
	if chain != nil {
		for _, it := range chain.Items {
			if it.Class != "" {
				active[it.Class] = true
			}
		}
	}
	r.mu.Lock()
	sess := make([]*snapSession, 0, len(r.sessions))
	for _, s := range r.sessions {
		sess = append(sess, s)
	}
	r.mu.Unlock()

	for _, ss := range sess {
		if ss.done {
			continue
		}
		elapsed := now.Sub(ss.t0)
		// DURING：一轮（默认 15 秒）可能跨过好几个偏移点。
		// 那就只抓一次、记下它覆盖了哪几格——同一时刻抓两遍既浪费又误导，
		// 看的人会以为那是两次独立观测。
		covered := []string{}
		for ss.nextIdx < len(snapshotOffsets) && elapsed >= snapshotOffsets[ss.nextIdx] {
			covered = append(covered, snapshotOffsets[ss.nextIdx].String())
			ss.nextIdx++
		}
		if len(covered) > 0 {
			d := r.sample(ss.class, "during", ss.t0, now, true)
			if len(covered) > 1 {
				d.System["_covers"] = strings.Join(covered, ",") + "（这几格落在同一个采样轮里）"
			}
			ss.samples = append(ss.samples, d)
		}
		// RECOVERY：结论消失够久，或超过封顶时长
		if !active[ss.class] {
			ss.goneFor++
		} else {
			ss.goneFor = 0
		}
		if ss.goneFor >= snapshotGoneRounds || elapsed >= snapshotMaxDuration {
			reason := "结论消失"
			if elapsed >= snapshotMaxDuration {
				reason = "超过封顶时长，故障仍在持续"
			}
			rec := r.sample(ss.class, "recovery", ss.t0, now, true)
			rec.System["_note"] = reason
			ss.samples = append(ss.samples, rec)
			ss.done = true
			r.finishSession(ss)
		}
	}
}

// finishSession 把时间线补写回事故文件。
func (r *Recorder) finishSession(ss *snapSession) {
	r.mu.Lock()
	delete(r.sessions, ss.id)
	r.mu.Unlock()

	b, err := os.ReadFile(ss.path)
	if err != nil {
		return
	}
	var m map[string]interface{}
	if json.Unmarshal(b, &m) != nil {
		return
	}
	m["timeline"] = ss.samples
	out, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return
	}
	tmp := ss.path + ".tmp"
	if os.WriteFile(tmp, out, 0o640) == nil {
		os.Rename(tmp, ss.path)
	}
}

// startSession 在一次留证之后开启时间线采样。
func (r *Recorder) startSession(id, class, path string, t0 time.Time, before []beforePoint) {
	r.mu.Lock()
	if len(r.sessions) >= snapshotMaxOpen { // 并发上限：故障风暴时不能无限开会话
		r.mu.Unlock()
		return
	}
	ss := &snapSession{id: id, class: class, t0: t0, path: path}
	r.sessions[id] = ss
	r.mu.Unlock()

	onset := r.sample(class, "onset", t0, t0, false)
	if len(before) > 0 {
		ss.samples = append(ss.samples, phaseSample{Phase: "before", At: before[0].TS0(),
			Offset: before[0].TS0().Sub(t0).Round(time.Second).String(),
			System: map[string]string{"_series": beforeJSON(before)}})
	}
	ss.samples = append(ss.samples, onset)
}

// TS0 把 unix 秒转回时刻。
func (b beforePoint) TS0() time.Time { return time.Unix(b.TS, 0) }

func beforeJSON(pts []beforePoint) string {
	b, err := json.Marshal(pts)
	if err != nil {
		return ""
	}
	return string(b)
}

// classBeforeMetrics 决定 BEFORE 段切哪些序列。
// 同样按类裁剪：CPU 异常没必要把磁盘的每条曲线都带上。
func classBeforeMetrics(class string) []string {
	switch class {
	case "CPU":
		return []string{"cpu.user", "cpu.sys", "cpu.iowait", "cpu.steal", "loadavg.1m",
			"procs_running", "procs_blocked", "psi.cpu.some10", "ctxt"}
	case "IO":
		return []string{"disk.util", "disk.await_w", "disk.await_r", "disk.riops", "disk.wiops",
			"disk.rbytes", "disk.wbytes", "psi.io.some10", "procs_blocked"}
	case "内存":
		return []string{"mem.available", "mem.used_pct", "mem.cached", "swap.used", "slab",
			"psi.mem.some10", "pgmajfault"}
	case "网络":
		return []string{"net.rx", "net.tx", "net.rx_pps", "net.tx_pps", "net.rx_drop",
			"tcp.retrans", "udp.rcvbuf_errors", "tcp.estab", "conntrack.used_pct"}
	}
	return []string{"cpu.user", "cpu.sys", "loadavg.1m", "mem.available", "disk.util", "net.rx"}
}
