// heatmap.go — L2/L3：把内存序列 + 偏离度组装成 data/*.json 的 heatmap 响应。
package main

import (
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/githubflyideas/nodedata/internal/deviation"
	"github.com/githubflyideas/nodedata/internal/server"
)

// maxPointsPerWindow 单个窗口输出的最大点数，超出则按步长降采样。
const maxPointsPerWindow = 240

// HeatmapBuilder 依据内存序列构造 HeatmapJSON。
type HeatmapBuilder struct {
	series *Series
	sigma  *deviation.SigmaTable
	dev    *deviation.Deviation
	host   string
	// procsFn 提供最近一轮进程快照；nil 时不输出。
	procsFn func() ([]server.ProcTop, time.Time)
	// groupsFn 提供进程组合计（浏览器/数据库的子进程合起来才是真正的占用）
	groupsFn func() []server.ProcGroup

	// frozen 是"处在异常中"的指标 → 异常起始时刻。见 MarkAnomaly。
	frozenMu sync.Mutex
	frozen   map[string]time.Time
}

// 基线排除（v4.3.0）。
//
// σ 每 5 分钟用最近历史重算，历史里**包含正在发生的故障**。尺度取 MAD / 四分位差 /
// 十分位差的最大者，而一个持续几天的阶跃故障会让十分位差张到故障幅度那么大：
// 实测一个持续 3 天的故障，峰值 z 从 4.45 掉到 −1.70 —— 机器还病着，报警自己没了。
//
// 对策：某指标一进入 L4 结论，就记下"异常从何时开始"，之后重算 σ 时把那之后的样本剔除，
// 基线仍是故障前的形状。
//
// 上限按**比例**而不是按时长：业务真的扩容了、流量真的涨一倍，那是新常态，该学进去。
// deviation.MaxExcludedFrac = 1/4，超过就不再排除。按时长封顶是行不通的——
// 试过 2 小时上限，3 天的故障里解冻 36 次，照样被污染干净。
//
// 只影响 σ 的样本选取，不改 z 的算法、不动任何页面或接口。

// anomalyBackdate 是异常起点的回溯量。
//
// 我们总是**事后**才发现故障：L4 每 15 秒跑一次、σ 每 5 分钟重算一次，从故障真正开始
// 到被标记，中间那几分钟的样本已经进了基线。实测这点污染就足以坏事——
// 标记晚 30 分钟（6 个长期层样本）就让十分位差张开，峰值 z 从 5.29 掉到 2.00。
// 多排除一小时历史的代价被 MaxExcludedFrac 兜住，不会失控。
const anomalyBackdate = time.Hour

// MarkAnomaly 记录这些指标的异常起点（已记过的不覆盖，保留最早的那次）。
func (b *HeatmapBuilder) MarkAnomaly(ids []string, since time.Time) {
	since = since.Add(-anomalyBackdate)
	if len(ids) == 0 {
		return
	}
	b.frozenMu.Lock()
	defer b.frozenMu.Unlock()
	if b.frozen == nil {
		b.frozen = make(map[string]time.Time, len(ids))
	}
	for _, id := range ids {
		if _, ok := b.frozen[id]; !ok {
			b.frozen[id] = since
		}
	}
}

// ClearAnomaly 指标恢复正常后清除标记（下一轮 σ 就会把这段学进来）。
func (b *HeatmapBuilder) ClearAnomaly(keep map[string]bool) {
	b.frozenMu.Lock()
	defer b.frozenMu.Unlock()
	for id := range b.frozen {
		if !keep[id] {
			delete(b.frozen, id)
		}
	}
}

func (b *HeatmapBuilder) anomalySince(id string) time.Time {
	b.frozenMu.Lock()
	defer b.frozenMu.Unlock()
	return b.frozen[id]
}

// AnomalyCount 供测试观察。
func (b *HeatmapBuilder) AnomalyCount() int {
	b.frozenMu.Lock()
	defer b.frozenMu.Unlock()
	return len(b.frozen)
}

func NewHeatmapBuilder(s *Series) *HeatmapBuilder {
	host, _ := os.Hostname()
	sigma := deviation.NewSigmaTable()
	dev := deviation.New(sigma)
	// 变化太小就不算异常：z 是尺度无关的，安静机器上 202 字节/秒也能是 6σ
	dev.MinDelta = minDeltaFor
	dev.LookupFn = s.Lookup
	return &HeatmapBuilder{series: s, sigma: sigma, dev: dev, host: host}
}

