// 整机偏离度快照 — 验收测试
//
// 实施方须在交付前跑通全部用例并提交输出：
//
//     go test -v -run 'T_' ./... 2>&1 | tee acceptance.log
//     go test -v -run 'T_PERF' -benchmem ./...
//     sudo ./run_strace_test.sh          # T_SAFE_01 需要
//
// 本文件定义的是**契约**，不是实现建议。函数签名可按实际代码调整，
// 但每个用例考察的性质不得放宽。放宽任何一项须书面批准。
//
// 标注 [硬] 的用例不可协商，未通过即验收不通过。

package acceptance

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ════════════════════════════════════════════════════════════
// 被测接口（实施方实现，签名可调整）
// ════════════════════════════════════════════════════════════

type Sample struct {
	MetricID string
	TS       time.Time
	Value    float64
}

// LagSigma 是某个 (metric, lag, hour) 组合的尺度常数。
type LagSigma struct {
	Sigma float64
	N     int  // 参与计算的样本数
	Ready bool // 数据长度 >= H
	Low   bool // N < 20，低置信度
}

type Collector interface {
	// CollectGlobal 执行一轮全局采集。procRoot 用于测试时注入假 /proc。
	CollectGlobal(procRoot string, now time.Time) ([]Sample, error)
	// CollectProcs 执行一轮进程采集。与全局同周期。
	CollectProcs(procRoot string, now time.Time) ([]Sample, error)
	// Interval 返回当前生效的采样周期（降级后会变）。
	Interval() time.Duration
	// OpenedPaths 返回本进程生命周期内打开过的所有路径（测试用）。
	OpenedPaths() []string
	// Health 返回自监控指标当前值。
	Health() map[string]float64
}

// NLag 是档位总数。顺序与 LagSeconds 一致。
const NLag = 14

// LagSeconds 是十四档的滞后时长，单位秒。
// L9..L14 必须是 86400 的整数倍 —— 这使其自动成为同槽比对。
var LagSeconds = [NLag]int{
	300, 600, 1200, 2400, 5400, 10800, 21600, 43200,
	86400, 172800, 345600, 604800, 1209600, 2419200,
}

type Deviation interface {
	// ComputeSigma 批算某档位在某小时桶上的 σ。hist 须按时间升序、覆盖 28 天。
	ComputeSigma(lag string, hour int, hist []Sample) (LagSigma, error)
	// Z 返回十四档 z 值，未就绪档位返回 NaN。
	Z(metricID string, v float64, at time.Time) [NLag]float64
	// Classify 返回 onset_lag (L1..L14 或 "") 与 breadth。
	// z 中的 NaN 表示该档位未就绪，不计入 breadth。
	Classify(z [NLag]float64, th float64) (onsetLag string, breadth int)
	// Cadence 返回 σ 表的重算周期。应为每日一次。
	SigmaRefreshInterval() time.Duration
}

// 由实施方在 TestMain 中注入
var (
	col Collector
	dev Deviation
)

// ════════════════════════════════════════════════════════════
// 组一：解析 T-PARSE-*  必须 100% 通过
// ════════════════════════════════════════════════════════════

// T_PARSE_01 基础解析正确性。
// testdata/procfs/normal/ 下是从真实机器抓取的 /proc 快照，
// expected.json 是逐字段的期望值。
func T_PARSE_01_GoldenSnapshot(t *testing.T) {
	root := "testdata/procfs/normal"
	got, err := col.CollectGlobal(root, time.Unix(1757116800, 0))
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	want := loadExpected(t, filepath.Join(root, "expected.json"))

	if len(got) != len(want) {
		t.Fatalf("指标数不符: got %d want %d", len(got), len(want))
	}
	for _, s := range got {
		w, ok := want[s.MetricID]
		if !ok {
			t.Errorf("产生了未定义指标 %s", s.MetricID)
			continue
		}
		if math.Abs(s.Value-w) > 1e-6 {
			t.Errorf("%s: got %v want %v", s.MetricID, s.Value, w)
		}
	}
}

// T_PARSE_02 counter 首轮不落库。
// 首次采集无前值，速率无法计算。必须丢弃该点，不得落 0 ——
// 落 0 会在热力图上产生一条虚假的"归零"，且污染基线。
func T_PARSE_02_CounterFirstRoundDropped(t *testing.T) {
	root := "testdata/procfs/normal"
	first, _ := col.CollectGlobal(root, time.Unix(1000, 0))
	for _, s := range first {
		if isCounter(s.MetricID) {
			t.Errorf("counter 指标 %s 在首轮就落库了（值 %v），应丢弃", s.MetricID, s.Value)
		}
	}
	second, _ := col.CollectGlobal(root, time.Unix(1001, 0))
	if len(second) <= len(first) {
		t.Error("第二轮应产生 counter 速率")
	}
}

