// nodedata-fleet — 巡视台：把几十上百台 nodedata 的 USE 五行汇到一块屏上轮播。
//
//	nodedata-fleet -hosts host.list -listen 0.0.0.0
//
// 一个二进制加一份 host.list。没有数据库、不存历史：历史在各台自己那里，
// 大屏只回答"现在哪台不对、从什么时候开始"。
package main

import (
	"compress/gzip"
	"context"
	_ "embed"
	"encoding/json"
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

//go:embed fleet.html
var pageHTML []byte

func main() {
	fs := flag.NewFlagSet("nodedata-fleet", flag.ExitOnError)
	hostsPath := fs.String("hosts", "host.list", "主机清单文件；改了会自动重读，不用重启")
	listen := fs.String("listen", "127.0.0.1", "监听地址；默认仅本机。给 iPad 看要设成 0.0.0.0，并用防火墙限制来源")
	port := fs.String("port", "19990", "HTTP 端口")
	interval := fs.Duration("interval", 15*time.Second, "多久拉一轮")
	timeout := fs.Duration("timeout", 5*time.Second, "单台拉取超时")
	lostAfter := fs.Duration("lost-after", 0, "多久没拉到算失联；0 = 3 轮")
	parallel := fs.Int("parallel", 32, "同时拉几台")
	webRoot := fs.String("web-root", "", "从该目录读 fleet.html 覆盖内置页面（仅前端开发用）")
	showVer := fs.Bool("version", false, "打印版本")
	fs.Parse(os.Args[1:])
	if *showVer {
		fmt.Println("nodedata-fleet", version)
		return
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
		fmt.Fprintf(os.Stderr, "nodedata-fleet: %s 里没有可用的主机：\n", *hostsPath)
		for _, e := range cur.Errors {
			fmt.Fprintln(os.Stderr, "  "+e)
		}
		fmt.Fprintln(os.Stderr, "格式：名字 地址:端口 dc=… rack=… product=… group=… tags=a,b")
		os.Exit(1)
	}
	for _, e := range cur.Errors {
		fmt.Fprintln(os.Stderr, "warn: "+e)
	}

	poller := NewPoller(*timeout, *parallel, "nodedata-fleet/"+version)
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
			if b, err := os.ReadFile(filepath.Join(*webRoot, "fleet.html")); err == nil {
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
	fmt.Fprintf(os.Stderr, "nodedata-fleet %s：%d 台，每 %s 拉一轮，http://%s/\n", version, len(cur.Hosts), *interval, addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "nodedata-fleet:", err)
		os.Exit(1)
	}
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
	if st.RoundAt == 0 || now.Unix()-st.RoundAt > int64(3*st.Interval) {
		status = "crit"
		why = append(why, "拉取停了")
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
