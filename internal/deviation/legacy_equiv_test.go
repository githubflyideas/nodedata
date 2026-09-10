package deviation

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

// legacyComputeSigma 是 v3.0.8 的逐桶实现，原样保留作对照。
func legacyComputeSigma(lagSeconds int, hour int, hist []Sample) (LagSigma, error) {
	if len(hist) == 0 {
		return LagSigma{Sigma: 1e-9}, nil
	}
	H := time.Duration(lagSeconds) * time.Second
	tol := LagTolerance(H)
	spanOK := hist[len(hist)-1].TS.Sub(hist[0].TS) >= H

	// 收集 28 天内 hour 桶的 Δv（有符号）。
	var diffs []float64
	end := hist[len(hist)-1].TS
	cutoff := end.Add(-28 * 24 * time.Hour)
	histStart := hist[0].TS

	for i := len(hist) - 1; i >= 0; i-- {
		s := hist[i]
		if s.TS.Before(cutoff) {
			break
		}
		if s.TS.UTC().Hour() != hour {
			continue
		}
		target := s.TS.Add(-H)
		if target.Before(histStart) {
			continue
		}
		// 容差必须与 Z 里配 v(t-H) 用的一致，否则 σ 是"松配对"的尺度、
		// Δ 是"紧配对"的量，两者根本不是同一个分布。早期版本这里传的是 H
		// 本身（等于 28 天内任何点都算"28 天前的值"），σ 完全失真。
		prev, ok := findNearest(hist, target, tol)
		if !ok {
			continue
		}
		diffs = append(diffs, s.Value-prev)
	}

	ls := LagSigma{N: len(diffs)}
	ls.Ready = spanOK && len(diffs) >= MinDiffsForZ
	ls.Low = ls.N < HighConfN
	if len(diffs) == 0 {
		ls.Sigma = 1e-9
		return ls, nil
	}

	ls.Center = median(diffs)
	ls.Quantum = resolutionOf(diffs)
	ls.ZeroFrac = zeroFraction(diffs)
	ls.Sigma = robustScale(diffs, ls.Center, ls.Quantum)
	return ls, nil
}

// TestComputeSigmaLagMatchesLegacy：单遍实现必须与逐桶实现逐位一致。
func TestComputeSigmaLagMatchesLegacy(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var hist []Sample
	ts := start
	for k := 0; k < 3*24*60; k++ { // 3 天，1 分钟一点，带随机空洞与抖动
		ts = ts.Add(time.Minute + time.Duration(r.Intn(20)-10)*time.Second)
		if r.Intn(50) == 0 {
			ts = ts.Add(time.Duration(r.Intn(40)) * time.Minute) // 采集空洞
		}
		v := 100 + r.NormFloat64()*5
		if k%3 == 0 {
			v = math.Round(v) // 混入整数型
		}
		hist = append(hist, Sample{TS: ts, Value: v})
	}
	for _, lag := range LagSeconds {
		all := ComputeSigmaLag(lag, hist)
		for h := 0; h < 24; h++ {
			want, _ := legacyComputeSigma(lag, h, hist)
			if all[h] != want {
				t.Fatalf("lag=%d hour=%d: got %+v want %+v", lag, h, all[h], want)
			}
		}
	}
}
