// heatmap.go — L2/L3：把内存序列 + 偏离度组装成 data/*.json 的 heatmap 响应。
package main

import (
	"math"
	"os"
	"strings"
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
}

func NewHeatmapBuilder(s *Series) *HeatmapBuilder {
	host, _ := os.Hostname()
	sigma := deviation.NewSigmaTable()
	dev := deviation.New(sigma)
	dev.LookupFn = s.Lookup
	return &HeatmapBuilder{series: s, sigma: sigma, dev: dev, host: host}
}

// RefreshSigma 用当前缓冲区历史重算 σ 表。每档 × 每小时桶。
func (b *HeatmapBuilder) RefreshSigma() {
	now := time.Now()
	for _, id := range b.series.MetricIDs() {
		pts := b.series.Range(id, now.Add(-30*24*time.Hour), now)
		if len(pts) < 2 {
			continue
		}
		hist := make([]deviation.Sample, len(pts))
		for i, p := range pts {
			hist[i] = deviation.Sample{TS: p.TS, Value: p.V}
		}
		for lagIdx, lagSec := range deviation.LagSeconds {
			for hour := 0; hour < 24; hour++ {
				ls, err := deviation.ComputeSigma(lagSec, hour, hist)
				if err != nil {
					continue
				}
				b.sigma.Set(id, lagIdx, hour, ls)
			}
		}
	}
}

// Build 组装 [from, to] 区间的 heatmap。
func (b *HeatmapBuilder) Build(from, to time.Time) (*server.HeatmapJSON, error) {
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
		pts := b.series.Range(id, from, to)
		if len(pts) == 0 {
			continue
		}
		step := 1
		if len(pts) > maxPointsPerWindow {
			step = (len(pts) + maxPointsPerWindow - 1) / maxPointsPerWindow
		}
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
		for i := 0; i < len(pts); i += step {
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
	return "other"
}

// unitOf 由 metricID 后缀推断单位。
func unitOf(id string) string {
	switch {
	case strings.HasSuffix(id, "_ms"), strings.Contains(id, "await"):
		return "ms"
	case strings.HasSuffix(id, "_s"):
		return "s"
	case strings.Contains(id, "bytes"):
		return "bytes"
	case strings.Contains(id, "iops"), strings.Contains(id, "pps"):
		return "ops/s"
	case strings.Contains(id, "util"), strings.HasPrefix(id, "cpu."),
		strings.Contains(id, "psi"):
		return "percent"
	default:
		return "count"
	}
}

// primaryOf 标记主视图指标。
func primaryOf(id string) int {
	switch domainOf(id) {
	case "cpu", "mem", "disk", "net", "psi", "load":
		return 1
	default:
		return 0
	}
}
