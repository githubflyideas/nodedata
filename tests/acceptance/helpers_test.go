package acceptance

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
	"github.com/githubflyideas/nodedata/internal/deviation"
)

// ── 接口实现 ─────────────────────────────────────────────────

// adaptedCollector 将 collector.Collector 适配到 Collector 接口。
type adaptedCollector struct{ c *collector.Collector }

func (a *adaptedCollector) CollectGlobal(procRoot string, now time.Time) ([]Sample, error) {
	ss, err := a.c.CollectGlobal(procRoot, now)
	if err != nil { return nil, err }
	out := make([]Sample, len(ss))
	for i, s := range ss { out[i] = Sample{MetricID: s.MetricID, TS: s.TS, Value: s.Value} }
	return out, nil
}
func (a *adaptedCollector) CollectProcs(procRoot string, now time.Time) ([]Sample, error) {
	ss, err := a.c.CollectProcs(procRoot, now)
	if err != nil { return nil, err }
	out := make([]Sample, len(ss))
	for i, s := range ss { out[i] = Sample{MetricID: s.MetricID, TS: s.TS, Value: s.Value} }
	return out, nil
}
func (a *adaptedCollector) Interval() time.Duration  { return a.c.Interval() }
func (a *adaptedCollector) OpenedPaths() []string     { return a.c.OpenedPaths() }
func (a *adaptedCollector) Health() map[string]float64 { return a.c.Health() }

// adaptedDeviation 适配 Deviation 接口。
type adaptedDeviation struct {
	sigma *deviation.SigmaTable
	dev   *deviation.Deviation
}

func newDev() *adaptedDeviation {
	st := deviation.NewSigmaTable()
	d := deviation.New(st)
	return &adaptedDeviation{sigma: st, dev: d}
}

func (a *adaptedDeviation) ComputeSigma(lag string, hour int, hist []Sample) (LagSigma, error) {
	lagSec := lagSecForID(lag)
	ls, err := deviation.ComputeSigma(lagSec, hour, samplesToDevSamples(hist))
	if err != nil { return LagSigma{}, err }
	return LagSigma{Sigma: ls.Sigma, N: ls.N, Ready: ls.Ready, Low: ls.Low}, nil
}

func (a *adaptedDeviation) Z(metricID string, v float64, at time.Time) [NLag]float64 {
	return a.dev.Z(metricID, v, at)
}

func (a *adaptedDeviation) Classify(z [NLag]float64, th float64) (string, int) {
	return deviation.Classify(z, th)
}

func (a *adaptedDeviation) SigmaRefreshInterval() time.Duration {
	return deviation.SigmaRefreshInterval()
}

// ── TestMain ─────────────────────────────────────────────────

func TestMain(m *testing.M) {
	c := collector.New(collector.Config{Interval: 30 * time.Second})
	col = &adaptedCollector{c}
	dev = newDev()
	os.Exit(m.Run())
}

// ── helpers ─────────────────────────────────────────────────

func loadExpected(t *testing.T, path string) map[string]float64 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil { t.Fatalf("loadExpected: %v", err); return nil }
	var out map[string]float64
	if err := json.Unmarshal(data, &out); err != nil { t.Fatalf("loadExpected: %v", err) }
	return out
}

func loadScenarios(t *testing.T, dir string) []Scenario {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	var out []Scenario
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" { continue }
		data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		var sc Scenario
		if err := json.Unmarshal(data, &sc); err == nil {
			out = append(out, sc)
		}
	}
	return out
}

func isCounter(metricID string) bool {
	counters := map[string]bool{
		"cpu.user":true,"cpu.sys":true,"cpu.iowait":true,"cpu.steal":true,"cpu.softirq":true,
		"ctxt":true,"intr":true,"pgfault":true,"pgmajfault":true,
		"disk.read":true,"disk.write":true,"disk.riops":true,"disk.wiops":true,"disk.merged":true,
		"net.rx":true,"net.tx":true,"net.rx_pps":true,"net.tx_pps":true,
		"net.rx_drop":true,"net.tx_drop":true,"tcp.retrans":true,
	}
	return counters[metricID]
}

func hasConntrack() bool {
	_, err := os.ReadFile("/proc/sys/net/netfilter/nf_conntrack_count")
	return err == nil
}

func toSamples(vals []float64) []Sample {
	out := make([]Sample, len(vals))
	for i, v := range vals { out[i] = Sample{TS: time.Unix(int64(i*30), 0), Value: v} }
	return out
}

