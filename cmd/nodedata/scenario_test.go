package main

// 故障语料库（第一层：确定性回放，CI 每次都跑）。
//
// 真实管线：Series → σ 重算（模拟时间每 5 分钟一次，与线上相同，故障数据会像线上一样污染基线）
// → L3 最新点 → L4 归因。指标形态按物理机/虚机：日周期、噪声、整数计数、大部分时间为 0 的丢包计数，
// 8 天历史。每个场景断言：出了哪几类结论（不能多也不能少）、责任方是谁，并报告从故障开始
// 到给出正确归因的耗时。安静的一天必须零结论。
//
// 第二层是 cmd/faultlab：在实验机上真的制造故障（见该目录）。

import (
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
	"github.com/githubflyideas/nodedata/internal/diagnosis"
)

const (
	gib = float64(1 << 30)
	mib = float64(1 << 20)
)

type gen func(ts time.Time, r *rand.Rand) float64

func daily(ts time.Time) float64 { // 本地业务日周期，峰在 UTC 12 点
	return math.Sin(2 * math.Pi * (float64(ts.Unix()%86400)/86400 - 0.25))
}
func drops(p float64) gen { // 大部分时间为 0，偶尔 1~2
	return func(_ time.Time, r *rand.Rand) float64 {
		if r.Float64() < p {
			return float64(1 + r.Intn(2))
		}
		return 0
	}
}

var baseline = map[string]gen{
	"cpu.user":                 func(t time.Time, r *rand.Rand) float64 { return 150 + 40*daily(t) + r.NormFloat64()*8 },
	"cpu.sys":                  func(t time.Time, r *rand.Rand) float64 { return 20 + r.NormFloat64()*2 },
	"cpu.softirq":              func(t time.Time, r *rand.Rand) float64 { return 2 + r.NormFloat64()*0.3 },
	"cpu.steal":                func(t time.Time, r *rand.Rand) float64 { return math.Abs(r.NormFloat64() * 0.3) },
	"cpu.iowait":               func(t time.Time, r *rand.Rand) float64 { return 1 + math.Abs(r.NormFloat64()*0.4) },
	"loadavg.1m":               func(t time.Time, r *rand.Rand) float64 { return 3 + 0.8*daily(t) + r.NormFloat64()*0.3 },
	"psi.cpu.some10":           func(t time.Time, r *rand.Rand) float64 { return 2 + math.Abs(r.NormFloat64()*0.8) },
	"psi.io.some10":            func(t time.Time, r *rand.Rand) float64 { return 0.5 + math.Abs(r.NormFloat64()*0.3) },
	"psi.mem.some10":           func(t time.Time, r *rand.Rand) float64 { return math.Abs(r.NormFloat64() * 0.05) },
	"procs_running":            func(t time.Time, r *rand.Rand) float64 { return math.Round(math.Abs(4 + r.NormFloat64()*1.5)) },
	"procs_blocked":            drops(0.1),
	"mem.available":            func(t time.Time, r *rand.Rand) float64 { return 12*gib + 1*gib*daily(t) + r.NormFloat64()*50*mib },
	"mem.free":                 func(t time.Time, r *rand.Rand) float64 { return 2*gib + 0.5*gib*daily(t) + r.NormFloat64()*40*mib },
	"slab":                     func(t time.Time, r *rand.Rand) float64 { return 888*mib + r.NormFloat64()*2*mib },
	"disk.await_w":             func(t time.Time, r *rand.Rand) float64 { return 2 + math.Abs(r.NormFloat64()*0.4) },
	"disk.await_r":             func(t time.Time, r *rand.Rand) float64 { return 1 + math.Abs(r.NormFloat64()*0.3) },
	"disk.wiops":               func(t time.Time, r *rand.Rand) float64 { return 300 + r.NormFloat64()*30 },
	"disk.util@sda":            func(t time.Time, r *rand.Rand) float64 { return 15 + r.NormFloat64()*3 },
	"disk.util@sdb":            func(t time.Time, r *rand.Rand) float64 { return 8 + r.NormFloat64()*2 },
	"disk.await_w@sda":         func(t time.Time, r *rand.Rand) float64 { return 2 + math.Abs(r.NormFloat64()*0.4) },
	"disk.await_w@sdb":         func(t time.Time, r *rand.Rand) float64 { return 1.5 + math.Abs(r.NormFloat64()*0.3) },
	"net.rx":                   func(t time.Time, r *rand.Rand) float64 { return 50*mib + 10*mib*daily(t) + r.NormFloat64()*3*mib },
	"net.tx":                   func(t time.Time, r *rand.Rand) float64 { return 30*mib + 6*mib*daily(t) + r.NormFloat64()*2*mib },
	"net.rx_drop@eth0":         drops(0.05),
	"net.rx_drop@eth1":         drops(0.05),
	"tcp.retrans":              func(t time.Time, r *rand.Rand) float64 { return 5 + math.Abs(r.NormFloat64()*2) },
	"proc.cpu.mysqld":          func(t time.Time, r *rand.Rand) float64 { return 200 + 30*daily(t) + r.NormFloat64()*10 },
	"proc.cpu.nginx":           func(t time.Time, r *rand.Rand) float64 { return 30 + r.NormFloat64()*4 },
	"proc.cpu.nodedata-linux-": func(t time.Time, r *rand.Rand) float64 { return 1 + math.Abs(r.NormFloat64()*0.3) },
	"proc.io.mysqld":           func(t time.Time, r *rand.Rand) float64 { return 5*mib + r.NormFloat64()*1*mib },
}