// RefreshSigma 用当前缓冲区历史重算 σ 表。每档 × 每小时桶。
//
// 档位取哪一层：配 v(t-H) 的容差 LagTolerance(H) 不小于长期层步长的一半时，
// 长期层里一定有足够近的点，就用长期层（L6=3h 及以上）；否则用原始层（L1–L5）。
// 长期层每天每小时均匀贡献 12 个样本、覆盖 14 天，σ 反映的是"很多天的这个时段"，
// 而不是被最近 24 小时的密集原始点主导；也不会因为重启而清零。
func (b *HeatmapBuilder) RefreshSigma() {
	toSamples := func(pts []point) []deviation.Sample {
		hist := make([]deviation.Sample, len(pts))
		for i, p := range pts {
			hist[i] = deviation.Sample{TS: p.TS, Value: p.V}
		}
		return hist
	}
	for _, id := range b.series.MetricIDs() {
		// 两层各自已按点数/保留期封顶，这里整层取出，不再按墙钟截。
		raw := toSamples(b.series.RawRange(id, time.Time{}, farFuture))
		coarse := toSamples(b.series.CoarseRange(id, time.Time{}, farFuture))
		rawSpan := spanOf(raw)
		for lagIdx, lagSec := range deviation.LagSeconds {
			hist := raw
			if lagUsesCoarse(lagSec) {
				hist = coarse
			} else if rawSpan < deviation.MinBaselineSpan {
				// 原始层只在内存里，重启即清空：L1–L5 全靠它，于是每次重启后
				// 短档位要瞎一个小时——而"突发抖动最先出现在 L1–L4"正是它们的用处，
				// 重启后的第一个小时恰恰最需要看。
				//
				// 长期层是落盘的、5 分钟一点，而 L1 的滞后正好是 300 秒：
				// 相邻两个长期层点恰好配成一对，容差 30 秒也盖得住采样抖动。
				// 所以原始层还没攒够跨度时，先用长期层顶上；
				// 攒够之后自动切回原始层（更密，尺度估得更准）。
				if spanOf(coarse) >= deviation.MinBaselineSpan {
					hist = coarse
				}
			}
			if len(hist) < 2 {
				continue
			}
			all := deviation.ComputeSigmaLagExcluding(lagSec, hist, b.anomalySince(id))
			for hour := 0; hour < 24; hour++ {
				b.sigma.Set(id, lagIdx, hour, all[hour])
			}
		}
	}
}

var farFuture = time.Unix(1<<62, 0)

// spanOf 返回一层历史覆盖的墙钟跨度。
func spanOf(hist []deviation.Sample) time.Duration {
	if len(hist) < 2 {
		return 0
	}
	return hist[len(hist)-1].TS.Sub(hist[0].TS)
}

// lagUsesCoarse 判断某档位的 σ 是否取长期层。
func lagUsesCoarse(lagSec int) bool {
	return deviation.LagTolerance(time.Duration(lagSec)*time.Second) >= coarseStep/2
}

// Prune 回收 24 小时没有新点的序列及其 σ（进程退出后的 proc.cpu.<comm> 等）。
func (b *HeatmapBuilder) Prune(now time.Time) int {
	gone := b.series.Prune(now.Add(-24 * time.Hour))
	for _, id := range gone {
		b.sigma.Delete(id)
	}
	return len(gone)
}

// Build 组装 [from, to] 区间的 heatmap。
func (b *HeatmapBuilder) Build(from, to time.Time) (*server.HeatmapJSON, error) {
	return b.build(from, to, false)
}

// sustainN：L4 要求偏离在最近几个采样点上持续。
const sustainN = 3

// Latest 只算每个指标最近 sustainN 个点（L4 用），不做整窗口的 z。
func (b *HeatmapBuilder) Latest(now time.Time) *server.HeatmapJSON {
	out, _ := b.build(now.Add(-time.Hour), now, true)
	return out
}

