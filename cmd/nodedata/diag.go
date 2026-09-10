// diag.go — L4：把 L0 检查结果与 L3 偏离度喂给 diagnosis 包。
package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/githubflyideas/nodedata/cmd/nodedata/check"
	"github.com/githubflyideas/nodedata/internal/deviation"
	"github.com/githubflyideas/nodedata/internal/diagnosis"
)

// Diagnoser 组装 L4 输入。
type Diagnoser struct {
	dataDir string
	series  *Series
	builder *HeatmapBuilder
}

func NewDiagnoser(dataDir string, s *Series, b *HeatmapBuilder) *Diagnoser {
	return &Diagnoser{dataDir: dataDir, series: s, builder: b}
}

// Run 读取当前 L0 结果与最新一轮 L3 偏离度，返回诊断链。
func (d *Diagnoser) Run(zThreshold float64) *diagnosis.Chain {
	return diagnosis.Diagnose(d.loadL0(), d.latestDeviations(), diagnosis.Options{
		ZThreshold: zThreshold,
		MaxItems:   50,
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
	// 只要最后一个点：早期这里每次请求都 Build 整个 1h 窗口，而页面每 5 秒调一次。
	hm := d.builder.Latest(time.Now())
	if hm == nil {
		return nil
	}
	out := make([]diagnosis.Deviation, 0, len(hm.Metrics))
	for _, m := range hm.Metrics {
		if len(m.Points) == 0 {
			continue
		}
		p := m.Points[len(m.Points)-1]
		z := make([]float64, deviation.NLag)
		for i := 0; i < deviation.NLag; i++ {
			if p.Z[i] == nil {
				z[i] = math.NaN() // nil = 该档未就绪
				continue
			}
			z[i] = float64(*p.Z[i]) / 20.0 // ZInt8 存的是 z×20
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