// 与线上采集一致的派生：整机 util = 最忙那块盘；整机丢包 = 各口之和。
func derive(v map[string]float64) {
	v["disk.util"] = math.Max(v["disk.util@sda"], v["disk.util@sdb"])
	v["net.rx_drop"] = v["net.rx_drop@eth0"] + v["net.rx_drop@eth1"]
}

var normalProcs = []diagnosis.Proc{
	{PID: 1200, Comm: "mysqld", Key: "mysqld", State: "S", CPU: 205, WriteBps: 5 * mib, RSS: uint64(8 * gib), RSSGrowth: int64(10 * mib), GrowthSpan: 3300},
	{PID: 1300, Comm: "nginx", Key: "nginx", State: "S", CPU: 30, RSS: uint64(200 * mib)},
	{PID: 459521, Comm: "nodedata-linux-", Key: "nodedata-linux-", State: "S", CPU: 1, RSS: uint64(80 * mib), Self: true},
}

type mod func(v float64, prog float64, r *rand.Rand) float64

type scenario struct {
	name      string
	faultLen  time.Duration
	mods      map[string]mod // 故障期间对已有指标的改写；prog ∈ [0,1] 为故障进度
	added     map[string]gen // 故障期间才出现的序列（新进程）
	procs     []diagnosis.Proc
	wantClass []string
	wantPID   int    // 首个进程责任方
	wantPlace string // 设备/接口/宿主机名（包含匹配）
	wantTitle string // 标题须包含
}

func add(d float64) mod { return func(v, _ float64, r *rand.Rand) float64 { return v + d } }
func set(x, noise float64) mod {
	return func(_, _ float64, r *rand.Rand) float64 { return x + r.NormFloat64()*noise }
}
func ramp(d float64) mod { return func(v, p float64, _ *rand.Rand) float64 { return v + d*p } }

func withProcs(extra ...diagnosis.Proc) []diagnosis.Proc {
	out := append([]diagnosis.Proc(nil), normalProcs...)
	for _, e := range extra {
		replaced := false
		for i := range out {
			if out[i].PID == e.PID {
				out[i], replaced = e, true
			}
		}
		if !replaced {
			out = append(out, e)
		}
	}
	return out
}

