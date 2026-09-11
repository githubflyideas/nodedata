// faultlab — 故障语料库第二层：在实验机（物理机/虚机）上真的制造故障，检验正在运行的 nodedata
// 能否在限定时间内给出"类别正确 + 责任方 PID 正确"的 L4 结论。
//
//	nodedata serve 先跑 ≥10 分钟（L1 就绪），然后：
//	./faultlab --url http://127.0.0.1:8888 --scenarios cpu,io,mem
//	sudo ./faultlab --scenarios net --iface eth0      # 需要 root、tc，以及该口上有 TCP 流量
//
// 每个故障由一个子进程制造（经符号链接 fl-cpu / fl-io / fl-mem 启动，进程名即场景名），
// faultlab 轮询 /api/diagnosis，直到出现指认该 PID 的结论，记录耗时；然后让故障再持续 --hold，
// 要求 /api/incidents 里出现指认该 PID 的事故证据。退出码非 0 = 有场景失败。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

type culprit struct {
	Kind string `json:"kind"`
	PID  int    `json:"pid"`
	Name string `json:"name"`
}
type item struct {
	Class    string    `json:"class"`
	Title    string    `json:"title"`
	Culprits []culprit `json:"culprits"`
}
type chain struct {
	Items []item `json:"items"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "child" {
		child(os.Args[2:])
		return
	}
	url := flag.String("url", "http://127.0.0.1:8888", "nodedata 地址")
	list := flag.String("scenarios", "cpu,io,mem", "要跑的场景：cpu,io,mem,net")
	timeout := flag.Duration("timeout", 5*time.Minute, "每个场景等待结论的上限")
	warmup := flag.Duration("warmup", 15*time.Minute, "等待 nodedata L1 就绪的上限")
	cool := flag.Duration("cooldown", 30*time.Second, "场景之间的间隔")
	dir := flag.String("dir", os.TempDir(), "io 场景写文件的目录（放在要测的盘上）")
	iface := flag.String("iface", "", "net 场景的网卡")
	loss := flag.String("loss", "10%", "net 场景 netem 丢包率")
	hold := flag.Duration("hold", 30*time.Second, "检出后故障再持续多久，用来检验事故留证")
	flag.Parse()

	if err := waitReady(*url, *warmup); err != nil {
		fmt.Fprintln(os.Stderr, "faultlab:", err)
		os.Exit(2)
	}
	self, _ := os.Executable()
	tmp, _ := os.MkdirTemp("", "faultlab")
	defer os.RemoveAll(tmp)

	type row struct {
		name, want, got, evidence string
		lat                       time.Duration
		ok                        bool
	}
	var rows []row
	for _, sc := range strings.Split(*list, ",") {
		sc = strings.TrimSpace(sc)
		var r row
		r.name = sc
		switch sc {
		case "cpu", "io", "mem":
			link := filepath.Join(tmp, "fl-"+sc)
			os.Symlink(self, link)
			cmd := exec.Command(link, "child", sc, *dir)
			cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
			if err := cmd.Start(); err != nil {
				r.got = "启动失败: " + err.Error()
				rows = append(rows, r)
				continue
			}
			pid := cmd.Process.Pid
			class := map[string]string{"cpu": "CPU", "io": "IO", "mem": "内存"}[sc]
			r.want = fmt.Sprintf("%s / PID %d (fl-%s)", class, pid, sc)
			r.lat, r.got, r.ok = poll(*url, *timeout, func(it item) bool {
				if it.Class != class {
					return false
				}
				for _, c := range it.Culprits {
					if c.PID > 0 {
						return c.PID == pid
					}
				}
				return false
			})
			// 检出后让故障再持续一会儿：留证每 15 秒跑一次，必须留下指认该 PID 的证据
			r.evidence = "—"
			if r.ok {
				time.Sleep(*hold)
				r.evidence = "缺失"
				var list []struct {
					ID      string   `json:"id"`
					Class   string   `json:"class"`
					Culprit *culprit `json:"culprit"`
				}
				if getJSON(*url+"/api/incidents", &list) == nil {
					for _, m := range list { // 类别与 PID 都要对上
						if m.Class == class && m.Culprit != nil && m.Culprit.PID == pid {
							r.evidence = m.ID
							break
						}
					}
				}
				r.ok = r.evidence != "缺失"
			}
			cmd.Process.Signal(syscall.SIGTERM)
			cmd.Wait()
		case "net":
			if *iface == "" {
				r.got = "跳过：需要 --iface"
				rows = append(rows, r)
				continue
			}
			add := exec.Command("tc", "qdisc", "add", "dev", *iface, "root", "netem", "loss", *loss)
			if out, err := add.CombinedOutput(); err != nil {
				r.got = "tc 失败: " + strings.TrimSpace(string(out))
				rows = append(rows, r)
				continue
			}
			r.want = "网络 / 接口 " + *iface
			r.lat, r.got, r.ok = poll(*url, *timeout, func(it item) bool {
				if it.Class != "网络" {
					return false
				}
				for _, c := range it.Culprits {
					if c.Kind == "interface" {
						return c.Name == *iface
					}
				}
				return true // 只有 tcp.retrans 偏离、没有单口指标时也算检出
			})
			exec.Command("tc", "qdisc", "del", "dev", *iface, "root").Run()
		default:
			r.got = "未知场景"
		}
		rows = append(rows, r)
		time.Sleep(*cool)
	}

	fail := 0
	fmt.Printf("\n%-5s %-6s %-34s %-9s %-26s %s\n", "场景", "结果", "期望", "耗时", "留证", "nodedata 的结论")
	for _, r := range rows {
		res, lat := "PASS", r.lat.Round(time.Second).String()
		if !r.ok {
			res, fail = "FAIL", fail+1
		}
		if r.lat == 0 {
			lat = "—"
		}
		fmt.Printf("%-5s %-6s %-34s %-9s %-26s %s\n", r.name, res, r.want, lat, r.evidence, r.got)
	}
	if fail > 0 {
		os.Exit(1)
	}
}

// waitReady：cpu.user 的 L1 就绪（1h 窗口最后一点 z[0] 非空）才开始。
func waitReady(url string, max time.Duration) error {
	deadline := time.Now().Add(max)
	for {
		var hm struct {
			Metrics []struct {
				ID     string `json:"metric_id"`
				Points []struct {
					Z []*int8 `json:"z"`
				} `json:"points"`
			} `json:"metrics"`
		}
		if err := getJSON(url+"/data/1h.json", &hm); err == nil {
			for _, m := range hm.Metrics {
				if m.ID == "cpu.user" && len(m.Points) > 0 {
					if z := m.Points[len(m.Points)-1].Z; len(z) > 0 && z[0] != nil {
						return nil
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nodedata 在 %v 内没有就绪（L1 需要约 6 分钟同时段历史）", max)
		}
		fmt.Fprintln(os.Stderr, "faultlab: 等待 nodedata L1 就绪…")
		time.Sleep(20 * time.Second)
	}
}

func poll(url string, timeout time.Duration, match func(item) bool) (time.Duration, string, bool) {
	start := time.Now()
	last := "（无结论）"
	for time.Since(start) < timeout {
		var c chain
		if err := getJSON(url+"/api/diagnosis?z=3", &c); err == nil {
			for _, it := range c.Items {
				if it.Class != "" {
					last = it.Title
				}
				if match(it) {
					return time.Since(start), it.Title, true
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
	return 0, last, false
}

func getJSON(u string, v interface{}) error {
	cl := http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: %s", u, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// ── 子进程：真正制造故障，收到 SIGTERM 退出并清理 ──

func child(args []string) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	switch args[0] {
	case "cpu": // 吃满 1 个核
		runtime.GOMAXPROCS(1)
		go func() {
			x := 0
			for {
				x++
			}
		}()
		<-stop
	case "io": // 持续顺序写 + fsync，文件到 1GiB 截断重来
		path := filepath.Join(args[1], fmt.Sprintf("faultlab-%d.dat", os.Getpid()))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		defer os.Remove(path)
		buf := make([]byte, 4<<20)
		for i := range buf {
			buf[i] = byte(i)
		}
		var n int64
		for {
			select {
			case <-stop:
				f.Close()
				return
			default:
			}
			f.Write(buf)
			if n += int64(len(buf)); n%(64<<20) == 0 {
				f.Sync()
			}
			if n >= 1<<30 {
				f.Truncate(0)
				f.Seek(0, 0)
				n = 0
			}
		}
	case "mem": // 每秒多占 32MiB（真的写页），到 1.5GiB 为止
		var hold [][]byte
		t := time.NewTicker(time.Second)
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if len(hold) < 48 {
					b := make([]byte, 32<<20)
					for i := 0; i < len(b); i += 4096 {
						b[i] = 1
					}
					hold = append(hold, b)
				}
			}
		}
	}
}
