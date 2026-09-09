package deviation

// 这些测试针对的是页面"整张表通红"的真实症状：
// 早期算法 z = Δ/σ 没有减去 Δ 的历史中位数，任何有稳定漂移的指标
// （累计计数器、page cache、slab）都恒定顶在 +6；而整数型指标因为
// MAD=0 落到硬地板 1/6，动一格就恰好是 6。下面每个用例都对应一类。
import (
	"math"
	"testing"
	"time"
)

const step = 5 * time.Second // 采集间隔

// build 用一个取值函数生成 dur 长度的升序历史。
func build(dur time.Duration, f func(i int, t time.Time) float64) []Sample {
	n := int(dur / step)
	base := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	out := make([]Sample, 0, n)
	for i := 0; i < n; i++ {
		t := base.Add(time.Duration(i) * step)
		out = append(out, Sample{TS: t, Value: f(i, t)})
	}
	return out
}

// zAt 按 hist 建 σ 表，再对最后一点求某档 z。
func zAt(t *testing.T, lagIdx int, hist []Sample, curVal float64) (float64, LagSigma) {
	t.Helper()
	last := hist[len(hist)-1]
	hour := last.TS.UTC().Hour()
	ls, err := ComputeSigma(LagSeconds[lagIdx], hour, hist)
	if err != nil {
		t.Fatalf("ComputeSigma: %v", err)
	}
	tbl := NewSigmaTable()
	tbl.Set("m", lagIdx, hour, ls)
	d := New(tbl)
	d.LookupFn = func(_ string, at time.Time, tol time.Duration) (float64, bool) {
		best, ok := -1, false
		for i := range hist {
			if absDur(hist[i].TS.Sub(at)) <= tol {
				if !ok || absDur(hist[i].TS.Sub(at)) < absDur(hist[best].TS.Sub(at)) {
					best, ok = i, true
				}
			}
		}
		if !ok {
			return 0, false
		}
		return hist[best].Value, true
	}
	return d.Z("m", curVal, last.TS)[lagIdx], ls
}

// 累计计数器：速率恒定的网卡字节数。正常状态下 z 必须接近 0。
// 早期算法在这里给 +6（Δ 是个大正数，σ 是它的 2%）。
func TestSteadyCounterIsNotAnomalous(t *testing.T) {
	const rate = 1_250_000 // 字节/秒
	hist := build(90*time.Minute, func(i int, _ time.Time) float64 {
		return float64(i) * rate * 5
	})
	last := hist[len(hist)-1].Value
	z, ls := zAt(t, 0, hist, last+rate*5) // L1=300s
	if math.Abs(z) > 1 {
		t.Fatalf("速率恒定的计数器不该告警：z=%.2f（Center=%.0f Sigma=%.0f N=%d）",
			z, ls.Center, ls.Sigma, ls.N)
	}
	if ls.Center <= 0 {
		t.Fatalf("Center 应等于一档时间内的正常增量，实得 %.0f", ls.Center)
	}
}

// 同一个计数器，速率突然涨到 10 倍：这才是要报的。
func TestCounterRateSpikeDoesAlert(t *testing.T) {
	const rate = 1_250_000
	hist := build(90*time.Minute, func(i int, _ time.Time) float64 {
		return float64(i) * rate * 5
	})
	last := hist[len(hist)-1].Value
	// 让 300 秒窗口内的增量变成正常值的 10 倍（而不是只让最后一个 5 秒变快——
	// 那对 300 秒的窗口来说本来就只是 3% 的扰动，不该报）。
	prev := hist[len(hist)-61].Value
	z, ls := zAt(t, 0, hist, prev+10*(last-prev))
	if z < 4 {
		t.Fatalf("速率涨到 10 倍必须报出来：z=%.2f（Center=%.0f Sigma=%.0f）", z, ls.Center, ls.Sigma)
	}
}

