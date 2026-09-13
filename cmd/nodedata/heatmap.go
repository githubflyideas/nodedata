// heatmap.go — L2/L3：把内存序列 + 偏离度组装成 data/*.json 的 heatmap 响应。
package main

import (
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
		for lagIdx, lagSec := range deviation.LagSeconds {
			hist := raw
			if lagUsesCoarse(lagSec) {
				hist = coarse
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
		strings.HasPrefix(id, "proc.io."):
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
