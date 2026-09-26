// server.go — 巡视台的进程外壳：参数、清单热加载循环、HTTP。
// nodedata-fleet 和 nodedata-pelican 共用这一份，只是页面和默认端口不同——
// 拉取和判断逻辑必须是同一份，两个界面才有可比性。
package fleet

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

// Options 区分同一套巡视台核心的不同外壳（不同页面、不同默认端口）。
type Options struct {
	Name        string // 程序名，用于日志和 User-Agent
	Version     string
	DefaultPort string
	Page        []byte // 内置页面
	PageFile    string // -web-root 下覆盖用的文件名
	Args        []string
}

// Main 跑巡视台，返回进程退出码。
func Main(o Options) int {
	version = o.Version
	pageHTML := o.Page
	fs := flag.NewFlagSet(o.Name, flag.ExitOnError)
	hostsPath := fs.String("hosts", "host.list", "主机清单文件；改了会自动重读，不用重启")
	listen := fs.String("listen", "127.0.0.1", "监听地址；默认仅本机。给 iPad 看要设成 0.0.0.0，并用防火墙限制来源")
	port := fs.String("port", o.DefaultPort, "HTTP 端口；跟本机上别的服务（比如 nodedata 默认的 8888）撞了就换一个")
	interval := fs.Duration("interval", 15*time.Second, "多久拉一轮")
	timeout := fs.Duration("timeout", 5*time.Second, "单台拉取超时")
	lostAfter := fs.Duration("lost-after", 0, "多久没拉到算失联；0 = 3 轮")
	parallel := fs.Int("parallel", 32, "同时拉几台")
	webRoot := fs.String("web-root", "", "从该目录读 "+o.PageFile+" 覆盖内置页面（仅前端开发用）")
	showVer := fs.Bool("version", false, "打印版本")
	fs.Parse(o.Args)
	if *showVer {
		fmt.Println(o.Name, version)
		return 0
	}
	if *interval < 5*time.Second {
		*interval = 5 * time.Second
	}
	if *timeout >= *interval {
		*timeout = *interval / 2
	}
	if *lostAfter <= 0 {
		*lostAfter = 3 * *interval
	}
	if *parallel < 1 {
		*parallel = 1
	}

	inv := newInvStore(*hostsPath)
	inv.Reload(time.Now())
	cur := inv.Get()
	if len(cur.Hosts) == 0 {
		fmt.Fprintf(os.Stderr, "%s: %s 里没有可用的主机：\n", o.Name, *hostsPath)
		for _, e := range cur.Errors {
			fmt.Fprintln(os.Stderr, "  "+e)
		}
		fmt.Fprintln(os.Stderr, "格式：名字 地址:端口 dc=… rack=… product=… group=… tags=a,b")
		return 1
	}
	for _, e := range cur.Errors {
		fmt.Fprintln(os.Stderr, "warn: "+e)
	}

	poller := NewPoller(*timeout, *parallel, o.Name+"/"+version)
	poller.lostAfter = *lostAfter
	poller.SetHosts(cur.Hosts)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go loop(ctx, poller, inv, *interval)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		body := pageHTML
		if *webRoot != "" {
			if b, err := os.ReadFile(filepath.Join(*webRoot, o.PageFile)); err == nil {
				body = b
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(body)
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		st := poller.Snapshot(time.Now(), *interval, *lostAfter, inv.Get())
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, r, st)
	})
	mux.HandleFunc("/health.txt", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		st := poller.Snapshot(now, *interval, *lostAfter, inv.Get())
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, healthLine(st, now))
	})

	addr := net.JoinHostPort(*listen, *port)
	if ip := net.ParseIP(*listen); ip == nil || !ip.IsLoopback() {
		fmt.Fprintf(os.Stderr, "warn: 监听 %s —— 页面会列出主机、进程名和 PID，请用防火墙限制来源\n", addr)
	}
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	fmt.Fprintf(os.Stderr, "%s %s：%d 台，每 %s 拉一轮，http://%s/\n", o.Name, version, len(cur.Hosts), *interval, addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, o.Name+":", err)
		if errors.Is(err, syscall.EADDRINUSE) {
			fmt.Fprintf(os.Stderr, "端口 %s 已被占用：本机是不是也在跑 nodedata（默认 8888）或另一个巡视台？用 -port 换一个\n", *port)
		}
		return 1
	}
	return 0
}

// loop：每轮先看清单变没变，再拉一轮。一轮没拉完不开下一轮（超时 < 间隔，正常不会发生）。
func loop(ctx context.Context, p *Poller, inv *invStore, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		now := time.Now()
		if inv.Reload(now) {
			c := inv.Get()
			p.SetHosts(c.Hosts)
			fmt.Fprintf(os.Stderr, "清单已重读：%d 台\n", len(c.Hosts))
		}
		p.Round(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// healthLine 给现有监控 grep 的一行。巡视台自己坏了也得有人知道。
func healthLine(st stateOut, now time.Time) string {
	ok, lost, bad, dev := st.Counts()
	status := "ok"
	var why []string
	// 刚启动、第一轮还没拉完不算停了（清单里有连不上的机器时，第一轮要等到超时）
	lastProgress := st.RoundAt
	if lastProgress == 0 {
		lastProgress = st.Started
	}
	if now.Unix()-lastProgress > int64(3*st.Interval) {
		status = "crit"
		why = append(why, "拉取停了")
	} else if st.RoundAt == 0 {
		status = "starting"
	}
	if len(st.Inventory.Errors) > 0 {
		if status == "ok" {
			status = "warn"
		}
		why = append(why, "清单有错")
	}
	line := fmt.Sprintf("%s hosts=%d ok=%d dev=%d bad=%d lost=%d", status, len(st.Hosts), ok, dev, bad, lost)
	if st.RoundAt > 0 {
		line += fmt.Sprintf(" last_round=%ds", now.Unix()-st.RoundAt)
	}
	if len(why) > 0 {
		line += " why=" + strings.Join(why, ",")
	}
	return line
}

func writeJSON(w http.ResponseWriter, r *http.Request, v any) {
	w.Header().Set("Content-Type", "application/json")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		json.NewEncoder(gz).Encode(v)
		return
	}
	json.NewEncoder(w).Encode(v)
}