// T_PARSE_03 counter 回绕不产生负值。
// testdata/procfs/wrap/ 的 counter 值小于 normal/，模拟 32 位溢出或设备重置。
func T_PARSE_03_CounterWrapNoNegative(t *testing.T) {
	col.CollectGlobal("testdata/procfs/normal", time.Unix(2000, 0))
	got, _ := col.CollectGlobal("testdata/procfs/wrap", time.Unix(2001, 0))
	for _, s := range got {
		if s.Value < 0 {
			t.Errorf("%s 回绕后产生负值 %v，应丢弃该点", s.MetricID, s.Value)
		}
	}
}

// T_PARSE_04 derived 指标分母为零时丢弃。
// disk.await = 累计等待时间差 / IOPS 差。空闲磁盘两次采样间 IOPS 差为 0。
func T_PARSE_04_DerivedZeroDenominator(t *testing.T) {
	col.CollectGlobal("testdata/procfs/idle_disk", time.Unix(3000, 0))
	got, _ := col.CollectGlobal("testdata/procfs/idle_disk", time.Unix(3001, 0))
	for _, s := range got {
		if s.MetricID == "disk.await_r" || s.MetricID == "disk.await_w" {
			t.Errorf("%s 在分母为 0 时仍落库（值 %v），应丢弃", s.MetricID, s.Value)
		}
		if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
			t.Errorf("%s 产生 NaN/Inf", s.MetricID)
		}
	}
}

// T_PARSE_05 畸形输入不 panic。
// testdata/procfs/malformed/ 含截断行、非数字字段、空文件、超长行。
func T_PARSE_05_MalformedInputNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("畸形输入导致 panic: %v", r)
		}
	}()
	for _, d := range []string{"truncated", "nonnumeric", "empty", "hugeline"} {
		root := filepath.Join("testdata/procfs/malformed", d)
		got, err := col.CollectGlobal(root, time.Now())
		if err == nil && len(got) == 0 {
			continue // 全部跳过是可接受的
		}
		for _, s := range got {
			if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
				t.Errorf("%s: %s 产生 NaN/Inf", d, s.MetricID)
			}
		}
	}
}

// T_PARSE_06 多设备聚合。
// testdata/procfs/multidisk/ 有 4 块磁盘、3 张网卡。
// 聚合值必须等于各设备之和；per-device 明细必须同时保留。
func T_PARSE_06_MultiDeviceAggregation(t *testing.T) {
	col.CollectGlobal("testdata/procfs/multidisk", time.Unix(4000, 0))
	got, _ := col.CollectGlobal("testdata/procfs/multidisk2", time.Unix(4001, 0))

	var agg float64
	var perDev float64
	var nDev int
	for _, s := range got {
		switch {
		case s.MetricID == "disk.wiops":
			agg = s.Value
		case len(s.MetricID) > 11 && s.MetricID[:11] == "disk.wiops@":
			perDev += s.Value
			nDev++
		}
	}
	if nDev != 4 {
		t.Errorf("per-device 明细数 got %d want 4", nDev)
	}
	if math.Abs(agg-perDev) > 1e-6 {
		t.Errorf("聚合值 %v 不等于 per-device 之和 %v", agg, perDev)
	}
}

// ════════════════════════════════════════════════════════════
// 组二：安全 T-SAFE-*  必须 100% 通过
// ════════════════════════════════════════════════════════════

// 禁读清单。读取任一路径即验收不通过 —— 这些文件的读取代价是 O(表长)，
// 在大表时会使采集器自身成为故障源。
var forbiddenPaths = []string{
	"/proc/net/nf_conntrack",
	"/proc/net/tcp",
	"/proc/net/tcp6",
	"/proc/net/udp",
	"/proc/net/udp6",
	"/proc/net/unix",
	"/proc/slabinfo",
	"/proc/kpageflags",
	"/proc/kpagecount",
}

// T_SAFE_01 [硬] 禁读清单。
// 本用例须同时以两种方式验证：
//   1) 代码内 OpenedPaths() 审计（本函数）
//   2) 外部 strace 验证（run_strace_test.sh），防止绕过审计层直接 syscall
func T_SAFE_01_ForbiddenPaths(t *testing.T) {
	for i := 0; i < 100; i++ {
		col.CollectGlobal("/proc", time.Now())
		col.CollectProcs("/proc", time.Now())
	}
	for _, p := range col.OpenedPaths() {
		for _, f := range forbiddenPaths {
			if p == f {
				t.Errorf("[硬] 读取了禁读路径 %s", f)
			}
		}
	}
}

// T_SAFE_02 conntrack 走 O(1) 的 sysctl。
func T_SAFE_02_ConntrackViaSysctl(t *testing.T) {
	col.CollectGlobal("/proc", time.Now())
	var found bool
	for _, p := range col.OpenedPaths() {
		if p == "/proc/sys/net/netfilter/nf_conntrack_count" {
			found = true
		}
	}
	if !found && hasConntrack() {
		t.Error("conntrack 数量未从 nf_conntrack_count 取得")
	}
}

