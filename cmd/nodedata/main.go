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
	procRoot := fs.String("proc", "/proc", "procfs root")
	sysRoot := fs.String("sys", "/sys", "sysfs root")
	dataDir := fs.String("data-dir", "", "读取基线的目录（有基线时累计计数器只判新增）")
	fs.Parse(os.Args[2:])
	check.SetRoots(*procRoot, *sysRoot)
	if *dataDir != "" {
		if _, err := check.LoadBaseline(*dataDir); err != nil {
			fmt.Fprintf(os.Stderr, "warn: 基线读取失败: %v\n", err)
		}
	}
	results, exitCode := check.Run(*timeout)
	check.PrintResults(results)
	os.Exit(exitCode)
}

func runServe() {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.String("port", "8888", "HTTP server port")
	interval := fs.String("interval", "5s", "Collect / check interval")
	procRoot := fs.String("proc", "/proc", "procfs root")
	sysRoot := fs.String("sys", "/sys", "sysfs root")
	dumpEvery := fs.String("dump-interval", "30s", "data/*.json dump interval")
	webRootFlag := fs.String("web-root", "", "directory containing index.html (default: cwd, else executable dir)")
	dataDirFlag := fs.String("data-dir", "", "directory for data/*.json (default: <web-root>/data)")
	fs.Parse(os.Args[2:])

	webDir := resolveWebRoot(*webRootFlag)
	dataDir := *dataDirFlag
	if dataDir == "" {
		dataDir = filepath.Join(webDir, "data")
	}

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

	// ── L1：/proc 采集 → 内存序列
	series := NewSeries()
	col := collector.New(collector.Config{ProcRoot: *procRoot, Interval: iv})
	stop := make(chan struct{})
	defer close(stop)
	go collectLoop(col, series, *procRoot, iv, stop)

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
		series.Add(samples)
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
		server.MuxConfig{WebRoot: webDir, DataDir: dataDir, Version: version,
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
	fmt.Printf("  web root      %s\n", webDir)
	fmt.Printf("  data dir      %s\n", dataDir)
	fmt.Printf("  procfs        %s\n", *procRoot)
	fmt.Printf("  sysfs         %s\n", *sysRoot)
	fmt.Printf("  collect every %s\n", iv)
	fmt.Printf("  dump every    %s  [%s]\n", dv, dumpState)
	fmt.Printf("  metrics       %d series, %d points buffered\n",
		len(series.MetricIDs()), series.Count())
	if _, err := os.Stat(filepath.Join(webDir, "index.html")); err != nil {
		fmt.Fprintf(os.Stderr, "warn: no index.html in %s — pass --web-root\n", webDir)
	}

	if err := server.ListenAndServe("0.0.0.0:"+*port, mux); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// resolveWebRoot 选择 web 根目录：显式参数 > 含 index.html 的 cwd > 可执行文件所在目录。
// 不再无条件依赖 cwd，避免以 systemd/其他目录启动时转储与静态文件分家。
func resolveWebRoot(flagVal string) string {
	if flagVal != "" {
		abs, err := filepath.Abs(flagVal)
		if err == nil {
			return abs
		}
		return flagVal
	}
	if cwd, err := os.Getwd(); err == nil {
		if _, err := os.Stat(filepath.Join(cwd, "index.html")); err == nil {
			return cwd
		}
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		if _, err := os.Stat(filepath.Join(dir, "index.html")); err == nil {
			return dir
		}
	}
	cwd, _ := os.Getwd()
	return cwd
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
		case now := <-t.C:
			b.Prune(now)
			b.RefreshSigma()
		}
	}
}
