package main

// 这个测试是"跑满 5 分钟看会不会崩"的确定性替身。
//
// 线上崩溃的成因是：sigmaLoop 每 5 分钟整表重写 σ 表，转储循环每 30 秒读它，
// 两者没有同步，于是第一次 RefreshSigma 一定撞上 Build 的读取，
// 触发 fatal error: concurrent map read and map write（不可恢复，进程直接死）。
//
// 靠等 5 分钟来复现既慢又不可靠。这里把同一组对象按同样的方式并发驱动，
// 在 go test -race 下运行：只要 σ 表的读写没有同步，race detector 立刻报错。
import (
	"sync"
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
)

func seedSeries(t *testing.T) *Series {
	t.Helper()
	s := NewSeries()
	now := time.Now()
	ids := []string{"cpu.busy", "mem.avail", "net.rx", "disk.await"}
	var batch []collector.Sample
	// 4 个指标 × 600 点 × 30 秒间隔 = 5 小时历史，足够 ComputeSigma 出结果。
	for i := 0; i < 600; i++ {
		ts := now.Add(-time.Duration(600-i) * 30 * time.Second)
		for j, id := range ids {
			batch = append(batch, collector.Sample{
				MetricID: id, TS: ts, Value: float64((i*7+j*13)%100) + 0.5,
			})
		}
	}
	s.Add(batch)
	if got := len(s.MetricIDs()); got != len(ids) {
		t.Fatalf("seed 失败：指标数 = %d，期望 %d", got, len(ids))
	}
	return s
}

// TestSigmaRefreshRacesWithBuild 模拟 sigmaLoop 与转储循环/HTTP 请求的并发。
func TestSigmaRefreshRacesWithBuild(t *testing.T) {
	s := seedSeries(t)
	b := NewHeatmapBuilder(s)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 写方：等价于 sigmaLoop，只是不等那 5 分钟。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				b.RefreshSigma()
			}
		}
	}()

	// 读方：等价于 Dumper.DumpAll 与 /api/heatmap 请求。
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			now := time.Now()
			for {
				select {
				case <-stop:
					return
				default:
					hm, err := b.Build(now.Add(-2*time.Hour), now)
					if err != nil {
						t.Errorf("Build 出错：%v", err)
						return
					}
					if hm == nil {
						t.Errorf("Build 返回 nil")
						return
					}
					_ = b.Health(map[string]float64{"cpu.busy": 1})
				}
			}
		}()
	}

	time.Sleep(600 * time.Millisecond)
	close(stop)
	wg.Wait()
}