// T_SAFE_03 PSI 缺失时降级而非崩溃。
// testdata/procfs/no_psi/ 不含 /proc/pressure/。
func T_SAFE_03_PSIMissingDegrades(t *testing.T) {
	got, err := col.CollectGlobal("testdata/procfs/no_psi", time.Now())
	if err != nil {
		t.Fatalf("PSI 缺失导致采集失败，应降级: %v", err)
	}
	for _, s := range got {
		if len(s.MetricID) > 4 && s.MetricID[:4] == "psi." {
			t.Errorf("PSI 不可用时仍落库了 %s", s.MetricID)
		}
	}
	if col.Health()["collector.psi_available"] != 0 {
		t.Error("psi_available 未置 0")
	}
}

// T_SAFE_04 无 CAP_NET_ADMIN 时正常启动。
func T_SAFE_04_NoCapNetAdminStarts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("需以非 root 运行")
	}
	if _, err := col.CollectGlobal("/proc", time.Now()); err != nil {
		t.Fatalf("非 root 下采集失败: %v", err)
	}
	if col.Health()["collector.taskstats_ok"] != 0 {
		t.Error("taskstats 不可用时未置 0")
	}
}

// T_SAFE_05 进程数超限时抽样而非全扫。
// testdata/procfs/many_procs/ 含 5000 个 PID 目录。
func T_SAFE_05_ProcSamplingUnderLoad(t *testing.T) {
	start := time.Now()
	got, err := col.CollectProcs("testdata/procfs/many_procs", time.Now())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("大量进程下采集失败: %v", err)
	}
	if budget := budgetFor(col.Interval()); elapsed > budget {
		t.Errorf("进程采集耗时 %v 超过预算 %v", elapsed, budget)
	}
	if len(got) > 25 {
		t.Errorf("单轮落库 %d 条，应 ≤ 25（4域×5 + others + 自监控）", len(got))
	}
	if col.Health()["collector.procs_skipped"] == 0 {
		t.Error("5000 进程下未启用抽样")
	}
}

// T_SAFE_06 超预算连续发生时自动降频。
func T_SAFE_06_BudgetOverrunDowngrades(t *testing.T) {
	root := "testdata/procfs/slow" // 通过 FUSE 或注入延迟模拟慢速 /proc
	before := col.Interval()
	for i := 0; i < 12; i++ {
		col.CollectGlobal(root, time.Now())
	}
	if col.Health()["collector.degraded"] != 1 {
		t.Error("连续超预算后未标记 degraded")
	}
	if col.Interval() <= before {
		t.Errorf("连续超预算后采样周期未加倍 (%v → %v)", before, col.Interval())
	}
}

// ════════════════════════════════════════════════════════════
// 组三：偏离度 T-LAG-*  必须 100% 通过
// ════════════════════════════════════════════════════════════

// T_LAG_01 σ 计算正确性。
func T_LAG_01_SigmaComputation(t *testing.T) {
	// 每 5 分钟增减 2.0 的锯齿信号：|Δ| 恒为 2，MAD = 0，
	// 因此 σ 应由 0.02×|median| 或绝对下限决定，而非 0。
	cases := []struct {
		name  string
		diffs []float64
		want  float64
	}{
		{"恒定变化量", []float64{2, 2, 2, 2, 2}, 0.04},        // 0.02 × med(2)
		{"正态变化", []float64{-1, 0, 1, 0, -1}, 1.4826},      // 1.4826 × MAD(1)
		{"恒零", []float64{0, 0, 0, 0, 0}, 1e-6},              // 绝对下限
		{"含离群", []float64{1, 1, 1, 1, 100}, 0.02},          // 离群不影响 MAD
	}
	for _, c := range cases {
		ls, err := dev.ComputeSigma("L1", 0, diffsToSamples(c.diffs))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if math.Abs(ls.Sigma-c.want) > 1e-4 {
			t.Errorf("%s: sigma got %v want %v", c.name, ls.Sigma, c.want)
		}
	}
}

// T_LAG_02 [硬] sigma 下限防止假阳性泛滥。
// net.rx_drop 常年为 0，滞后差恒为 0，MAD = 0。
// 无下限时第一个非零变化即 ∞σ，整张热力图被假阳性填满 ——
// 这是产品级失效，不是边界情况。
func T_LAG_02_SigmaFloor(t *testing.T) {
	zeros := make([]float64, 8064) // 28 天 × 每 5 分钟
	ls, _ := dev.ComputeSigma("L1", 0, diffsToSamples(zeros))
	if ls.Sigma <= 0 {
		t.Fatal("[硬] sigma 为 0 或负")
	}
	z := 1.0 / ls.Sigma
	if math.IsInf(z, 0) || math.Abs(z) > 6.0001 {
		t.Errorf("[硬] 全零 σ 下单个变化产生 z = %v，未被 clamp 到 ±6", z)
	}
}

