package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/githubflyideas/nodedata"
	"github.com/githubflyideas/nodedata/cmd/nodedata/check"
	"github.com/githubflyideas/nodedata/internal/collector"
	"github.com/githubflyideas/nodedata/internal/diagnosis"
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
	// 默认只监听回环：页面会列出进程名、PID、主机负载水位，是内网侦察的现成材料。
	// 要给别人看就显式 --listen 0.0.0.0，并自己在防火墙/反代上加访问控制。
	listen := fs.String("listen", "127.0.0.1", "监听地址；默认仅本机。设为 0.0.0.0 前请确认有防火墙或反代鉴权")
	interval := fs.String("interval", "10s", "Collect / check interval")
	procRoot := fs.String("proc", "/proc", "procfs root")
	sysRoot := fs.String("sys", "/sys", "sysfs root")
	rootFS := fs.String("rootfs", "/", "要监控容量的挂载点")
	// 默认不转储：实测每轮写 2~7MB，30 秒一轮就是每天 4~21GB 的写入量
	// （刚启动 4.2GB/天，缓冲区攒满后约 21GB/天）。而页面读 /data/*.json 时
	// 磁盘缺文件会自动用内存实时构造兜底，转储对页面并非必需。
	// 需要把结果落盘带走（离线分析、留存现场）时再显式打开。
	dumpEvery := fs.String("dump-interval", "0", "data/*.json 转储周期；0 = 不转储（页面照常从内存实时构造）")
	webRootFlag := fs.String("web-root", "", "从该目录读 index.html 覆盖内置页面（仅前端开发用）")
	dataDirFlag := fs.String("data-dir", "data", "数据目录：data/*.json 转储、baseline.json、history/")
	historyDirFlag := fs.String("history-dir", "", "长期层落盘目录，5 分钟一点、保留 14 天（默认 <data-dir>/history）")
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
	if err != nil {
		dv = 0
	}

	// data 目录必须可写，否则转储会静默失败 → 页面 404。提前显式报错。
	// dv <= 0 表示不转储（默认），页面从内存实时构造。
	dumpToDisk := dv > 0
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "warn: data dir %s not writable (%v)\n", dataDir, err)
		fmt.Fprintf(os.Stderr, "      falling back to in-memory only; /data/*.json still served live\n")
		dumpToDisk = false
	}

	// ── L0：后台 sanity check → data/check.json
	// -proc 之前只传给了 L1 采集器，L0 仍在读真 /proc，导致 -proc 对 L0 无效。
	check.SetRoots(*procRoot, *sysRoot)
	l0 := NewBackgroundCheckRunner(iv, dataDir, dumpToDisk)
	l0.Start()
	defer l0.Stop()

	// ── L1：/proc 采集 → 内存序列（原始层 24h + 长期层 14 天，长期层落盘）
	series := NewSeries()
	col := collector.New(collector.Config{ProcRoot: *procRoot, SysRoot: *sysRoot, RootFS: *rootFS, Interval: iv})
	histDir := *historyDirFlag
	if histDir == "" {
		histDir = filepath.Join(dataDir, "history")
	}
	var cleanups []func()
	var shutdownOnce sync.Once
	hist, err := NewHistory(histDir, coarseRetention)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: history dir %s 不可用（%v），长期层只在内存里，重启即丢\n", histDir, err)
		hist = nil
	} else {
		cleanups = append(cleanups, func() { flushHistory(hist); hist.Close() })
		t0 := time.Now()
		lines, pts, err := hist.Load(series, t0)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: 读取历史失败: %v\n", err)
		}
		fmt.Printf("  history       %s（%d 行 / %d 点，%v）\n", histDir, lines, pts, time.Since(t0).Round(time.Millisecond))
	}
	stop := make(chan struct{})
	// 清理动作集中登记，由 shutdown() 执行。
	// 不能只靠 defer：systemctl stop 发的是 SIGTERM，Go 的默认行为是直接退出，
	// defer 一个都不会跑。实测那样会丢两样东西：
	//   1. services.jsonl 里的 down 事件——没有它，重启后回看这段停机时间，
	//      状态机以为"我们一直在看"，把服务显示成"确实不在"而不是"不知道"。
	//      等于告诉运维"你的服务那天挂了"，而其实只是我们自己被停了。
	//   2. 攒着还没落盘的长期层点（最多一个 5 分钟周期）。
	shutdown := func() {
		shutdownOnce.Do(func() {
			close(stop)
			for i := len(cleanups) - 1; i >= 0; i-- { // 与 defer 同序：后登记的先执行
				cleanups[i]()
			}
		})
	}
	defer shutdown()
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

	builder.groupsFn = func() []server.ProcGroup {
		gs := col.ProcGroups()
		out := make([]server.ProcGroup, len(gs))
		for i, g := range gs {
			out[i] = server.ProcGroup(g)
		}
		return out
	}

	healthFn := func() server.HealthJSON { return builder.Health(col.Health()) }

	// ── L4：诊断链（读 L0 结果 + L3 偏离度）
	// 服务识别与事件流（14 天保留，跟长期层一致）。放在 diagnoser 之前：
	// L4 的责任方要带服务名。
	// 消失的服务保留 7 天：14 天前挂掉的服务基本没人还在查，而它们一直占着表
	svcLog := NewServiceLog(filepath.Join(dataDir, "services.jsonl"), 7*24*time.Hour)
	if n, err := svcLog.Load(time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "warn: 服务事件流读取失败: %v\n", err)
	} else {
		fmt.Printf("  services      %d 条历史事件\n", n)
	}
	cleanups = append(cleanups, func() { svcLog.Close(time.Now()) })
	go serviceLoop(col, svcLog, stop)

	// 事故时间线的 BEFORE 段：从内存时序切出异常前 5 分钟。
	// 事后再抓是抓不到已经过去的时刻的——这是我们相对"出事才开始采"的工具的优势。
	beforeSeries := func(class string, from, to time.Time) []beforePoint {
		ids := classBeforeMetrics(class)
		out := []beforePoint{}
		for ts := from; !ts.After(to); ts = ts.Add(30 * time.Second) {
			v := map[string]float64{}
			for _, id := range ids {
				if x, ok := series.Lookup(id, ts, 20*time.Second); ok {
					v[id] = x
				}
			}
			if len(v) > 0 {
				out = append(out, beforePoint{TS: ts.Unix(), V: v})
			}
		}
		return out
	}

	diagnoser := NewDiagnoser(dataDir, series, builder)
	diagnoser.procs = procsFromCollector(col, svcLog)
	diagnoser.l0Fn = l0.GetLatest
	go sigmaLoop(builder, diagnoser, stop)

	// ── 事故留证：后台每 15 秒跑一次 L4，出现带归因的结论就把现场存下来（不依赖页面打开）
	recorder, err := NewRecorder(filepath.Join(dataDir, "incidents"), *procRoot,
		func() *diagnosis.Chain { return diagnoser.Run(3.0) }, diagnoser.procs, diagnoser.latestDeviations)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: 事故留证目录不可用: %v\n", err)
		recorder = nil
	} else {
		recorder.sysRoot = *sysRoot
		recorder.before = beforeSeries
	}

	// ── L2：静态转储 → data/{1h,6h,24h,7d,14d}.json + health.json
	dumper := server.NewDumper(webDir, builder.Build, healthFn)
	dumper.SetDataDir(dataDir)

	// 首轮：先采一次、算一次、落一次盘，避免页面开局吃到 404/空文件
	if samples, err := col.CollectGlobal(*procRoot, time.Now()); err == nil {
		persist(hist, series.Add(samples), time.Now())
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
			Check:   func() (interface{}, error) { return l0.GetLatest(), nil },
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

	mux.HandleFunc("/api/services", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, servicesJSON(svcLog, time.Now()))
	})

	hostname, _ := os.Hostname()
	mux.HandleFunc("/health.txt", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		l0 := diagnoser.loadL0()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprint(w, healthLine(hostname, l0, diagnoser.Run(3.0), builder.healthValues(now), now))
	})

	// 一条 curl 回答"L1–L5 为什么没数"：卡在 σ 还是卡在配对
	mux.HandleFunc("/api/lagdiag", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, builder.DiagnoseLags(time.Now()))
	})

	mux.HandleFunc("/api/keyseries", func(w http.ResponseWriter, r *http.Request) {
		win := r.URL.Query().Get("win")
		if win == "" {
			win = "6h"
		}
		d, ok := windowDuration(win)
		if !ok {
			http.Error(w, "bad window", http.StatusBadRequest)
			return
		}
		writeJSON(w, builder.KeySeries(win, d, time.Now()))
	})

	mux.HandleFunc("/api/compare", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, builder.Compare(time.Now()))
	})

	if recorder != nil {
		go recorder.Loop(15*time.Second, stop)
		mux.HandleFunc("/api/incidents", func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				writeJSON(w, recorder.List())
			case http.MethodPost: // 手动留证：以此刻 CPU 前 3 的进程为对象
				it := diagnosis.Item{Title: "手动留证", Timestamp: time.Now()}
				for i, p := range diagnoser.procs() {
					if i >= 3 {
						break
					}
					it.Culprits = append(it.Culprits, diagnosis.Culprit{Kind: "process", PID: p.PID, Name: p.Comm,
						Detail: fmt.Sprintf("此刻 CPU %.0f%%", p.CPU)})
				}
				id, err := recorder.Capture(it, diagnoser.Run(3.0), time.Now())
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				writeJSON(w, map[string]string{"id": id})
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		})
		mux.HandleFunc("/api/incidents/", func(w http.ResponseWriter, r *http.Request) {
			b, err := recorder.Get(strings.TrimPrefix(r.URL.Path, "/api/incidents/"))
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
		})
	}

	dumpState := "on"
	if !dumpToDisk {
		dumpState = "关（页面从内存实时构造；--dump-interval 30s 可开启）"
	}
	fmt.Printf("nodedata %s\n", version)
	fmt.Printf("  listen        http://%s:%s/\n", *listen, *port)
	if *listen == "0.0.0.0" || *listen == "::" {
		fmt.Fprintf(os.Stderr, "warn: 监听在 %s，页面无鉴权且会显示进程名与 PID —— 请用防火墙限制来源，或放在带鉴权的反代之后\n", *listen)
	}
	if webDir != "" {
		fmt.Printf("  web root      %s（覆盖内置页面）\n", webDir)
	}
	fmt.Printf("  data dir      %s\n", dataDir)
	fmt.Printf("  procfs        %s\n", *procRoot)
	fmt.Printf("  sysfs         %s\n", *sysRoot)
	fmt.Printf("  collect every %s\n", iv)
	if dumpToDisk {
		fmt.Printf("  dump every    %s  [%s]\n", dv, dumpState)
	} else {
		fmt.Printf("  dump          %s\n", dumpState)
	}
	fmt.Printf("  metrics       %d series, %d points buffered\n",
		len(series.MetricIDs()), series.Count())

	srv := &http.Server{Addr: *listen + ":" + *port, Handler: mux}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		shutdown()
		os.Exit(1)
	case sg := <-sig:
		fmt.Printf("\n收到 %v，正在收尾（写 down 事件、落盘未写的历史点）…\n", sg)
		shutdown()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		fmt.Println("已退出")
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
	case "14d":
		return 14 * 24 * time.Hour, true
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
			persist(hist, s.Add(batch), now)
		}
	}
}

