package main

import (
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/deviation"
)

// 三张表一套时间点：变化幅度的 9 档必须正好是 timePoints 的前 9 个。
// 原来三张表三套点，中间各不相同——这根桩防止它们再次各自漂走。
func TestLagsMatchPageTimePoints(t *testing.T) {
	if len(timePoints) != deviation.NLag+1 {
		t.Fatalf("时间点应比档位多一个（14天只给不需要统计的表），实得 %d 个点、%d 档",
			len(timePoints), deviation.NLag)
	}
	for i, sec := range deviation.LagSeconds {
		if got := timePoints[i].Ago; got != time.Duration(sec)*time.Second {
			t.Errorf("第 %d 档 %ds 与页面时间点 %s(%v) 不一致", i+1, sec, timePoints[i].Name, got)
		}
	}
	// 最长档必须撑得住：一档需要 2 倍历史
	if longest := time.Duration(deviation.LagSeconds[deviation.NLag-1]) * time.Second; 2*longest > coarseRetention {
		t.Errorf("最长档 %v 需要 %v 历史，超过保留期 %v", longest, 2*longest, coarseRetention)
	}
	// 对比表、服务表就是同一份
	if len(compareCols) != len(timePoints) || len(svcCols) != len(timePoints) {
		t.Errorf("对比表 %d 列、服务表 %d 列，应都是 %d", len(compareCols), len(svcCols), len(timePoints))
	}
}

// 短列不能被"长期层格子"的下限撑大到超过自身跨度的一半：
// "5分钟前"拿 7 分半前的点来顶是错的。
func TestLookupTolShortColumns(t *testing.T) {
	for _, c := range []struct {
		ago, max time.Duration
	}{
		{5 * time.Minute, 150 * time.Second},
		{10 * time.Minute, 150 * time.Second},
		{time.Hour, 150 * time.Second},
	} {
		if got := lookupTol(c.ago); got > c.max || got > c.ago/2 {
			t.Errorf("lookupTol(%v) = %v，不该超过 %v 也不该超过跨度一半", c.ago, got, c.max)
		}
	}
}