// T_LAG_03 [硬] 起病时刻定位。
//
// 滞后差分的核心性质：故障持续超过 H 之后 z_H 自动归零，
// 因此最左亮格即故障起始时刻。这是十四档存在的理由（§6.4）。
// onset_lag 必须落在包含实际起始时刻的档位上，误差不超过一档。
func T_LAG_03_OnsetLocalization(t *testing.T) {
	cases := []struct {
		elapsed  time.Duration // 故障已持续多久
		wantLag  string
		neighbor []string // 允许的相邻档
	}{
		{3 * time.Minute, "L1", []string{"L2"}},
		{8 * time.Minute, "L2", []string{"L1", "L3"}},
		{15 * time.Minute, "L3", []string{"L2", "L4"}},
		{35 * time.Minute, "L4", []string{"L3", "L5"}},
		{70 * time.Minute, "L5", []string{"L4", "L6"}},
		{2 * time.Hour, "L6", []string{"L5", "L7"}},
		{5 * time.Hour, "L7", []string{"L6", "L8"}},
		{20 * time.Hour, "L9", []string{"L8", "L10"}},
		{36 * time.Hour, "L10", []string{"L9", "L11"}},
		{3 * 24 * time.Hour, "L11", []string{"L10", "L12"}},
	}
	for _, c := range cases {
		z := simulateFault(c.elapsed, 40*time.Hour*24)
		onset, _ := dev.Classify(z, 3.0)
		ok := onset == c.wantLag
		for _, n := range c.neighbor {
			if onset == n {
				ok = true
			}
		}
		if !ok {
			t.Errorf("[硬] 故障已持续 %v，onset_lag = %q，期望 %q（或相邻档 %v）",
				c.elapsed, onset, c.wantLag, c.neighbor)
		}
	}
}

// T_LAG_04 [硬] σ 必须按小时分桶。
//
// v(t) − v(t−3h) 中混着自然的日周期变化。凌晨三点的 3 小时变化量与
// 早高峰的 3 小时变化量量级完全不同。用同一个 σ 会两头都不准：
// 凌晨假阳性泛滥，早高峰完全失灵。且不报错、不崩溃 —— 静默失效。
func T_LAG_04_SigmaBucketedByHour(t *testing.T) {
	// 日周期振幅 3 倍、噪声很小的合成指标
	hist := buildDiurnalAmplitude(3.0, 0.02, 28*24*time.Hour)

	// 同一档位在不同小时桶上的 σ 必须显著不同
	quiet, _ := dev.ComputeSigma("L6", 3, hist)  // 03:00 平稳段
	ramp, _ := dev.ComputeSigma("L6", 9, hist)   // 09:00 爬升段
	if ratio := ramp.Sigma / quiet.Sigma; ratio < 2.0 {
		t.Errorf("[硬] L6 在 09:00 与 03:00 的 σ 比值仅 %.2f，应 > 2。"+
			"σ 未按小时分桶，日内段将全线失准", ratio)
	}

	// 凌晨的假阳性率
	var fp, n int
	for d := 0; d < 28; d++ {
		for m := 0; m < 60; m++ {
			at := dayHourMinute(d, 3, m)
			z := dev.Z("test.metric", diurnalValue(at), at)
			onset, _ := dev.Classify(z, 3.0)
			if onset != "" {
				fp++
			}
			n++
		}
	}
	if rate := float64(fp) / float64(n); rate > 0.01 {
		t.Errorf("[硬] 凌晨假阳性率 %.1f%%，应 < 1%%", rate*100)
	}
}

// T_LAG_05 [硬] 对齐段必须是 24h 整数倍。
// 这使 L9–L14 自动成为同槽比对，不受日周期影响。
// 若某档被设成 80h 之类，日周期会渗入该档，表现为该档灵敏度异常低。
func T_LAG_05_AlignedLagsAreDayMultiples(t *testing.T) {
	for i := 8; i < NLag; i++ { // L9 起
		if LagSeconds[i]%86400 != 0 {
			t.Errorf("[硬] L%d = %ds 不是 86400 的整数倍，对齐段被破坏",
				i+1, LagSeconds[i])
		}
	}
	// 对齐段对纯日周期信号应无响应
	hist := buildDiurnalAmplitude(3.0, 0.0, 40*24*time.Hour)
	at := time.Now()
	z := dev.Z("test.metric", diurnalValue(at), at)
	for i := 8; i < NLag; i++ {
		if !math.IsNaN(z[i]) && math.Abs(z[i]) > 1.0 {
			t.Errorf("[硬] 纯日周期信号在 L%d 上产生 z=%.2f，应接近 0", i+1, z[i])
		}
	}
	_ = hist
}

