package acceptance

import (
	"runtime"
	"testing"
	"time"
)

func BenchmarkCollectGlobal(b *testing.B) {
	var stats runtime.MemStats
	
	// warmup
	for i := 0; i < 10; i++ {
		col.CollectGlobal("/proc", time.Now())
	}
	
	runtime.GC()
	runtime.ReadMemStats(&stats)
	before := stats.Mallocs
	
	for i := 0; i < b.N; i++ {
		col.CollectGlobal("/proc", time.Now())
	}
	
	runtime.ReadMemStats(&stats)
	after := stats.Mallocs
	perRun := float64(after-before) / float64(b.N)
	b.Logf("allocs per run: %.1f", perRun)
}