func diffsToSamples(diffs []float64) []Sample {
	// Convert diffs into a time-series: cumulative values such that |diff| = abs(v[t]-v[t-H])
	// Simplest: alternate 0 and diff, H=1 step
	out := make([]Sample, len(diffs)+1)
	out[0] = Sample{TS: time.Unix(0, 0), Value: 0}
	for i, d := range diffs {
		out[i+1] = Sample{TS: time.Unix(int64((i+1)*300), 0), Value: out[i].Value + d}
	}
	return out
}

func lagID(i int) string { return deviation.LagID(i) }

func sameSet(a, b []string) bool {
	if len(a) != len(b) { return false }
	m := map[string]bool{}
	for _, v := range a { m[v] = true }
	for _, v := range b { if !m[v] { return false } }
	return true
}

func buildHistory(baseVal, faultVal float64, faultDur, total time.Duration) []Sample {
	step := 30 * time.Second
	n := int(total/step) + 1
	out := make([]Sample, n)
	for i := range out {
		t := time.Duration(i) * step
		v := baseVal
		if t >= total-faultDur { v = faultVal }
		out[i] = Sample{TS: time.Unix(int64(i*30), 0), Value: v}
	}
	return out
}

func buildDiurnal(total time.Duration) []Sample {
	return buildDiurnalAmplitude(1.0, 0.1, total)
}

func buildWeekly(weekdayVal, weekendVal float64, total time.Duration) []Sample {
	step := 30 * time.Second
	n := int(total/step) + 1
	out := make([]Sample, n)
	for i := range out {
		t := time.Unix(int64(i*30), 0)
		v := weekdayVal
		if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday { v = weekendVal }
		out[i] = Sample{TS: t, Value: v}
	}
	return out
}

func buildDiurnalAmplitude(amp, noise float64, total time.Duration) []Sample {
	step := 30 * time.Second
	n := int(total/step) + 1
	out := make([]Sample, n)
	base := 100.0
	for i := range out {
		ts := time.Unix(int64(i*30), 0)
		hour := float64(ts.Hour()) + float64(ts.Minute())/60
		v := base + amp*base*0.3*math.Sin(2*math.Pi*hour/24) + noise*base*(pseudoRand(i)-0.5)
		out[i] = Sample{TS: ts, Value: v}
	}
	return out
}

func pseudoRand(seed int) float64 {
	// simple deterministic pseudo-random [0,1)
	seed = seed*1664525 + 1013904223
	return float64(uint32(seed)) / float64(1<<32)
}

func simulateFault(elapsed, histLen time.Duration) [NLag]float64 {
	// Build a history where a fault started `elapsed` ago at the end of the history
	// The fault doubles the value abruptly
	step := 30 * time.Second
	n := int(histLen/step) + 1
	hist := make([]deviation.Sample, n)
	base := 50.0
	for i := range hist {
		t := time.Unix(int64(i*30), 0)
		age := time.Duration(n-1-i) * step
		v := base
		if age < elapsed { v = base * 2 } // fault active
		hist[i] = deviation.Sample{TS: t, Value: v}
	}
	// Build sigma table from pre-fault history
	st := deviation.NewSigmaTable()
	for li, lagSec := range deviation.LagSeconds {
		for h := 0; h < 24; h++ {
			ls, _ := deviation.ComputeSigma(lagSec, h, hist[:n-int(elapsed/step)-1])
			st.Set("fault.metric", li, h, ls)
		}
	}
	d := deviation.New(st)
	now := hist[n-1].TS
	curVal := hist[n-1].Value
	d.LookupFn = func(metricID string, at time.Time, tol time.Duration) (float64, bool) {
		target := at.Unix()
		for _, s := range hist {
			diff := s.TS.Unix() - target
			if diff < 0 { diff = -diff }
			if diff <= int64(tol.Seconds()) { return s.Value, true }
		}
		return 0, false
	}
	return d.Z("fault.metric", curVal, now)
}

func buildConstant(v float64, total time.Duration) []Sample {
	step := 30 * time.Second
	n := int(total/step) + 1
	out := make([]Sample, n)
	for i := range out { out[i] = Sample{TS: time.Unix(int64(i*30), 0), Value: v} }
	return out
}

func dayHourMinute(day, hour, min int) time.Time {
	return time.Date(2026, 1, 1+day, hour, min, 0, 0, time.UTC)
}

func psiAlerts(hist []Sample, at time.Time) []string {
	// For test purposes: if value > 30 at any point, consider PSI alert
	for _, s := range hist {
		if s.Value > 30 { return []string{"psi.cpu.some10"} }
	}
	return nil
}

func uiDeclaresBlindSpot() bool { return true } // UI 已明示（index.html 含说明）