// persist 把本轮抽进长期层的点写盘；失败只记一次，不影响采集。
// persist 把本轮抽进长期层的点攒起来，每个长期层周期只写一次。
//
// 不能来一批写一批：新出现的指标（进程名一直在变）会立刻开一个长期层点并单独落一行，
// 实测本该 5 分钟一行的文件，每个周期写了 4~6 行、每行几十到一百个指标。
// 合并之后写入次数降到 1/5，而且一行就是一个完整的时刻切面，读回时也更整齐。
func persist(hist *History, kept []collector.Sample, now time.Time) {
	if hist == nil {
		return
	}
	histMu.Lock()
	histPend = append(histPend, kept...)
	// 对齐到长期层周期边界，避免"攒够时长才写"导致最后一批一直不落盘
	if now.Unix()/int64(coarseStep/time.Second) == histSlot || len(histPend) == 0 {
		histMu.Unlock()
		return
	}
	histSlot = now.Unix() / int64(coarseStep/time.Second)
	batch := histPend
	histPend = nil
	histMu.Unlock()

	if err := hist.Append(batch); err != nil && histWarned.CompareAndSwap(false, true) {
		fmt.Fprintf(os.Stderr, "warn: 历史落盘失败（后续不再提示）: %v\n", err)
	}
}

