package main

import (
	"sort"
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
	"github.com/githubflyideas/nodedata/internal/metrics"
)

// 采集器在这台机器上实际吐出的每一个指标，都必须在注册表里登记过。
// 黄金快照只认识写快照那天的指标；将来有人加了新指标却忘了登记，
// 它会悄悄走兜底（只猜单位、不归类、不进族）——这条测试让它当场失败。
func TestEveryEmittedMetricIsRegistered(t *testing.T) {
	c := collector.New(collector.Config{ProcRoot: "/proc", SysRoot: "/sys", RootFS: "/", Interval: time.Second})
	seen := map[string]bool{}
	now := time.Now()
	for i := 0; i < 3; i++ { // 速率类要第二轮才有值
		now = now.Add(time.Second)
		if s, err := c.CollectGlobal("/proc", now); err == nil {
			for _, x := range s {
				seen[x.MetricID] = true
			}
		}
		if s, err := c.CollectProcs("/proc", now); err == nil {
			for _, x := range s {
				seen[x.MetricID] = true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(seen) < 30 {
		t.Skipf("只采到 %d 个指标，这台机器的 /proc 不完整", len(seen))
	}
	var missing []string
	for id := range seen {
		if !metrics.Lookup(id).Registered {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("以下指标采集器在吐、注册表里没有：%v\n（在 internal/metrics/registry.go 的 known 里加一行）", missing)
	}
	t.Logf("采到 %d 个指标，全部已登记", len(seen))
}