// T_LAG_06 [硬] 恒定故障盲区由 PSI 绝对阈值覆盖。
//
// 滞后差分只能检测"变了"，不能检测"一直是坏的"。
// 一台 CPU 打满一个月的机器，任何滞后差都是 0，十四档全部沉默。
// 这个盲区必须由 PSI 绝对阈值补上，且在 UI 明示。
func T_LAG_06_ConstantFaultBlindSpot(t *testing.T) {
	// CPU 恒定打满 40 天
	hist := buildConstant(100.0, 40*24*time.Hour)
	at := time.Now()
	z := dev.Z("cpu.user", 100.0, at)

	// 确认盲区确实存在（这是设计的已知性质，不是 bug）
	for i := range z {
		if !math.IsNaN(z[i]) && math.Abs(z[i]) >= 3 {
			t.Errorf("恒定故障不应触发滞后档 L%d (z=%.2f)", i+1, z[i])
		}
	}
	// PSI 绝对阈值必须补上
	alerts := psiAlerts(hist, at)
	if len(alerts) == 0 {
		t.Error("[硬] 恒定故障下 PSI 绝对阈值未触发，盲区无覆盖")
	}
	// UI 必须明示
	if !uiDeclaresBlindSpot() {
		t.Error("[硬] UI 帮助中未说明恒定故障盲区")
	}
}

// T_LAG_07 日周期不产生假阳性（全天）。
func T_LAG_07_DiurnalNoFalsePositive(t *testing.T) {
	var fp int
	for i := 0; i < 1440; i++ {
		at := time.Now().Add(time.Duration(i) * time.Minute)
		z := dev.Z("test.metric", diurnalValue(at), at)
		onset, _ := dev.Classify(z, 3.0)
		if onset != "" {
			fp++
		}
	}
	if rate := float64(fp) / 1440; rate > 0.01 {
		t.Errorf("日周期假阳性率 %.2f%%，应 < 1%%", rate*100)
	}
}

// T_LAG_08 冷启动：档位可用性只取决于数据长度。
// L1 在 5 分钟内即可用（临时排障部署时装上即用）。
// 不得因长档缺失而拒绝渲染。
func T_LAG_08_ColdStart(t *testing.T) {
	cases := []struct {
		age       time.Duration
		wantReady int // 前 N 档应就绪
	}{
		{3 * time.Minute, 0},
		{7 * time.Minute, 1},   // L1
		{15 * time.Minute, 2},  // L1-L2
		{45 * time.Minute, 4},  // L1-L4
		{2 * time.Hour, 5},     // L1-L5
		{7 * time.Hour, 7},     // L1-L7
		{25 * time.Hour, 9},    // L1-L9
		{8 * 24 * time.Hour, 12},
		{30 * 24 * time.Hour, 14},
	}
	for _, c := range cases {
		hist := buildDiurnal(c.age)
		for i := 0; i < NLag; i++ {
			ls, _ := dev.ComputeSigma(lagID(i), 0, hist)
			want := i < c.wantReady
			if ls.Ready != want {
				t.Errorf("运行 %v 时 %s.Ready = %v，want %v",
					c.age, lagID(i), ls.Ready, want)
			}
		}
	}
}

// T_LAG_09 低置信度标记。σ 样本数 < 20 时须标记 low_confidence。
func T_LAG_09_LowConfidenceFlag(t *testing.T) {
	hist := buildDiurnal(30 * 24 * time.Hour)
	// L14 = 28 天，28 天历史下只有约 2 个样本
	ls, _ := dev.ComputeSigma("L14", 0, hist)
	if ls.Ready && ls.N >= 20 {
		t.Skip("样本量足够，不适用")
	}
	if ls.Ready && !ls.Low {
		t.Errorf("L14 样本数 %d < 20 但未标记 low_confidence", ls.N)
	}
}

