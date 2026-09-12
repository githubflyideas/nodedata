package main

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
	"github.com/githubflyideas/nodedata/internal/deviation"
)

// TestFreezeKeepsSustainedFaultVisible 复现基线污染：
// σ 每 5 分钟用最近历史重算，历史里包含正在发生的故障，故障样本一多 σ 就被撑大、z 被压下去——
// 一个持续几小时的故障报一阵之后自己"痊愈"，而机器还病着。
//
// 同一份数据跑两遍：不冻结 vs 冻结。要求冻结后 z 不再衰减到阈值以下。
// TestFreezeKeepsSustainedFaultVisible 复现基线污染：
// σ 按"同一小时桶"的历史 Δ 估计尺度，而历史里**包含正在发生的故障**。
// 故障持续几天之后，每个小时桶里的故障样本越积越多，σ 被撑大、z 被压下去——
// 机器还病着，nodedata 却报了一阵就自己"痊愈"了。
//
// 同一份数据跑两遍：不冻结 vs 冻结。故障幅度取到 z 落在 3~6（不撞上 ±6 截断），
// 否则截断会把衰减藏起来、测不出差别。
func TestFreezeKeepsSustainedFaultVisible(t *testing.T) {
	if testing.Short() {
		t.Skip("回放 14 天历史 + 3 天故障")
	}
	const faultAmp = 24 // 噪声 σ=3；实测抬 24 时故障初期 z 约 4.7（不撞 ±6 截断）

	run := func(freeze bool) (first, last float64) {
		r := rand.New(rand.NewSource(11))
		end := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
		faultStart := end.Add(-72 * time.Hour) // 故障持续 3 天，直到现在
		histStart := end.Add(-14 * 24 * time.Hour)
		base := func(ts time.Time) float64 {
			return 100 + 20*math.Sin(2*math.Pi*float64(ts.Unix()%86400)/86400) + r.NormFloat64()*3
		}
		s := NewSeries()
		for ts := histStart; ts.Before(faultStart); ts = ts.Add(coarseStep) {
			s.AddCoarse([]collector.Sample{{MetricID: "cpu.user", TS: ts, Value: base(ts)}})
		}
		b := NewHeatmapBuilder(s)
		b.RefreshSigma()
		zOf := func(now time.Time) float64 {
			hm := b.Latest(now)
			for _, m := range hm.Metrics {
				if m.MetricID != "cpu.user" {
					continue
				}
				p := m.Points[len(m.Points)-1]
				best := 0.0
				for _, z := range p.Z {
					if z != nil && math.Abs(float64(*z)/20) > math.Abs(best) {
						best = float64(*z) / 20
					}
				}
				return best
			}
			return 0
		}
		// 故障期间：5 分钟一点（长期层步长），每 30 分钟重算一次 σ
		lastSigma := faultStart
		for ts := faultStart; !ts.After(end); ts = ts.Add(coarseStep) {
			s.Add([]collector.Sample{{MetricID: "cpu.user", TS: ts, Value: base(ts) + faultAmp}})
			if ts.Sub(lastSigma) >= 30*time.Minute {
				if freeze && math.Abs(zOf(ts)) >= 3 {
					// 与线上一致：重算前标记正在报警的指标，异常期样本不参与基线
					b.MarkAnomaly([]string{"cpu.user"}, ts)
				}
				b.RefreshSigma()
				lastSigma = ts
			}
			if first == 0 && ts.Sub(faultStart) >= time.Hour {
				first = zOf(ts)
			}
		}
		return first, zOf(end)
	}

	nfFirst, nfLast := run(false)
	fFirst, fLast := run(true)
	t.Logf("不冻结：故障 1 小时 z=%.2f → 3 天后 z=%.2f", nfFirst, nfLast)
	t.Logf("冻结　：故障 1 小时 z=%.2f → 3 天后 z=%.2f", fFirst, fLast)

	if math.Abs(nfFirst) < 3 {
		t.Fatalf("前提不成立：故障刚开始就该被检出，z=%.2f", nfFirst)
	}
	if math.Abs(nfLast) >= 3 {
		t.Logf("注意：这份数据里不冻结也没被掩盖（z=%.2f），说明污染没有想象中严重", nfLast)
	}
	if math.Abs(fLast) < 3 {
		t.Errorf("冻结后 3 天仍应报警，got z=%.2f", fLast)
	}
	if math.Abs(fLast) < math.Abs(nfLast) {
		t.Errorf("冻结不该让 z 更低：冻结 %.2f vs 不冻结 %.2f", fLast, nfLast)
	}
}

// 排除必须有比例上限：业务真的扩容了，那是新常态，该学进去。
func TestAnomalyExclusionCapped(t *testing.T) {
	// 14 天历史，前 11 天正常、后 N 天抬高，看 σ 是否把抬高那段算进去
	end := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	mk := func(faultDays float64) (hist []deviation.Sample, since time.Time) {
		since = end.Add(-time.Duration(faultDays * 24 * float64(time.Hour)))
		r := rand.New(rand.NewSource(5))
		for ts := end.Add(-14 * 24 * time.Hour); ts.Before(end); ts = ts.Add(coarseStep) {
			v := 100 + r.NormFloat64()
			if !ts.Before(since) {
				v += 50
			}
			hist = append(hist, deviation.Sample{TS: ts, Value: v})
		}
		return hist, since
	}
	// 用 7 天档：1 天档的 Δ 在连续故障日之间是 0，看不出污染；7 天档才把故障与正常配成对。
	// 3/14 天 ≈ 21% < 25%：排除生效，σ 保持故障前的小尺度
	h3, s3 := mk(3)
	small := deviation.ComputeSigmaLagExcluding(604800, h3, s3.Add(-time.Hour))
	// 6/14 天 ≈ 43% > 25%：不再排除，σ 把抬高学进去，变大
	h6, s6 := mk(6)
	big := deviation.ComputeSigmaLagExcluding(604800, h6, s6.Add(-time.Hour))
	// 直接比该小时桶：排除生效时 σ 应停在噪声量级（个位数），
	// 超上限时把 +50 的抬高学进基线，σ 被张到抬高量级。
	h := end.Add(-coarseStep).UTC().Hour()
	sm, bg := small[h].Sigma, big[h].Sigma
	t.Logf("hour=%d：排除 21%%（生效）σ=%.2f；排除 43%%（超上限、不生效）σ=%.2f", h, sm, bg)
	if sm > 10 {
		t.Errorf("排除生效时 σ 应停在噪声量级，got %.2f", sm)
	}
	if bg < 15 {
		t.Errorf("超过 %.0f%% 时应把新水平学进基线（σ 张到抬高量级），got %.2f",
			deviation.MaxExcludedFrac*100, bg)
	}
	if b := NewHeatmapBuilder(NewSeries()); true {
		b.MarkAnomaly([]string{"a", "b"}, end)
		if b.AnomalyCount() != 2 {
			t.Fatalf("标记数 = %d", b.AnomalyCount())
		}
		b.ClearAnomaly(map[string]bool{"a": true})
		if b.AnomalyCount() != 1 {
			t.Fatalf("恢复正常的指标应解除标记，count=%d", b.AnomalyCount())
		}
	}
}