func (b *HeatmapBuilder) build(from, to time.Time, lastOnly bool) (*server.HeatmapJSON, error) {
	ids := b.series.MetricIDs()
	out := &server.HeatmapJSON{
		Host:        b.host,
		From:        from.Unix(),
		To:          to.Unix(),
		GeneratedAt: time.Now().Unix(),
		Degraded:    map[string]bool{},
		Lags:        make([]string, deviation.NLag),
		LagSeconds:  make([]int, deviation.NLag),
		LagReady:    make([]bool, deviation.NLag),
		LowConf:     []string{},
		PSIAlerts:   []server.PSIAlert{},
		Metrics:     make([]server.MetricPoints, 0, len(ids)),
		Rules:       []server.RuleHit{},
	}
	for i, sec := range deviation.LagSeconds {
		out.Lags[i] = deviation.LagID(i)
		out.LagNames = append(out.LagNames, deviation.LagName(i))
		out.LagSeconds[i] = sec
		// 档位就绪 = 缓冲区历史跨度 ≥ 该档滞后时长
		out.LagReady[i] = to.Sub(from) >= time.Duration(sec)*time.Second
	}

	resolution := 0
	for _, id := range ids {
		var pts []point
		if lastOnly {
			// 最近 sustainN 个原始点（L4 用来判断偏离是否持续）
			pts = b.series.RawRange(id, to.Add(-time.Minute), to)
			if len(pts) > sustainN {
				pts = pts[len(pts)-sustainN:]
			}
			if n := len(pts); n > 0 && pts[n-1].TS.Before(from) {
				pts = nil
			}
		} else {
			pts = b.series.Range(id, from, to)
		}
		if len(pts) == 0 {
			continue
		}
		// 降采样时从末尾往前取，保证最后一个（当前）点一定在输出里 ——
		// 表格的"当前值"与各档 z 读的就是它。
		step := 1
		if len(pts) > maxPointsPerWindow {
			step = (len(pts) + maxPointsPerWindow - 1) / maxPointsPerWindow
		}
		first := (len(pts) - 1) % step
		if resolution == 0 && len(pts) >= 2 {
			resolution = int(pts[1].TS.Sub(pts[0].TS).Seconds()) * step
		}

		mp := server.MetricPoints{
			MetricID:  id,
			Domain:    domainOf(id),
			Unit:      unitOf(id),
			IsPrimary: primaryOf(id),
			Points:    make([]server.Point, 0, len(pts)/step+1),
		}
		for i := first; i < len(pts); i += step {
			p := pts[i]
			zf := b.dev.Z(id, p.V, p.TS)
			var zi [deviation.NLag]*int8
			for k := 0; k < deviation.NLag; k++ {
				if math.IsNaN(zf[k]) {
					continue // nil = 未就绪
				}
				v := deviation.ZInt8(zf[k])
				zi[k] = &v
			}
			onset, breadth := deviation.Classify(zf, 3.0)
			var onsetPtr *string
			if onset != "" {
				o := onset
				onsetPtr = &o
			}
			mp.Points = append(mp.Points, server.Point{
				TS:       p.TS.Unix(),
				V:        p.V,
				Z:        zi,
				OnsetLag: onsetPtr,
				Breadth:  breadth,
			})
		}
		out.Metrics = append(out.Metrics, mp)
	}
	if resolution == 0 {
		resolution = 5
	}
	out.Resolution = resolution
	out.Procs = []server.ProcTop{}
	if b.procsFn != nil {
		procs, ts := b.procsFn()
		if procs != nil {
			out.Procs = procs
		}
		if !ts.IsZero() {
			out.ProcsTS = ts.Unix()
		}
		if b.groupsFn != nil {
			out.Groups = b.groupsFn()
		}
	}
	return out, nil
}

// Health 供 data/health.json 使用。
func (b *HeatmapBuilder) Health(collectorHealth map[string]float64) server.HealthJSON {
	return server.HealthJSON{
		Host:        b.host,
		GeneratedAt: time.Now().Unix(),
		Health:      collectorHealth,
		Degraded:    map[string]bool{},
	}
}

// domainOf 由 metricID 前缀推断域。
func domainOf(id string) string {
	if i := strings.IndexByte(id, '.'); i > 0 {
		return id[:i]
	}
	return id
}