// T_LAG_10 onset_lag 与 breadth 的分类逻辑。
func T_LAG_10_Classification(t *testing.T) {
	n := math.NaN()
	hi, lo := 5.0, 1.0
	z := func(v ...float64) [NLag]float64 {
		var a [NLag]float64
		copy(a[:], v)
		return a
	}
	cases := []struct {
		name        string
		z           [NLag]float64
		wantOnset   string
		wantBreadth int
	}{
		{"40 分钟前起病",
			z(lo, lo, lo, hi, hi, hi, lo, lo, lo, lo, lo, lo, lo, lo), "L4", 3},
		{"刚发生",
			z(hi, hi, lo, lo, lo, lo, lo, lo, lo, lo, lo, lo, lo, lo), "L1", 2},
		{"周期性事件：左亮右全暗",
			z(hi, hi, hi, lo, lo, lo, lo, lo, lo, lo, lo, lo, lo, lo), "L1", 3},
		{"慢性劣化：左暗右亮",
			z(lo, lo, lo, lo, lo, lo, lo, lo, hi, hi, hi, hi, lo, lo), "L9", 4},
		{"全档触发",
			z(hi, hi, hi, hi, hi, hi, hi, hi, hi, hi, hi, hi, hi, hi), "L1", 14},
		{"正常",
			z(lo, lo, lo, lo, lo, lo, lo, lo, lo, lo, lo, lo, lo, lo), "", 0},
		{"负偏离同样触发",
			z(lo, lo, -hi, -hi, lo, lo, lo, lo, lo, lo, lo, lo, lo, lo), "L3", 2},
		{"冷启动：长档 NaN 不计入 breadth",
			z(hi, hi, lo, lo, n, n, n, n, n, n, n, n, n, n), "L1", 2},
	}
	for _, c := range cases {
		onset, breadth := dev.Classify(c.z, 3.0)
		if onset != c.wantOnset {
			t.Errorf("%s: onset got %q want %q", c.name, onset, c.wantOnset)
		}
		if breadth != c.wantBreadth {
			t.Errorf("%s: breadth got %d want %d", c.name, breadth, c.wantBreadth)
		}
	}
}

// T_LAG_12 [硬] v(t−H) 查找：就近取样，绝不插值。
//
// 采样周期可配，同一台机器的历史数据可能有不同密度。
// t−H 处不保证有精确匹配的样本。
// 插值出的参照点会产生虚假平滑，使 z 系统性偏小 ——
// 即掩盖故障，最坏的失效方向。
func T_LAG_12_LookupNearestNoInterpolation(t *testing.T) {
	iv := 30 * time.Second

	// 精确匹配
	if got := lookupAt(mixedDensityHistory(), targetExact(), iv); !got.Exact {
		t.Error("存在精确匹配时未命中")
	}

	// 容差内：取最近样本，其 value 必须等于某个真实存储值
	got := lookupAt(mixedDensityHistory(), targetWithinTolerance(), iv)
	if !got.Found {
		t.Error("容差内应能取到样本")
	} else if !isStoredValue(got.Value) {
		t.Errorf("[硬] 返回值 %v 不是任何已存样本，说明做了插值", got.Value)
	}

	// 容差外（数据缺口）：必须返回未找到，对应档 z 为 null
	if got := lookupAt(mixedDensityHistory(), targetInGap(), iv); got.Found {
		t.Errorf("[硬] 超出容差 ±%v 仍返回样本，该档 z 应为 null", iv)
	}
}

// T_LAG_13 变更采样周期不使已存 σ 失效。
// σ = median(|v(t) − v(t−H)|) 只依赖 H，不依赖采样周期。
func T_LAG_13_SigmaSurvivesIntervalChange(t *testing.T) {
	hist30 := buildDiurnalAtInterval(30*time.Second, 28*24*time.Hour)
	hist10 := buildDiurnalAtInterval(10*time.Second, 28*24*time.Hour)

	for _, lag := range []string{"L1", "L6", "L9", "L14"} {
		a, _ := dev.ComputeSigma(lag, 12, hist30)
		b, _ := dev.ComputeSigma(lag, 12, hist10)
		if rel := math.Abs(a.Sigma-b.Sigma) / math.Max(a.Sigma, 1e-9); rel > 0.15 {
			t.Errorf("%s 的 σ 随采样周期变化 %.0f%%（30s: %.4f, 10s: %.4f）。"+
				"σ 应只依赖 H", lag, rel*100, a.Sigma, b.Sigma)
		}
	}
}

// T_LAG_11 σ 表规模与刷新频率。
// 400 指标 × 14 档 × 24 小时 ≈ 134k float64 ≈ 1 MB，每日一次。
func T_LAG_11_SigmaTableSizeAndCadence(t *testing.T) {
	if iv := dev.SigmaRefreshInterval(); iv < 12*time.Hour {
		t.Errorf("σ 刷新周期 %v 过短，应为每日一次。"+
			"σ 是尺度常数，不需要频繁重算", iv)
	}
	if sz := sigmaTableBytes(); sz > 4<<20 {
		t.Errorf("σ 表 %d MB 过大，预期约 1 MB", sz>>20)
	}
}

// ════════════════════════════════════════════════════════════
// 组四：性能 T-PERF-*  必须 100% 通过
// ════════════════════════════════════════════════════════════

// T_PERF_01 全局采集单轮耗时。
func T_PERF_01_GlobalCollectLatency(t *testing.T) {
	col.CollectGlobal("/proc", time.Now())
	var worst time.Duration
	for i := 0; i < 1000; i++ {
		start := time.Now()
		col.CollectGlobal("/proc", time.Now())
		if d := time.Since(start); d > worst {
			worst = d
		}
	}
	if worst > 50*time.Millisecond {
		t.Errorf("全局采集最坏耗时 %v 超过 50ms", worst)
	}
}

