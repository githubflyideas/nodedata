package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/githubflyideas/nodedata/cmd/nodedata/check"
	"github.com/githubflyideas/nodedata/internal/collector"
	"github.com/githubflyideas/nodedata/internal/server"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "check":
		runCheck()
	case "serve":
		runServe()
	case "version":
		fmt.Printf("nodedata %s\n", version)
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `nodedata %s

Usage:
  nodedata check [--timeout 5s]              Run L0 39-point sanity check
  nodedata serve [--port 8888] [--interval 5s]  Start collector + Web UI
  nodedata version

Exit codes (check): 0 = pass, 1 = fail, 2 = warn only
`, version)
}

func runCheck() {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	timeout := fs.String("timeout", "5s", "Check timeout duration")
	fs.Parse(os.Args[2:])
	results, exitCode := check.Run(*timeout)
	check.PrintResults(results)
	os.Exit(exitCode)
}

func runServe() {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.String("port", "8888", "HTTP server port")
	interval := fs.String("interval", "5s", "Collect / check interval")
	procRoot := fs.String("proc", "/proc", "procfs root")
	dumpEvery := fs.String("dump-interval", "30s", "data/*.json dump interval")
	fs.Parse(os.Args[2:])

	webDir, _ := os.Getwd()
	dataDir := filepath.Join(webDir, "data")
	os.MkdirAll(dataDir, 0755)

	iv, err := time.ParseDuration(*interval)
	if err != nil || iv <= 0 {
		iv = 5 * time.Second
	}
	dv, err := time.ParseDuration(*dumpEvery)
	if err != nil || dv <= 0 {
		dv = 30 * time.Second
	}

	// ── L0：后台 sanity check → data/check.json
	l0 := NewBackgroundCheckRunner(iv, dataDir)
	l0.Start()
	defer l0.Stop()

	// ── L1：/proc 采集 → 内存序列
	series := NewSeries()
	col := collector.New(collector.Config{ProcRoot: *procRoot, Interval: iv})
	stop := make(chan struct{})
	defer close(stop)
	go collectLoop(col, series, *procRoot, iv, stop)

	// ── L3：偏离度
	builder := NewHeatmapBuilder(series)
	go sigmaLoop(builder, stop)

	// ── L2：静态转储 → data/{1h,6h,24h,7d,30d}.json + health.json
	dumper := server.NewDumper(webDir, builder.Build, func() server.HealthJSON {
		return builder.Health(col.Health())
	})
	go dumper.RunLoop(dv, stop)

	// 首轮：先采一次再立即转储，避免页面开局拿到空文件
	if samples, err := col.CollectGlobal(*procRoot, time.Now()); err == nil {
		series.Add(samples)
	}
	builder.RefreshSigma()
	if err := dumper.DumpAll(); err != nil {
		fmt.Fprintf(os.Stderr, "warn: initial dump: %v\n", err)
	}

	mux := server.NewMux(webDir, server.QueryFns{
		Heatmap: builder.Build,
		Detail:  func(ts time.Time) (interface{}, error) { return builder.Build(ts.Add(-time.Hour), ts) },
		Raw: func(metricID string, from, to time.Time) (interface{}, error) {
			return series.Range(metricID, from, to), nil
		},
	})

	fmt.Printf("nodedata %s\n", version)
	fmt.Printf("  listen        http://localhost:%s/\n", *port)
	fmt.Printf("  web root      %s\n", webDir)
	fmt.Printf("  data dir      %s\n", dataDir)
	fmt.Printf("  procfs        %s\n", *procRoot)
	fmt.Printf("  collect every %s\n", iv)
	fmt.Printf("  dump every    %s\n", dv)
	fmt.Printf("  metrics       %d series, %d points buffered\n",
		len(series.MetricIDs()), series.Count())

	if err := server.ListenAndServe("0.0.0.0:"+*port, mux); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// collectLoop 周期性采集 /proc 写入内存序列。
func collectLoop(col *collector.Collector, s *Series, procRoot string, iv time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			if samples, err := col.CollectGlobal(procRoot, now); err == nil {
				s.Add(samples)
			}
			if samples, err := col.CollectProcs(procRoot, now); err == nil {
				s.Add(samples)
			}
		}
	}
}

// sigmaLoop 周期性重算 σ 表。
func sigmaLoop(b *HeatmapBuilder, stop <-chan struct{}) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			b.RefreshSigma()
		}
	}
}
