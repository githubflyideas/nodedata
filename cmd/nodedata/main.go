package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/githubflyideas/nodedata"
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
  nodedata serve [flags]    采集 + L0 巡检 + 偏离度 + Web UI（唯一的运行方式）
  nodedata version

L0 绝对判定在 serve 里每个采集周期跑一次，结果在首页和 /api/check。
运行 nodedata serve -h 查看全部参数。
`, version)
}

func runServe() {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.String("port", "8888", "HTTP server port")
	interval := fs.String("interval", "5s", "Collect / check interval")
	procRoot := fs.String("proc", "/proc", "procfs root")
	sysRoot := fs.String("sys", "/sys", "sysfs root")
	dumpEvery := fs.String("dump-interval", "30s", "data/*.json dump interval")
	webRootFlag := fs.String("web-root", "", "从该目录读 index.html 覆盖内置页面（仅前端开发用）")
	dataDirFlag := fs.String("data-dir", "data", "数据目录：data/*.json 转储、baseline.json、history/")
	historyDirFlag := fs.String("history-dir", "", "长期层落盘目录，5 分钟一点、保留 56 天（默认 <data-dir>/history）")
	fs.Parse(os.Args[2:])

	webDir := ""
	if *webRootFlag != "" {
		webDir, _ = filepath.Abs(*webRootFlag)
	}
	dataDir, _ := filepath.Abs(*dataDirFlag)

	iv, err := time.ParseDuration(*interval)
	if err != nil || iv <= 0 {
		iv = 5 * time.Second
	}
	dv, err := time.ParseDuration(*dumpEvery)
	if err != nil || dv <= 0 {
		dv = 30 * time.Second
	}

	// data 目录必须可写，否则转储会静默失败 → 页面 404。提前显式报错。
	dumpToDisk := true
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "warn: data dir %s not writable (%v)\n", dataDir, err)
		fmt.Fprintf(os.Stderr, "      falling back to in-memory only; /data/*.json still served live\n")
		dumpToDisk = false
	}

	// ── L0：后台 sanity check → data/check.json
	// -proc 之前只传给了 L1 采集器，L0 仍在读真 /proc，导致 -proc 对 L0 无效。
	check.SetRoots(*procRoot, *sysRoot)
	l0 := NewBackgroundCheckRunner(iv, dataDir)
	l0.Start()
	defer l0.Stop()

	// ── L1：/proc 采集 → 内存序列（原始层 24h + 长期层 56 天，长期层落盘）
	series := NewSeries()
	col := collector.New(collector.Config{ProcRoot: *procRoot, Interval: iv})
	histDir := *historyDirFlag
	if histDir == "" {
		histDir = filepath.Join(dataDir, "history")
	}
	hist, err := NewHistory(histDir, coarseRetention)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: history dir %s 不可用（%v），长期层只在内存里，重启即丢\n", histDir, err)
		hist = nil
	} else {
		defer hist.Close()
		t0 := time.Now()
		lines, pts, err := hist.Load(series, t0)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: 读取历史失败: %v\n", err)
		}
		fmt.Printf("  history       %s（%d 行 / %d 点，%v）\n", histDir, lines, pts, time.Since(t0).Round(time.Millisecond))
	}
	stop := make(chan struct{})
	defer close(stop)
	go collectLoop(col, series, hist, *procRoot, iv, stop)

	// ── L3：偏离度
	builder := NewHeatmapBuilder(series)
	builder.procsFn = func() ([]server.ProcTop, time.Time) {
		ps, ts := col.TopProcs()
		out := make([]server.ProcTop, len(ps))
		for i, p := range ps {
			out[i] = server.ProcTop(p)
		}
		return out, ts
	}
	go sigmaLoop(builder, stop)

	healthFn := func() server.HealthJSON { return builder.Health(col.Health()) }

	// ── L4：诊断链（读 L0 结果 + L3 偏离度）
	diagnoser := NewDiagnoser(dataDir, series, builder)

	// ── L2：静态转储 → data/{1h,6h,24h,7d,30d}.json + health.json
	dumper := server.NewDumper(webDir, builder.Build, healthFn)
	dumper.SetDataDir(dataDir)

	// 首轮：先采一次、算一次、落一次盘，避免页面开局吃到 404/空文件
	if samples, err := col.CollectGlobal(*procRoot, time.Now()); err == nil {
		persist(hist, series.Add(samples))
	} else {
		fmt.Fprintf(os.Stderr, "warn: initial collect: %v\n", err)
	}
	// 载入人工基线（如果有）：让"曾经发生过"的累计计数器不再永久告警。
	if b, err := check.LoadBaseline(dataDir); err != nil {
		fmt.Fprintf(os.Stderr, "warn: 基线读取失败，按本次启动起算: %v\n", err)
	} else if b != nil {
		fmt.Printf("  基线          %s", b.At.Local().Format("2006-01-02 15:04:05"))
		if b.Note != "" {
			fmt.Printf("（%s）", b.Note)
		}
		fmt.Println()
	}

	builder.RefreshSigma()
	if dumpToDisk {
		if err := dumper.DumpAll(); err != nil {
			fmt.Fprintf(os.Stderr, "warn: initial dump failed: %v\n", err)
			dumpToDisk = false
		}
		go dumper.RunLoop(dv, stop)
	}

	mux := server.NewMuxWithConfig(
		server.MuxConfig{WebRoot: webDir, Index: nodedata.IndexHTML, DataDir: dataDir, Version: version,
			BaselineGet: func() interface{} {
				if b := check.CurrentBaseline(); b != nil {
					return b
				}
				return nil
			},
			BaselineSet: func(note string) (interface{}, error) {
				return check.SaveBaseline(dataDir, note)
			},
			BaselineClear: func() error { return check.ClearBaseline(dataDir) },
		},
		server.QueryFns{
			Heatmap: builder.Build,
			Detail:  func(ts time.Time) (interface{}, error) { return builder.Build(ts.Add(-time.Hour), ts) },
			Raw: func(metricID string, from, to time.Time) (interface{}, error) {
				return series.Range(metricID, from, to), nil
			},
			Window: func(name string) (interface{}, error) {
				d, ok := windowDuration(name)
				if !ok {
					return nil, fmt.Errorf("unknown window %q", name)
				}
				now := time.Now()
				return builder.Build(now.Add(-d), now)
			},
			Health: func() (interface{}, error) { return healthFn(), nil },
			Diagnosis: func(th float64) (interface{}, error) {
				return diagnoser.Run(th), nil
			},
		})

	dumpState := "on"
	if !dumpToDisk {
		dumpState = "off (serving live)"
	}
	fmt.Printf("nodedata %s\n", version)
	fmt.Printf("  listen        http://localhost:%s/\n", *port)
	if webDir != "" {
		fmt.Printf("  web root      %s（覆盖内置页面）\n", webDir)
	}
	fmt.Printf("  data dir      %s\n", dataDir)
	fmt.Printf("  procfs        %s\n", *procRoot)
	fmt.Printf("  sysfs         %s\n", *sysRoot)
	fmt.Printf("  collect every %s\n", iv)
	fmt.Printf("  dump every    %s  [%s]\n", dv, dumpState)
	fmt.Printf("  metrics       %d series, %d points buffered\n",
		len(series.MetricIDs()), series.Count())

	if err := server.ListenAndServe("0.0.0.0:"+*port, mux); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// windowDuration 把窗口名映射为时长。
func windowDuration(name string) (time.Duration, bool) {
	switch name {
	case "1h":
		return time.Hour, true
	case "6h":
		return 6 * time.Hour, true
	case "24h":
		return 24 * time.Hour, true
	case "7d":
		return 7 * 24 * time.Hour, true
	case "30d":
		return 30 * 24 * time.Hour, true
	}
	return 0, false
}

// collectLoop 周期性采集 /proc 写入内存序列；被抽进长期层的点同时落盘。
func collectLoop(col *collector.Collector, s *Series, hist *History, procRoot string, iv time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			var batch []collector.Sample
			if samples, err := col.CollectGlobal(procRoot, now); err == nil {
				batch = append(batch, samples...)
			}
			if samples, err := col.CollectProcs(procRoot, now); err == nil {
				batch = append(batch, samples...)
			}
			persist(hist, s.Add(batch))
		}
	}
}

// persist 把本轮抽进长期层的点写盘；失败只记一次，不影响采集。
func persist(hist *History, kept []collector.Sample) {
	if hist == nil || len(kept) == 0 {
		return
	}
	if err := hist.Append(kept); err != nil && histWarned.CompareAndSwap(false, true) {
		fmt.Fprintf(os.Stderr, "warn: 历史落盘失败（后续不再提示）: %v\n", err)
	}
}

var histWarned atomic.Bool

// sigmaLoop 周期性重算 σ 表。
func sigmaLoop(b *HeatmapBuilder, stop <-chan struct{}) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			b.Prune(now)
			b.RefreshSigma()
		}
	}
}