// T_PERF_02 [硬] 零分配读取。
// 常驻进程每轮的堆分配必须接近零。累积的 garbage 会造成周期性 GC 抖动，
// 而抖动会出现在它自己观测的指标里。禁止 bufio.Scanner、strings.Split、
// fmt.Sscanf；须用预分配 buffer + bytes.IndexByte 手工切分。
func T_PERF_02_ZeroAllocCollect(t *testing.T) {
	col.CollectGlobal("/proc", time.Now())
	allocs := testing.AllocsPerRun(100, func() {
		col.CollectGlobal("/proc", time.Now())
	})
	if allocs > 20 {
		t.Errorf("[硬] 单轮堆分配 %.0f 次，上限 20。检查是否使用了 bufio.Scanner 或 strings.Split", allocs)
	}
}

// T_PERF_07 [硬] CPU 占用上限。
// 采集器是常驻进程，运行在被观测的机器上。占用越低，
// 它越不会改变自己观测的对象。资源占用是硬约束，
// 不是可以用存储余量换的东西。
func T_PERF_07_CPUBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("需长时运行")
	}
	for _, iv := range []time.Duration{10 * time.Second, 30 * time.Second, 60 * time.Second} {
		frac := measureCPUFraction(iv, 10*time.Minute)
		if frac > 0.01 {
			t.Errorf("[硬] 周期 %v 下 CPU 占用 %.2f%%，超过 1%% 单核上限", iv, frac*100)
		}
		if iv == 30*time.Second && frac > 0.002 {
			t.Errorf("默认周期下 CPU 占用 %.3f%%，超过 0.2%% 目标", frac*100)
		}
	}
}

// T_PERF_08 采样周期可配与边界。
func T_PERF_08_IntervalConfigurable(t *testing.T) {
	for _, iv := range []time.Duration{10 * time.Second, 30 * time.Second, 300 * time.Second} {
		if err := startWithInterval(iv); err != nil {
			t.Errorf("周期 %v 应被接受: %v", iv, err)
		}
	}
	for _, iv := range []time.Duration{time.Second, 5 * time.Second, 600 * time.Second} {
		if err := startWithInterval(iv); err == nil {
			t.Errorf("周期 %v 超出 [10s, 300s]，应拒绝启动", iv)
		}
	}
}

// T_PERF_03 静态转储：页面加载零计算。
// 转储由采集器周期性写出，页面只 fetch 静态文件。
// 本用例考察转储本身的耗时与原子性，不考察页面加载延迟 ——
// 后者按设计就是一次文件读取。
func T_PERF_03_StaticDump(t *testing.T) {
	if testing.Short() {
		t.Skip("需预置数据集")
	}
	dir := t.TempDir()

	start := time.Now()
	dumpAll(dir)
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("全部视图转储耗时 %v，超过 30 秒转储周期的三分之一", d)
	}

	for _, f := range []string{"1h.json", "6h.json", "24h.json", "7d.json", "30d.json"} {
		st, err := os.Stat(filepath.Join(dir, "data", f))
		if err != nil {
			t.Errorf("%s 未生成: %v", f, err)
			continue
		}
		if st.Size() > 8<<20 {
			t.Errorf("%s 为 %d MB，过大。考虑将 z 数组打包为 base64 二进制",
				f, st.Size()>>20)
		}
	}
}

// T_PERF_06 转储必须原子。
// 直接覆写会让页面读到半截 JSON。须写临时文件后 rename。
func T_PERF_06_AtomicDump(t *testing.T) {
	dir := t.TempDir()
	dumpAll(dir)

	done := make(chan struct{})
	var corrupt int
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			if !validJSON(filepath.Join(dir, "data", "1h.json")) {
				corrupt++
			}
		}
	}()
	for i := 0; i < 20; i++ {
		dumpAll(dir)
	}
	<-done
	if corrupt > 0 {
		t.Errorf("[硬] 并发读取到 %d 次残缺 JSON。转储须写临时文件后 rename", corrupt)
	}
}

// T_PERF_04 常驻内存。
func T_PERF_04_ResidentMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("需长时运行")
	}
	rss := runFor(2 * time.Hour)
	if rss > 40<<20 {
		t.Errorf("常驻 RSS %d MB 超过 40 MB", rss>>20)
	}
}

// T_PERF_05 容量守护生效。
// 预置 1.2 GB 数据，守护须在 10 分钟内降至 800 MB 以下，
// 且 baseline 表不得被淘汰。
func T_PERF_05_StorageWatchdog(t *testing.T) {
	if testing.Short() {
		t.Skip("需预置数据集")
	}
	seedData(1200 << 20)
	blBefore := countRows("baseline")

	runWatchdog()

	if used := diskUsed(); used > 900<<20 {
		t.Errorf("守护执行后仍占用 %d MB", used>>20)
	}
	if countRows("baseline") != blBefore {
		t.Error("[硬] baseline 表被淘汰，将导致 7 天冷启动")
	}
	if usedMutation() {
		t.Error("[硬] 使用了 ALTER DELETE。必须用 DROP PARTITION —— " +
			"mutation 会重写整个 part，容量吃紧时反而撑爆磁盘")
	}
}