// unitOf 由 metricID 推断单位。/proc 里的内存类指标采集时已换算成字节。
// unitOf 决定某指标的显示单位。
//
// 顺序要紧：百分比规则必须排在 mem./swap. 前缀之前，否则 mem.used_pct（一个百分数）
// 会命中"mem. 开头 = 字节"，14.49% 被显示成 "14.49 B"。
// 落到 default 的后果同样具体：fs.avail 会走通用格式化，67 GiB 显示成 "67046.88M"。
func unitOf(id string) string {
	switch {
	case strings.HasSuffix(id, "_pct"), strings.Contains(id, "util"),
		strings.HasPrefix(id, "cpu."), strings.HasPrefix(id, "proc.cpu."),
		strings.HasPrefix(id, "psi"):
		return "percent"
	case strings.HasSuffix(id, "_ms"), strings.Contains(id, "await"):
		return "ms"
	case strings.HasSuffix(id, "_s"), strings.HasSuffix(id, "_sec"):
		return "s"
	// 速率类：吞吐（字节/秒）与事件/秒。@ 后面是设备或接口名。
	case strings.HasPrefix(id, "disk.rbytes"), strings.HasPrefix(id, "disk.wbytes"),
		id == "net.rx", id == "net.tx", strings.HasPrefix(id, "net.rx@"), strings.HasPrefix(id, "net.tx@"),
		strings.HasPrefix(id, "proc.io."),
		// 必须排在下面的 "swap. 开头 = 字节" 之前：swap.in/out 是速率不是存量，
		// 落到那条规则上 50MB/s 会显示成 "50 MiB"，看不出这台机器正在挨打。
		id == "swap.in", id == "swap.out":
		return "bytes/s"
	case strings.Contains(id, "_drop"), strings.Contains(id, "_errs"), strings.Contains(id, "_errors"),
		id == "tcp.retrans", id == "tcp.passive_opens", id == "tcp.attempt_fails",
		id == "udp.no_ports", id == "pgfault", id == "pgmajfault":
		return "/s"
	// 字节量：注意 fs.avail 与 proc.rss.<名字> 都不含 "bytes" 字样，必须显式列出
	case strings.Contains(id, "bytes"), strings.HasPrefix(id, "mem."),
		strings.HasPrefix(id, "swap."), strings.HasPrefix(id, "proc.rss."),
		id == "slab", id == "fs.avail":
		return "bytes"
	case strings.Contains(id, "iops"), strings.Contains(id, "pps"),
		strings.HasSuffix(id, "_per_s"):
		return "ops/s"
	case strings.HasPrefix(id, "loadavg"):
		return "load"
	default:
		return "count"
	}
}

// primaryOf 标记主视图指标。
func primaryOf(id string) int {
	switch domainOf(id) {
	case "cpu", "mem", "disk", "net", "psi", "loadavg", "swap",
		"procs_running", "procs_blocked", "slab":
		return 1
	default:
		return 0
	}
}

// LowConfLags 返回某指标各档位此刻是否"低置信"（同时段样本数 < HighConfN）。
//
// 一档只要攒够 MinDiffsForZ=8 个差分就算就绪，而 8 个样本估出的稳健尺度很不稳：
// 刚过门槛那阵子，稍有变动 z 就顶格，页面上是一整屏 ±6。
// L3 热力图照常显示（那是摆事实），但 L4 不该拿 8 个样本估出来的 σ 去下结论。
func (b *HeatmapBuilder) LowConfLags(id string, at time.Time) [deviation.NLag]bool {
	var out [deviation.NLag]bool
	hour := at.UTC().Hour()
	for i := 0; i < deviation.NLag; i++ {
		ls := b.sigma.Get(id, i, hour)
		out[i] = ls.Low
	}
	return out
}

// sigmaAt 取某指标某档位在当前小时桶的 σ 统计，供测试与自检使用。
func (b *HeatmapBuilder) sigmaAt(id string, lagIdx int, at time.Time) deviation.LagSigma {
	return b.sigma.Get(id, lagIdx, at.UTC().Hour())
}

// LagDiag 是单个档位的自检结果，用来回答"L1–L5 为什么没数"这类问题。
//
// z 为空只有两个原因：σ 没就绪，或者配不到 t−H 那个点。
// 这两件事的修法完全不同（前者等历史、后者等原始层攒够 H），
// 但页面上都显示成同一个斜纹格，看的人无从分辨。
type LagDiag struct {
	Lag          string `json:"lag"`
	Seconds      int    `json:"seconds"`
	Tier         string `json:"tier"`        // raw | coarse
	HistSpanS    int64  `json:"hist_span_s"` // 该层历史覆盖的跨度
	HistPoints   int    `json:"hist_points"` // 该层点数
	SigmaReady   int    `json:"sigma_ready"` // σ 就绪的指标数
	SigmaTotal   int    `json:"sigma_total"` // 参与统计的指标数
	SigmaN       int    `json:"sigma_n"`     // 样例指标当前小时桶的样本数
	LookupOK     int    `json:"lookup_ok"`   // 能配到 t−H 的指标数
	ZAvailable   int    `json:"z_available"` // 最终能出 z 的指标数
	SampleMetric string `json:"sample_metric"`
	Reason       string `json:"reason"` // 给排障的人看：σ、层、容差这些词都在
	Name         string `json:"name"`   // "3小时"
	Plain        string `json:"plain"`  // 给页面看的人话
}