var scenarios = []scenario{
	{name: "quiet-day", faultLen: 3 * time.Hour, procs: normalProcs}, // 3 小时里每 30 秒评估一次
	{name: "cpu-new-runaway", faultLen: 10 * time.Minute,
		mods:      map[string]mod{"cpu.user": add(150), "loadavg.1m": add(2), "psi.cpu.some10": add(15), "procs_running": add(2)},
		added:     map[string]gen{"proc.cpu.burner": func(_ time.Time, r *rand.Rand) float64 { return 150 + r.NormFloat64()*5 }},
		procs:     withProcs(diagnosis.Proc{PID: 4242, Comm: "burner", Key: "burner", State: "R", CPU: 150, RSS: uint64(5 * mib)}),
		wantClass: []string{diagnosis.ClassCPU}, wantPID: 4242},
	{name: "cpu-existing-not-biggest", faultLen: 10 * time.Minute, // nginx 30→150%，mysqld 一直 200% 最大，不能背锅
		mods:      map[string]mod{"cpu.user": add(120), "loadavg.1m": add(1.5), "proc.cpu.nginx": add(120)},
		procs:     withProcs(diagnosis.Proc{PID: 1300, Comm: "nginx", Key: "nginx", State: "R", CPU: 150, RSS: uint64(200 * mib)}),
		wantClass: []string{diagnosis.ClassCPU}, wantPID: 1300},
	{name: "self-overhead", faultLen: 10 * time.Minute, // v3.0.8 事故本身
		mods:      map[string]mod{"cpu.user": add(150), "loadavg.1m": add(2), "proc.cpu.nodedata-linux-": set(150, 5)},
		procs:     withProcs(diagnosis.Proc{PID: 459521, Comm: "nodedata-linux-", Key: "nodedata-linux-", State: "R", CPU: 150, RSS: uint64(900 * mib), Self: true}),
		wantClass: []string{diagnosis.ClassCPU}, wantPID: 459521, wantTitle: "nodedata 自身"},
	{name: "io-writer-one-disk", faultLen: 10 * time.Minute,
		mods: map[string]mod{"disk.util@sdb": set(95, 2), "disk.await_w@sdb": set(40, 4), "disk.await_w": set(25, 3),
			"disk.wiops": add(2000), "psi.io.some10": add(20), "cpu.iowait": add(10), "procs_blocked": set(3, 0.5)},
		added:     map[string]gen{"proc.io.rsync": func(_ time.Time, r *rand.Rand) float64 { return 200*mib + r.NormFloat64()*10*mib }},
		procs:     withProcs(diagnosis.Proc{PID: 5151, Comm: "rsync", Key: "rsync", State: "D", CPU: 30, WriteBps: 200 * mib, RSS: uint64(20 * mib)}),
		wantClass: []string{diagnosis.ClassIO}, wantPID: 5151, wantPlace: "sdb"},
	{name: "mem-leak-3h", faultLen: 3 * time.Hour,
		mods:      map[string]mod{"mem.available": ramp(-6 * gib), "mem.free": ramp(-1.5 * gib)},
		procs:     withProcs(diagnosis.Proc{PID: 6161, Comm: "java", Key: "java", State: "S", CPU: 40, RSS: uint64(7 * gib), RSSGrowth: int64(5 * gib), GrowthSpan: 3300}),
		wantClass: []string{diagnosis.ClassMem}, wantPID: 6161},
	{name: "nic-rx-drop", faultLen: 10 * time.Minute,
		mods:  map[string]mod{"net.rx_drop@eth1": set(200, 20), "tcp.retrans": set(60, 8)},
		procs: normalProcs, wantClass: []string{diagnosis.ClassNet}, wantPlace: "eth1"},
	{name: "vm-steal", faultLen: 10 * time.Minute,
		mods:  map[string]mod{"cpu.steal": set(25, 2), "loadavg.1m": add(1), "psi.cpu.some10": add(10)},
		procs: normalProcs, wantClass: []string{diagnosis.ClassCPU}, wantPlace: "宿主机"},
}

