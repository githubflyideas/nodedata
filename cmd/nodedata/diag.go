// diag.go — L4：把 L0 检查结果与 L3 偏离度喂给 diagnosis 包。
package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/githubflyideas/nodedata/cmd/nodedata/check"
	"github.com/githubflyideas/nodedata/internal/collector"
	"github.com/githubflyideas/nodedata/internal/deviation"
	"github.com/githubflyideas/nodedata/internal/diagnosis"
	"github.com/githubflyideas/nodedata/internal/server"
)

// Diagnoser 组装 L4 输入。
type Diagnoser struct {
	dataDir string
	series  *Series
	builder *HeatmapBuilder
	// procs 提供 L1 最近一轮进程快照，供 L4 归因到 PID；nil 时只能归因到设备/接口。
	procs func() []diagnosis.Proc
}

func NewDiagnoser(dataDir string, s *Series, b *HeatmapBuilder) *Diagnoser {
	return &Diagnoser{dataDir: dataDir, series: s, builder: b}
}

// Run 读取当前 L0 结果与最新一轮 L3 偏离度，返回诊断链。
func (d *Diagnoser) Run(zThreshold float64) *diagnosis.Chain {
	var procs []diagnosis.Proc
	if d.procs != nil {
		procs = d.procs()
	}
	return diagnosis.Diagnose(d.loadL0(), d.latestDeviations(), diagnosis.Options{
		ZThreshold: zThreshold,
		MaxItems:   50,
		Procs:      procs,
	})
}

// loadL0 从 data/check.json 读取 L0 结果并转成 diagnosis 的输入类型。
func (d *Diagnoser) loadL0() []diagnosis.L0Category {
	b, err := os.ReadFile(filepath.Join(d.dataDir, "check.json"))
	if err != nil {
		return nil
	}
	var fr check.FullResult
	if err := json.Unmarshal(b, &fr); err != nil {
		return nil
	}
	out := make([]diagnosis.L0Category, 0, len(fr.Categories))
	for _, c := range fr.Categories {
		dc := diagnosis.L0Category{Name: c.Name, Level: c.Level}
		for _, ck := range c.Checks {
			dc.Checks = append(dc.Checks, diagnosis.L0Check{
				ID: ck.ID, Name: ck.Name, Level: ck.Level, Message: ck.Message,
			})
		}
		out = append(out, dc)
	}
	return out
}

// latestDeviations 取每个指标最近一个点的十四档 z。
func (d *Diagnoser) latestDeviations() []diagnosis.Deviation {
	return d.latestDeviationsAt(time.Now())
}

func (d *Diagnoser) latestDeviationsAt(now time.Time) []diagnosis.Deviation {
	// 只要最后一个点：早期这里每次请求都 Build 整个 1h 窗口，而页面每 5 秒调一次。
	hm := d.builder.Latest(now)
	if hm == nil {
		return nil
	}
	out := make([]diagnosis.Deviation, 0, len(hm.Metrics))
	for _, m := range hm.Metrics {
		if len(m.Points) == 0 {
			continue
		}
		p := m.Points[len(m.Points)-1]
		z := sustainedZ(m.Points)
		// 低置信档位（同时段样本 < 20）不参与下结论：见 HeatmapBuilder.LowConfLags
		low := d.builder.LowConfLags(m.MetricID, now)
		for i := range z {
			if i < len(low) && low[i] {
				z[i] = math.NaN()
			}
		}
		onset := ""
		if p.OnsetLag != nil {
			onset = *p.OnsetLag
		}
		out = append(out, diagnosis.Deviation{
			MetricID: m.MetricID,
			Domain:   m.Domain,
			Unit:     m.Unit,
			Value:    p.V,
			Z:        z,
			OnsetLag: onset,
			Breadth:  p.Breadth,
		})
	}
	return out
}

// procsFromCollector 把采集器快照转成 diagnosis 的输入类型，并附上所属服务。
func procsFromCollector(col *collector.Collector, svc *ServiceLog) func() []diagnosis.Proc {
	return func() []diagnosis.Proc {
		ps, _ := col.TopProcs()
		out := make([]diagnosis.Proc, len(ps))
		for i, p := range ps {
			out[i] = diagnosis.Proc{PID: p.PID, Comm: p.Comm, Key: p.Key, State: p.State, CPU: p.CPU,
				ReadBps: p.ReadBps, WriteBps: p.WriteBps, MajFlt: p.MajFlt, RSS: p.RSS,
				RSSGrowth: p.RSSGrowth, GrowthSpan: p.GrowthSpan, Self: p.Self}
			if svc != nil {
				name, ports := svc.ServiceOf(p.PID)
				if name == "" {
					name, ports = svc.ServiceByKey(p.Key)
				}
				out[i].Service, out[i].Ports = name, ports
			}
		}
		return out
	}
}

// sustainedZ：每档取最近几个点里"同号且最小"的 |z|；任一点未就绪则该档未就绪，符号不一致记 0。
//
// 为什么：约 40 个指标 × 9 个就绪档 ≈ 360 个 z 格，单点 |z|≥3 纯靠噪声几乎每次评估都会出现一个。
// 白噪声的极值不会连续三个采样点同向出现，真实的变化会。L3 热力图照样显示单点尖峰，
// 只有下结论（以及随之的事故留证）才要求持续 15 秒。
func sustainedZ(pts []server.Point) []float64 {
	z := make([]float64, deviation.NLag)
	for i := range z {
		first := true
		for _, p := range pts {
			if p.Z[i] == nil {
				z[i] = math.NaN()
				break
			}
			v := float64(*p.Z[i]) / 20.0 // ZInt8 存的是 z×20
			switch {
			case first:
				z[i], first = v, false
			case v*z[i] <= 0:
				z[i] = 0
			case math.Abs(v) < math.Abs(z[i]):
				z[i] = v
			}
		}
	}
	return z
}