// ════════════════════════════════════════════════════════════
// 组五：端到端 T-E2E-*  必须 ≥ 90% 通过
// ════════════════════════════════════════════════════════════

// 黄金场景：每个场景是一段合成的多指标时序，标注了期望识别的域。
// testdata/scenarios/*.json
type Scenario struct {
	Name       string
	Samples    []Sample
	FaultStart time.Time
	FaultEnd   time.Time
	WantDomain []string // 期望识别的域，空表示不应触发
	WantOnset  string
}

// T_E2E_01 单域故障定位。
func T_E2E_01_SingleDomainFault(t *testing.T) {
	for _, sc := range loadScenarios(t, "testdata/scenarios/single_domain") {
		t.Run(sc.Name, func(t *testing.T) {
			got := detectDomains(sc)
			if !sameSet(got, sc.WantDomain) {
				t.Errorf("识别域 %v，期望 %v", got, sc.WantDomain)
			}
		})
	}
}

// T_E2E_02 跨域级联识别。
func T_E2E_02_CascadeDetection(t *testing.T) {
	for _, sc := range loadScenarios(t, "testdata/scenarios/cascade") {
		t.Run(sc.Name, func(t *testing.T) {
			got := detectDomains(sc)
			if len(got) < 3 {
				t.Errorf("级联场景仅识别 %d 个域 %v，应 ≥ 3", len(got), got)
			}
		})
	}
}

// T_E2E_03 慢性漂移识别。
// 6 小时内缓慢爬升的内存泄漏：H1 不应触发（每小时变化很小），
// H3/H4 必须触发，onset_class = chronic 或 creep。
func T_E2E_03_ChronicDriftDetection(t *testing.T) {
	for _, sc := range loadScenarios(t, "testdata/scenarios/drift") {
		t.Run(sc.Name, func(t *testing.T) {
			onset := detectOnset(sc)
			if onset != "chronic" && onset != "creep" {
				t.Errorf("漂移场景 onset = %q，应为 chronic 或 creep", onset)
			}
		})
	}
}

// T_E2E_04 [硬] 真故障与良性负载变化的区分。
// 这是整个产品的核心假设。低于 90% 意味着"一眼辨故障点"不成立，
// 须回到设计阶段而非继续实施。
//
// testdata/scenarios/mixed/ 各含一个真故障与一个良性事件（流量尖峰、
// 定时批处理、发版后的阶跃变化），标注在 WantDomain 中。
func T_E2E_04_FaultVsBenignDiscrimination(t *testing.T) {
	scenarios := loadScenarios(t, "testdata/scenarios/mixed")
	var correct int
	for _, sc := range scenarios {
		got := detectDomains(sc)
		if sameSet(got, sc.WantDomain) {
			correct++
		} else {
			t.Logf("误判: %s — 识别 %v，期望 %v", sc.Name, got, sc.WantDomain)
		}
	}
	rate := float64(correct) / float64(len(scenarios))
	if rate < 0.90 {
		t.Errorf("[硬] 区分准确率 %.1f%%，低于 90%% 门槛（%d/%d）",
			rate*100, correct, len(scenarios))
	}
}

// T_E2E_05 周期性事件不被误报为故障。
// 整点 cron 之类：H1 触发但 H3/H4 不触发，须标记 periodic。
func T_E2E_05_PeriodicNotAlerted(t *testing.T) {
	for _, sc := range loadScenarios(t, "testdata/scenarios/periodic") {
		t.Run(sc.Name, func(t *testing.T) {
			if !detectPeriodic(sc) {
				t.Error("周期性事件未标记 periodic，将成为假阳性来源")
			}
		})
	}
}

// T_E2E_06 是否存在"隐藏真故障"的危险失效。
//
// periodic 标记会降低视觉显著性。若真故障被误标为 periodic，
// 产品就在主动隐藏信号 —— 这比假阳性严重得多。
// 本项容忍度为 0：任何一例都不可接受。
func T_E2E_06_NoFaultHiddenAsPeriodic(t *testing.T) {
	for _, sc := range loadScenarios(t, "testdata/scenarios/mixed") {
		if len(sc.WantDomain) == 0 {
			continue // 良性事件，跳过
		}
		if detectPeriodic(sc) {
			t.Errorf("[硬] 真故障 %s 被标记为 periodic，产品在隐藏信号", sc.Name)
		}
	}
}

// ════════════════════════════════════════════════════════════
// 测试辅助（实施方实现）
// ════════════════════════════════════════════════════════════
