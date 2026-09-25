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
	"math"
	"strings"
	"time"

	"github.com/githubflyideas/nodedata/internal/metrics"
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
	// Worst 是每一步里"最坏到过哪"（v5.20），只在跟 Values 差得够大时给，其余为 null。
	//
	// 曲线每一步只取一个点：24 小时窗口一步 6 分钟，等于 36 个原始点里取 1 个，
	// 一次 40 秒的打满大概率被跳过——原始数据明明在内存里，图上却看不到。
	// 页面在这一步画一根竖线，从取到的点连到最坏值。
	// 只给单一来源的曲线：合成的（CPU 忙碌 = 各项相加再除核数）各项的最坏值不在同一时刻，
	// 硬加起来会画出一个从没发生过的数。
	Worst []*float64 `json:"w,omitempty"`
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
	// 落到长期层的那段要用长期层的容差：长期层是 5 分钟一格，拿 30 秒的容差去配必然全空。
	// 重启后原始层清零，1h 窗口的曲线会整片空白——而那段时间的数据其实在磁盘上躺着。
	coarseTol := coarseStep / 2
	if coarseTol < tol {
		coarseTol = tol
	}
	rawStart := b.rawStartOf(compareDefs)
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
			// 合成指标要给全部来源：CPU 忙碌 % = user+sys+softirq，
			// 只写 def.ids[0] 会让放大图标题显示成 "cpu.user"，看的人以为只统计了用户态。
			s := chartSeries{Label: def.label, Unit: def.unit, Bad: def.bad,
				ID: strings.Join(def.ids, " + ")}
			any, anyWorst := false, false
			single := len(def.ids) == 1
			floor := 0.0
			if single {
				floor = metrics.Lookup(def.ids[0]).MinDelta
				if def.f != nil { // 门槛也按同样的换算走，否则跟换算后的值比不上口径
					floor = math.Abs(def.f([]float64{floor}) - def.f([]float64{0}))
				}
			}
			for ts := from; !ts.After(now); ts = ts.Add(step) {
				t := tol
				if rawStart.IsZero() || ts.Before(rawStart) {
					t = coarseTol // 这个时刻原始层还没有，只能靠长期层
				}
				v := b.valueAtTol(def, ts, t)
				s.TS = append(s.TS, ts.Unix())
				s.Values = append(s.Values, v)
				any = any || v != nil
				var w *float64
				if single && v != nil {
					if x, ok := b.series.Worst(def.ids[0], ts.Add(-step/2), ts.Add(step/2)); ok {
						if def.f != nil {
							x = def.f([]float64{x})
						}
						if math.Abs(x-*v) >= floor && floor > 0 {
							w = &x
							anyWorst = true
						}
					}
				}
				s.Worst = append(s.Worst, w)
			}
			if !anyWorst {
				s.Worst = nil // 整条都没有值得画的尖峰：不传
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

// rawStartOf 返回原始层最早的一个点的时刻（取几个常见指标的最小值）。
// 早于它的时刻只能从长期层取，容差必须按长期层的粒度放宽。
func (b *HeatmapBuilder) rawStartOf(groups []struct {
	name string
	rows []cmpDef
}) time.Time {
	var earliest time.Time
	for _, g := range groups {
		for _, d := range g.rows {
			for _, id := range d.ids {
				pts := b.series.RawRange(id, time.Time{}, farFuture)
				if len(pts) == 0 {
					continue
				}
				if earliest.IsZero() || pts[0].TS.Before(earliest) {
					earliest = pts[0].TS
				}
			}
		}
	}
	return earliest
}