// DiagnoseLags 逐档位说明"现在能不能出 z，不能的话卡在哪一步"。
func (b *HeatmapBuilder) DiagnoseLags(now time.Time) []LagDiag {
	ids := b.series.MetricIDs()
	sample := ""
	for _, id := range ids {
		if id == "cpu.user" || (sample == "" && strings.HasPrefix(id, "cpu.")) {
			sample = id
		}
	}
	if sample == "" && len(ids) > 0 {
		sample = ids[0]
	}
	rawSpan := int64(0)
	if pts := b.series.RawRange(sample, time.Time{}, farFuture); len(pts) > 1 {
		rawSpan = int64(pts[len(pts)-1].TS.Sub(pts[0].TS).Seconds())
	}
	out := make([]LagDiag, 0, deviation.NLag)
	for i, sec := range deviation.LagSeconds {
		// 必须和 RefreshSigma 的判断完全一致，否则自检本身会误导：
		// 原始层不够时只有在长期层够的前提下才回退，否则仍然用原始层。
		d := LagDiag{Lag: deviation.LagID(i), Seconds: sec, SampleMetric: sample, Tier: "raw"}
		coarseSpan := int64(0)
		if pts := b.series.CoarseRange(sample, time.Time{}, farFuture); len(pts) > 1 {
			coarseSpan = int64(pts[len(pts)-1].TS.Sub(pts[0].TS).Seconds())
		}
		minSpan := int64(deviation.MinBaselineSpan.Seconds())
		if lagUsesCoarse(sec) || (rawSpan < minSpan && coarseSpan >= minSpan) {
			d.Tier = "coarse"
		}
		pts := b.series.RawRange(sample, time.Time{}, farFuture)
		if d.Tier == "coarse" {
			pts = b.series.CoarseRange(sample, time.Time{}, farFuture)
		}
		d.HistPoints = len(pts)
		if len(pts) > 1 {
			d.HistSpanS = int64(pts[len(pts)-1].TS.Sub(pts[0].TS).Seconds())
		}
		tol := deviation.LagTolerance(time.Duration(sec) * time.Second)
		for _, id := range ids {
			d.SigmaTotal++
			if ls := b.sigmaAt(id, i, now); ls.Ready {
				d.SigmaReady++
				if id == sample {
					d.SigmaN = ls.N
				}
			}
			if _, ok := b.series.Lookup(id, now.Add(-time.Duration(sec)*time.Second), tol); ok {
				d.LookupOK++
			}
		}
		for _, m := range b.Latest(now).Metrics {
			if p := m.Points[len(m.Points)-1]; p.Z[i] != nil {
				d.ZAvailable++
			}
		}
		switch {
		case d.ZAvailable > 0:
			d.Reason = "正常"
		case d.SigmaReady == 0:
			d.Reason = fmt.Sprintf("σ 未就绪：%s 层历史只覆盖 %ds，需要 ≥%.0fs 且该小时桶 ≥%d 个同时段样本（还需约 %.0f 分钟；若 history/ 里有历史则重启后立即可用）",
				d.Tier, d.HistSpanS, deviation.MinBaselineSpan.Seconds(), deviation.MinDiffsForZ,
				(deviation.MinBaselineSpan.Seconds()-float64(d.HistSpanS))/60)
		case d.LookupOK == 0:
			d.Reason = fmt.Sprintf("配不到 %ds 前的点（容差 %v）：原始层还没攒够这么长，长期层是 %v 一格、对不上短档",
				sec, tol, coarseStep)
		default:
			d.Reason = "部分指标可出 z"
		}
		d.Name = deviation.LagName(i)
		switch {
		case d.ZAvailable > 0:
			d.Plain = ""
		case d.SigmaReady == 0:
			// 最常见的是长档：和 7天前比，要先有 14 天历史才知道"一周的变化"平时有多大
			d.Plain = fmt.Sprintf("和 %s前比：历史还不够（已有 %s），暂时判断不了变化算不算异常",
				d.Name, deviation.DurationName(int(d.HistSpanS)))
		case d.LookupOK == 0:
			d.Plain = fmt.Sprintf("和 %s前比：找不到那个时刻的数据（nodedata 那时还没在跑，或刚重启）", d.Name)
		}
		out = append(out, d)
	}
	return out
}
