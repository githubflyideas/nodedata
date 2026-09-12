// keychart.go — /api/keyseries：关键指标的曲线数据。
//
// "关键指标"页原来是一张对比表，与"OS 指标"页完全重复（同样的行、同样的列，只是少几行）。
// 改成曲线：表回答"变了多少"，曲线回答"怎么变的"——是阶跃、是爬坡、还是周期性尖峰。
// 这三种形状对应完全不同的排查方向，数字列看不出来。
//
// 组合指标（CPU 忙碌 % = user+sys+softirq 再按核数归一）必须在服务端合成：
// 各条序列的采样时刻不一定对齐，客户端逐点相加会错位。这里用与对比表同一套 cmpDef 定义，
// 保证曲线和表里的数字是同一个口径。
package main

import (
	"time"
)

// keyChartPoints 每条曲线的点数上限。240 个点在 1000px 宽度上已经密到看不出间断，
// 再多只是传输浪费。
const keyChartPoints = 240

type chartSeries struct {
	Label string  `json:"label"`
	Unit  string  `json:"unit"`
	Bad   int     `json:"bad"`
	ID    string  `json:"id"`
	TS    []int64 `json:"ts"`
	// Values 缺样本处为 nil → JSON null。必须用指针：encoding/json 不能编码 NaN，
	// 写成 float64 时整个响应编码失败（200 但空 body）。
	Values []*float64 `json:"v"`
}

type chartGroup struct {
	Name   string        `json:"name"`
	Series []chartSeries `json:"series"`
}

type KeySeriesJSON struct {
	At     int64        `json:"at"`
	Window string       `json:"window"`
	From   int64        `json:"from"`
	Groups []chartGroup `json:"groups"`
}

// KeySeries 返回关键指标在 [now-d, now] 上的曲线。
func (b *HeatmapBuilder) KeySeries(win string, d time.Duration, now time.Time) *KeySeriesJSON {
	from := now.Add(-d)
	step := d / keyChartPoints
	if step < 5*time.Second {
		step = 5 * time.Second
	}
	// 取点容差取半个步长：窗口越长步长越大，容差跟着放宽，否则长窗口会满是空洞。
	tol := step / 2
	if tol < 30*time.Second {
		tol = 30 * time.Second
	}
	out := &KeySeriesJSON{At: now.Unix(), Window: win, From: from.Unix()}
	// 进程组是动态的（取占用最高的几个），和对比表用同一份定义。
	// 内存泄漏在曲线上是一条持续爬坡的线，这是数字列最看不出来的形状。
	groups := append([]struct {
		name string
		rows []cmpDef
	}(nil), compareDefs...)
	if pr := b.procRows(now, b.series.MetricIDs()); len(pr) > 0 {
		groups = append(groups, struct {
			name string
			rows []cmpDef
		}{"进程", pr})
	}
	for _, g := range groups {
		grp := chartGroup{Name: g.name}
		for _, def := range g.rows {
			if !def.core {
				continue // 曲线只画关键项；全部采集项在 OS 指标页
			}
			s := chartSeries{Label: def.label, Unit: def.unit, Bad: def.bad, ID: def.ids[0]}
			any := false
			for ts := from; !ts.After(now); ts = ts.Add(step) {
				v := b.valueAtTol(def, ts, tol)
				s.TS = append(s.TS, ts.Unix())
				s.Values = append(s.Values, v)
				any = any || v != nil
			}
			if any { // 本机没有这项指标时整条不输出
				grp.Series = append(grp.Series, s)
			}
		}
		if len(grp.Series) > 0 {
			out.Groups = append(out.Groups, grp)
		}
	}
	return out
}

// valueAtTol 与对比表的取值口径相同（含组合指标的合成），只是容差可调。
func (b *HeatmapBuilder) valueAtTol(d cmpDef, at time.Time, tol time.Duration) *float64 {
	vals := make([]float64, 0, len(d.ids))
	for _, id := range d.ids {
		v, ok := b.series.Lookup(id, at, tol)
		if !ok || isNaN(v) {
			return nil
		}
		vals = append(vals, v)
	}
	x := vals[0]
	if d.f != nil {
		x = d.f(vals)
	}
	return &x
}