// 整数型指标：某进程 CPU 在 0/1 之间跳。动一格是常态，不是满量程异常。
// 早期算法：MAD=0 → σ=1/6 → Δ=1 → z=6.00，页面上一整片 ±6 就是这么来的。
func TestIntegerJitterIsNotFullScale(t *testing.T) {
	hist := build(90*time.Minute, func(i int, _ time.Time) float64 {
		return float64(i % 2) // 0,1,0,1…
	})
	z, ls := zAt(t, 0, hist, 1)
	if math.Abs(z) > 2 {
		t.Fatalf("整数指标跳一格不该是满量程：z=%.2f（Quantum=%.2f Sigma=%.2f ZeroFrac=%.2f）",
			z, ls.Quantum, ls.Sigma, ls.ZeroFrac)
	}
	if ls.Quantum != 1 {
		t.Fatalf("整数指标的分辨率应为 1，实得 %.4f", ls.Quantum)
	}
}

// 恒为 0 的指标：Δ 恒为 0，z 必须是 0，不能因为除以极小的 σ 而爆掉。
func TestFlatZeroMetricStaysZero(t *testing.T) {
	hist := build(90*time.Minute, func(int, time.Time) float64 { return 0 })
	z, _ := zAt(t, 0, hist, 0)
	if z != 0 {
		t.Fatalf("恒 0 指标的 z 应为 0，实得 %.4f", z)
	}
}

// 但恒为 0 的指标真的跳起来，还是要报。
func TestFlatZeroMetricAlertsOnRealJump(t *testing.T) {
	hist := build(90*time.Minute, func(int, time.Time) float64 { return 0 })
	z, ls := zAt(t, 0, hist, 50)
	if z < 4 {
		t.Fatalf("恒 0 指标跳到 50 必须报：z=%.2f（Sigma=%.4f）", z, ls.Sigma)
	}
}

// 样本不够就画斜纹，不要编一个数出来。
func TestNotReadyBelowMinDiffs(t *testing.T) {
	hist := build(6*time.Minute, func(i int, _ time.Time) float64 { return float64(i) })
	ls, _ := ComputeSigma(LagSeconds[0], hist[len(hist)-1].TS.UTC().Hour(), hist)
	if ls.N >= MinDiffsForZ {
		t.Skipf("这段历史的样本数 %d 已经够了，用例前提不成立", ls.N)
	}
	if ls.Ready {
		t.Fatalf("N=%d < %d 时不该 Ready", ls.N, MinDiffsForZ)
	}
	z, _ := zAt(t, 0, hist, 1e9)
	if !math.IsNaN(z) {
		t.Fatalf("未就绪时应返回 NaN，实得 %.2f", z)
	}
}

// 小样本要收缩：同样的偏离，N 小的时候 z 必须更小。
func TestSmallSampleShrinksZ(t *testing.T) {
	mk := func(n int) LagSigma {
		return LagSigma{Center: 0, Sigma: 1, N: n, Ready: true, Quantum: 1}
	}
	zOf := func(ls LagSigma) float64 {
		tbl := NewSigmaTable()
		tbl.Set("m", 0, 10, ls)
		d := New(tbl)
		d.LookupFn = func(string, time.Time, time.Duration) (float64, bool) { return 0, true }
		return d.Z("m", 3, time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC))[0]
	}
	small, large := zOf(mk(8)), zOf(mk(2000))
	if !(small < large*0.8) {
		t.Fatalf("小样本应被收缩：N=8 得 %.2f，N=2000 得 %.2f", small, large)
	}
}

// σ 的配对容差必须和 Z 的一致。早期版本 ComputeSigma 传的是 H 本身，
// 也就是"28 天内任何点都算 28 天前的值"，σ 完全失真。
func TestLagToleranceIsBounded(t *testing.T) {
	if got := LagTolerance(300 * time.Second); got != 30*time.Second {
		t.Fatalf("L1 容差应为 30s，实得 %v", got)
	}
	if got := LagTolerance(28 * 24 * time.Hour); got != 30*time.Minute {
		t.Fatalf("L14 容差应封顶 30min，实得 %v", got)
	}
	if got := LagTolerance(24 * time.Hour); got != 28*time.Minute+48*time.Second {
		t.Fatalf("L9 容差应为 H/50，实得 %v", got)
	}
}