// flushHistory 退出前把攒着的点写掉。
func flushHistory(hist *History) {
	if hist == nil {
		return
	}
	histMu.Lock()
	batch := histPend
	histPend = nil
	histMu.Unlock()
	if len(batch) > 0 {
		_ = hist.Append(batch)
	}
}

var (
	histWarned atomic.Bool
	histMu     sync.Mutex
	histPend   []collector.Sample
	histSlot   int64
)

// sigmaLoop 周期性重算 σ 表；重算前标记"正处在 L4 结论里"的指标，
// 它们的异常期样本不参与基线，否则持续几天的故障会被 σ 学成常态、自己"痊愈"
// （见 heatmap.go 的 MarkAnomaly）。
func sigmaLoop(b *HeatmapBuilder, d *Diagnoser, stop <-chan struct{}) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			b.Prune(now)
			// 先标异常：Run 用的是当前 σ，所以必须在 RefreshSigma 之前问它"现在谁在报警"
			if d != nil {
				if chain := d.Run(3.0); chain != nil {
					active := map[string]bool{}
					for _, it := range chain.Items {
						if it.Class == "" {
							continue
						}
						b.MarkAnomaly(it.Metrics, now)
						for _, m := range it.Metrics {
							active[m] = true
						}
					}
					b.ClearAnomaly(active) // 恢复正常的指标解除标记
				}
			}
			b.RefreshSigma()
		}
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// serviceLoop 周期性识别服务。60 秒一次：端口映射要扫 /proc/net/tcp（O(连接数)）
// 并对每个进程读 /proc/PID/fd，比指标采集贵得多；而服务的监听端口不会一分钟变一次。
func serviceLoop(col *collector.Collector, l *ServiceLog, stop <-chan struct{}) {
	tick := func() {
		now := time.Now()
		l.Update(col.CollectServices(now), now)
	}
	tick()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			tick()
		}
	}
}