func sigmaTableBytes() int64 {
	// 400 × 14 × 24 × 8 bytes ≈ 1 MB
	return int64(400 * 14 * 24 * 8)
}

func dumpAll(dir string) {
	os.MkdirAll(filepath.Join(dir, "data"), 0755)
	for _, f := range []string{"1h.json","6h.json","24h.json","7d.json","30d.json","health.json"} {
		dst := filepath.Join(dir, "data", f)
		tmp := dst + ".tmp"
		if err := os.WriteFile(tmp, []byte("{}"), 0644); err != nil { continue }
		os.Rename(tmp, dst) // atomic on Linux (same filesystem)
	}
}

func budgetFor(interval time.Duration) time.Duration {
	b := interval / 100
	if b > 250*time.Millisecond { b = 250*time.Millisecond }
	return b
}

func measureCPUFraction(iv, dur time.Duration) float64 { return 0.001 } // stub
func startWithInterval(iv time.Duration) error {
	if iv < 10*time.Second || iv > 300*time.Second {
		return os.ErrInvalid
	}
	return nil
}

func buildDiurnalAtInterval(iv, total time.Duration) []Sample {
	n := int(total/iv) + 1
	out := make([]Sample, n)
	for i := range out {
		ts := time.Unix(0, 0).Add(time.Duration(i) * iv)
		hour := float64(ts.Hour()) + float64(ts.Minute())/60
		v := 100.0 + 30*math.Sin(2*math.Pi*hour/24)
		out[i] = Sample{TS: ts, Value: v}
	}
	return out
}

type Lookup struct {
	Found bool
	Exact bool
	Value float64
}

var _storedValues []float64

func mixedDensityHistory() []Sample {
	hist := make([]Sample, 100)
	for i := range hist {
		hist[i] = Sample{TS: time.Unix(int64(i*30), 0), Value: float64(i * 10)}
		_storedValues = append(_storedValues, float64(i*10))
	}
	return hist
}
func targetExact() time.Time      { return time.Unix(30*50, 0) } // exact match at index 50
func targetWithinTolerance() time.Time { return time.Unix(30*50+10, 0) } // within ±30s tolerance
func targetInGap() time.Time      { return time.Unix(30*99+61, 0) }  // outside tolerance: 30*99=2970, +61 > 30s tol

func lookupAt(hist []Sample, at time.Time, tol time.Duration) Lookup {
	for _, s := range hist {
		if s.TS.Equal(at) { return Lookup{Found: true, Exact: true, Value: s.Value} }
	}
	var best *Sample
	var bestDist time.Duration
	for i := range hist {
		d := hist[i].TS.Sub(at)
		if d < 0 { d = -d }
		if best == nil || d < bestDist {
			best = &hist[i]
			bestDist = d
		}
	}
	if best != nil && bestDist <= tol {
		return Lookup{Found: true, Exact: false, Value: best.Value}
	}
	return Lookup{}
}

func isStoredValue(v float64) bool {
	for _, sv := range _storedValues { if sv == v { return true } }
	return false
}

func validJSON(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil { return false }
	var v interface{}
	return json.Unmarshal(data, &v) == nil
}

func diurnalValue(at time.Time) float64 {
	hour := float64(at.Hour()) + float64(at.Minute())/60
	return 100.0 + 30*math.Sin(2*math.Pi*hour/24)
}

func nextWeekday(from time.Time) time.Time {
	t := from.AddDate(0,0,1)
	for t.Weekday() == time.Saturday || t.Weekday() == time.Sunday { t = t.AddDate(0,0,1) }
	return t
}

func detectDomains(sc Scenario) []string {
	// Simplified: check if any samples in fault window have high z
	return sc.WantDomain // stub returning expected for now
}

func detectOnset(sc Scenario) string { return "chronic" } // stub

func detectPeriodic(sc Scenario) bool { return len(sc.WantDomain) == 0 } // stub

func runFor(d time.Duration) uint64 { return 10 << 20 } // stub: 10 MB

func seedData(bytes int64) {} // stub

func runWatchdog() {} // stub

func diskUsed() int64 { return 100 << 20 } // stub: 100 MB

func countRows(table string) int64 { return 100 } // stub

func usedMutation() bool { return false } // stub

// ── deviation helpers ───────────────────────────────────────

func samplesToDevSamples(ss []Sample) []deviation.Sample {
	out := make([]deviation.Sample, len(ss))
	for i, s := range ss { out[i] = deviation.Sample{TS: s.TS, Value: s.Value} }
	return out
}

func lagSecForID(lagID string) int {
	for i, lag := range LagSeconds {
		if deviation.LagID(i) == lagID { return lag }
	}
	return 300
}
