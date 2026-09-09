package deviation

import (
	"sync"
	"testing"
	"time"
)

// TestSigmaTableConcurrentSetGet 复现线上崩溃：
// sigmaLoop 每 5 分钟整表重写 σ，转储循环同时读取，
// 无锁时 go test -race 会报 data race，运行足够久则 fatal error:
// concurrent map read and map write。
func TestSigmaTableConcurrentSetGet(t *testing.T) {
	tbl := NewSigmaTable()
	ids := []string{"cpu.user", "mem.available", "disk.await", "net.rx_bytes"}
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 写：模拟 RefreshSigma
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			for _, id := range ids {
				for lag := 0; lag < NLag; lag++ {
					for hour := 0; hour < 24; hour++ {
						tbl.Set(id, lag, hour, LagSigma{Sigma: float64(i + 1), N: i, Ready: true})
					}
				}
			}
		}
	}()

	// 读：模拟 Deviation.Z
	dev := New(tbl)
	dev.LookupFn = func(string, time.Time, time.Duration) (float64, bool) { return 1, true }
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, id := range ids {
					z := dev.Z(id, 2, time.Now())
					for _, v := range z {
						if v > 6 || v < -6 {
							t.Errorf("z 越界: %v", v)
							return
						}
					}
					if ls := tbl.Get(id, 0, 0); ls.Sigma < 0 {
						t.Errorf("σ 为负: %v", ls.Sigma)
						return
					}
				}
			}
		}()
	}

	time.Sleep(400 * time.Millisecond)
	close(stop)
	wg.Wait()
	if tbl.Len() != len(ids) {
		t.Fatalf("指标数 = %d，期望 %d", tbl.Len(), len(ids))
	}
}

// TestSigmaTableOutOfRangeIndex 越界索引不该 panic。
func TestSigmaTableOutOfRangeIndex(t *testing.T) {
	tbl := NewSigmaTable()
	tbl.Set("x", -1, 0, LagSigma{Sigma: 9})
	tbl.Set("x", NLag, 0, LagSigma{Sigma: 9})
	tbl.Set("x", 0, 24, LagSigma{Sigma: 9})
	tbl.Set("x", 0, -3, LagSigma{Sigma: 9})
	for _, c := range [][2]int{{-1, 0}, {NLag, 0}, {0, 24}, {0, -1}} {
		if got := tbl.Get("x", c[0], c[1]).Sigma; got != 1e-6 {
			t.Fatalf("Get(%v) = %v，期望回落到 1e-6", c, got)
		}
	}
	tbl.Set("x", 3, 5, LagSigma{Sigma: 2, Ready: true})
	if got := tbl.Get("x", 3, 5); got.Sigma != 2 || !got.Ready {
		t.Fatalf("正常写读失败: %+v", got)
	}
}