func (sc scenario) values(ts time.Time, faultStart time.Time, r *rand.Rand) map[string]float64 {
	v := make(map[string]float64, len(baseline)+4)
	keys := make([]string, 0, len(baseline))
	for k := range baseline {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 固定随机数消耗顺序，结果可复现
	for _, k := range keys {
		v[k] = baseline[k](ts, r)
	}
	if !ts.Before(faultStart) {
		prog := math.Min(1, float64(ts.Sub(faultStart))/float64(sc.faultLen))
		for _, k := range sortedKeys(sc.mods) {
			v[k] = sc.mods[k](v[k], prog, r)
		}
		for _, k := range sortedKeys(sc.added) {
			v[k] = sc.added[k](ts, r)
		}
	}
	derive(v)
	for k, x := range v {
		if x < 0 && !strings.HasPrefix(k, "mem.") {
			v[k] = 0
		}
	}
	return v
}

func sortedKeys[T any](m map[string]T) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func toSamples(v map[string]float64, ts time.Time) []collector.Sample {
	out := make([]collector.Sample, 0, len(v))
	for _, k := range sortedKeys(v) {
		out = append(out, collector.Sample{MetricID: k, TS: ts, Value: v[k]})
	}
	return out
}

type result struct {
	chain    *diagnosis.Chain
	latency  time.Duration // 故障开始到首次给出正确归因；-1 = 没给出
	evals    int           // 无故障场景：评估次数
	falsePos int           // 无故障场景：出了结论的评估次数
	fpTitles []string
}

func matches(sc scenario, it diagnosis.Item) bool {
	if len(sc.wantClass) == 0 || it.Class != sc.wantClass[0] {
		return false
	}
	if sc.wantPID != 0 {
		ok := false
		for _, c := range it.Culprits {
			if c.PID > 0 {
				ok = c.PID == sc.wantPID
				break
			}
		}
		if !ok {
			return false
		}
	}
	if sc.wantPlace != "" {
		ok := false
		for _, c := range it.Culprits {
			ok = ok || (c.PID == 0 && strings.Contains(c.Name, sc.wantPlace))
		}
		if !ok {
			return false
		}
	}
	return sc.wantTitle == "" || strings.Contains(it.Title, sc.wantTitle)
}

func runScenario(sc scenario) result {
	r := rand.New(rand.NewSource(42))
	now := time.Date(2026, 9, 11, 14, 30, 0, 0, time.UTC) // 不落在整点：原始层只回放 3 小时
	faultStart := now.Add(-sc.faultLen)
	rawStart := faultStart.Add(-3 * time.Hour)
	s := NewSeries()
	for ts := rawStart.Add(-8 * 24 * time.Hour); ts.Before(rawStart); ts = ts.Add(coarseStep) {
		s.AddCoarse(toSamples(sc.values(ts, faultStart, r), ts))
	}
	b := NewHeatmapBuilder(s)
	d := &Diagnoser{builder: b, series: s}
	diag := func(at time.Time) *diagnosis.Chain {
		procs := normalProcs
		if !at.Before(faultStart) {
			procs = sc.procs
		}
		return diagnosis.Diagnose(nil, d.latestDeviationsAt(at), diagnosis.Options{ZThreshold: 3, Procs: procs, Now: at})
	}
	res := result{latency: -1}
	lastSigma := time.Time{}
	for ts := rawStart; !ts.After(now); ts = ts.Add(5 * time.Second) {
		s.Add(toSamples(sc.values(ts, faultStart, r), ts))
		if ts.Sub(lastSigma) >= 5*time.Minute {
			b.RefreshSigma()
			lastSigma = ts
		}
		// 无故障场景：前 1 小时让 σ 稳定，之后每 30 秒评估一次，统计误报
		if len(sc.wantClass) == 0 && ts.Sub(rawStart) >= time.Hour && ts.Sub(rawStart)%(30*time.Second) == 0 {
			res.evals++
			for _, it := range diag(ts).Items {
				if it.Class != "" {
					res.falsePos++
					res.fpTitles = append(res.fpTitles, ts.Format("15:04:05")+" "+it.Title)
					break
				}
			}
		}
		// 故障期间每 30 秒评估一次，记录首次给出正确归因的时刻
		if !ts.Before(faultStart) && res.latency < 0 && ts.Sub(faultStart)%(30*time.Second) == 0 {
			for _, it := range diag(ts).Items {
				if matches(sc, it) {
					res.latency = ts.Sub(faultStart)
				}
			}
		}
	}
	res.chain = diag(now)
	return res
}

func TestFaultCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("replays 8 days × 8 scenarios")
	}
	pass := 0
	for _, sc := range scenarios {
		res := runScenario(sc)
		var classes []string
		var head string
		for _, it := range res.chain.Items {
			if it.Class != "" {
				classes = append(classes, it.Class)
				if head == "" {
					head = it.Title
				}
			}
		}
		ok := strings.Join(classes, ",") == strings.Join(sc.wantClass, ",") && res.chain.Count == len(sc.wantClass)
		if len(sc.wantClass) == 0 {
			hours := float64(res.evals) * 30 / 3600
			t.Logf("     %-26s 评估 %d 次（%.1f 小时），误报 %d 次 = %.2f 次/小时", sc.name, res.evals, hours, res.falsePos, float64(res.falsePos)/hours)
			for i, ft := range res.fpTitles {
				if i < 5 {
					t.Logf("       误报: %s", ft)
				}
			}
			ok = ok && res.falsePos == 0
		}
		if ok && len(sc.wantClass) > 0 {
			ok = res.latency >= 0 && matches(sc, res.chain.Items[0])
		}
		lat := "—"
		if res.latency >= 0 {
			lat = res.latency.String()
		}
		mark := "PASS"
		if !ok {
			mark = "FAIL"
		} else {
			pass++
		}
		t.Logf("%s %-26s 出结论 %-6s 结论数 %d  %s", mark, sc.name, lat, res.chain.Count, head)
		if !ok {
			for _, it := range res.chain.Items {
				t.Logf("      item: [%v] %s | culprits=%+v", it.Level, it.Title, it.Culprits)
			}
			t.Errorf("%s: want classes %v, got %v (count %d)", sc.name, sc.wantClass, classes, res.chain.Count)
		}
	}
	t.Logf("故障语料库：%d/%d 通过", pass, len(scenarios))
}
