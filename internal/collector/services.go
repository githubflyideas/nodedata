// services.go — 服务识别：这台机器上跑着哪些服务，各自什么时候起来的。
//
// 全部只读 /proc：不连服务、不读配置文件、不执行任何命令（`mysqld --version` 这种
// 要在生产机上派生进程，不做）。这样才谈得上"scp 一个文件、跑一行命令"。
//
// 两层，职责分开：
//
//  1. **谁值得进视野** —— 动态判断，与名字无关：在监听端口 / 由 systemd 拉起 / 资源占用靠前。
//     一台跑着自研服务的机器，我们从没见过它，照样能看见它。
//  2. **它是什么** —— exe basename 查表 → 监听端口反查兜底 → 都不认就显示 exe 名。
//     认不出来不影响第一层，绝不因为识别失败而丢掉这个服务。
//
// 身份不是 PID：PID 会复用，一台跑几个月的机器早绕回来好几轮。
// 服务的身份是 `名字 + 监听端口集合`，实例的身份是 `PID + starttime`。
package collector

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Service 是一个被识别出的服务。
type Service struct {
	Name      string  `json:"name"`      // 人话名字：MySQL、Nginx…；认不出时是 exe basename
	Exe       string  `json:"exe"`       // 可执行文件 basename
	Kind      string  `json:"kind"`      // exe | port | systemd | resource —— 凭什么认出来的
	PID       int     `json:"pid"`       // 主进程（同组里最老的那个）
	Instances int     `json:"instances"` // 同名进程数（nginx 的 master+worker 算一个服务）
	StartTS   int64   `json:"start_ts"`  // 主进程启动时刻，unix 秒
	Ports     []int   `json:"ports"`     // 监听端口（已排序去重）
	Unit      string  `json:"unit"`      // systemd unit 名（若有）
	CPU       float64 `json:"cpu"`       // 占一个核的百分比
	RSS       uint64  `json:"rss"`       // 同组合计
	Self      bool    `json:"self,omitempty"`
}

// ID 是服务的稳定身份：名字 + 监听端口。重启后 PID 变、身份不变。
func (s Service) ID() string {
	if len(s.Ports) == 0 {
		return s.Name
	}
	ps := make([]string, len(s.Ports))
	for i, p := range s.Ports {
		ps[i] = strconv.Itoa(p)
	}
	return s.Name + ":" + strings.Join(ps, ",")
}

// knownExe 把 exe basename 翻译成人话。用 exe 而不是 comm：
// comm 被内核截断到 15 字节，`postgres: checkpointer` 这种直接废掉。
var knownExe = map[string]string{
	"nginx": "Nginx", "httpd": "Apache", "apache2": "Apache", "caddy": "Caddy",
	"haproxy": "HAProxy", "envoy": "Envoy", "traefik": "Traefik",
	"mysqld": "MySQL", "mariadbd": "MariaDB", "postgres": "PostgreSQL", "postmaster": "PostgreSQL",
	"mongod": "MongoDB", "redis-server": "Redis", "memcached": "Memcached",
	"clickhouse-server": "ClickHouse", "influxd": "InfluxDB", "etcd": "etcd",
	"dockerd": "Docker", "containerd": "containerd", "podman": "Podman", "crio": "CRI-O",
	"named": "BIND", "unbound": "Unbound", "dnsmasq": "dnsmasq", "coredns": "CoreDNS",
	"sshd": "SSH", "chronyd": "chrony", "ntpd": "NTP", "rsyslogd": "rsyslog",
	"php-fpm": "PHP-FPM", "uwsgi": "uWSGI", "gunicorn": "Gunicorn",
	"rabbitmq-server": "RabbitMQ", "beam.smp": "Erlang/RabbitMQ",
	"prometheus": "Prometheus", "grafana": "Grafana", "grafana-server": "Grafana",
	"kafka": "Kafka", "zookeeper": "ZooKeeper", "nodedata": "nodedata",
}

// knownPort 是端口反查，救"编译安装 + 改名部署"的场景：
// 用户把 mysqld 改名叫 db-main，exe 认不出来，但它监听 3306。
var knownPort = map[int]string{
	3306: "MySQL", 5432: "PostgreSQL", 6379: "Redis", 27017: "MongoDB",
	9200: "Elasticsearch", 9300: "Elasticsearch", 5601: "Kibana", 9600: "Logstash",
	11211: "Memcached", 5672: "RabbitMQ", 15672: "RabbitMQ", 9092: "Kafka", 2181: "ZooKeeper",
	8123: "ClickHouse", 9000: "ClickHouse", 2379: "etcd", 53: "DNS", 25: "SMTP",
	3000: "Grafana", 9090: "Prometheus",
}

// javaMain 从 java 的命令行里认出真正的服务。ELK 全是 java，靠 exe 分不开。
var javaMain = []struct{ token, name string }{
	{"org.elasticsearch.bootstrap.Elasticsearch", "Elasticsearch"},
	{"org.logstash.Logstash", "Logstash"},
	{"kafka.Kafka", "Kafka"},
	{"org.apache.zookeeper", "ZooKeeper"},
	{"org.apache.catalina", "Tomcat"},
}

type svcProc struct {
	pid, ppid int
	exe, comm string
	startTS   int64
	unit      string
	cpu       float64
	rss       uint64
	inodes    []uint64
	self      bool
}

// CollectServices 扫描进程与监听端口，返回当前服务集合。
// 端口映射较贵（/proc/net/tcp 是 O(连接数)），由调用方控制频率。
func (c *Collector) CollectServices(now time.Time) []Service {
	procRoot := c.cfg.ProcRoot
	bootTS := c.bootTime()
	listen := c.listenSockets() // inode → 端口

	d, err := os.Open(procRoot)
	if err != nil {
		return nil
	}
	names, _ := d.Readdirnames(-1)
	d.Close()

	selfPID := os.Getpid()
	var procs []svcProc
	for _, name := range names {
		if name == "" || name[0] < '0' || name[0] > '9' {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		base := filepath.Join(procRoot, name)
		st, err := os.ReadFile(filepath.Join(base, "stat"))
		if err != nil {
			continue
		}
		ps, ppid, ok := parseSvcStat(c, st)
		if !ok {
			continue
		}
		p := svcProc{pid: pid, ppid: ppid, comm: ps.comm, startTS: bootTS + int64(ps.start)/clockTicks,
			rss: ps.rss, self: pid == selfPID}
		if exe, err := os.Readlink(filepath.Join(base, "exe")); err == nil {
			p.exe = filepath.Base(exe)
			// 删除的可执行文件会变成 "/usr/sbin/nginx (deleted)"
			p.exe = strings.TrimSuffix(p.exe, " (deleted)")
		}
		if p.exe == "" {
			p.exe = ps.comm
		}
		p.unit = systemdUnit(filepath.Join(base, "cgroup"))
		if len(listen) > 0 {
			p.inodes = socketInodes(filepath.Join(base, "fd"))
		}
		procs = append(procs, p)
	}
	return c.groupServices(procs, listen, now)
}

const clockTicks = 100 // USER_HZ；Linux 上固定 100

func parseSvcStat(c *Collector, data []byte) (ps procStat, ppid int, ok bool) {
	ps, ok = c.parseProcStat(data)
	if !ok {
		return ps, 0, false
	}
	r := bytes.LastIndexByte(data, ')')
	f := c.splitFieldsBuf(data[r+2:])
	if len(f) < 2 {
		return ps, 0, false
	}
	v, err := strconv.Atoi(string(f[1])) // 字段 4 = ppid
	if err != nil {
		return ps, 0, false
	}
	return ps, v, true
}

// bootTime 返回开机时刻（unix 秒），用于把 starttime（jiffies）换成绝对时间。缓存。
func (c *Collector) bootTime() int64 {
	if c.bootTS != 0 {
		return c.bootTS
	}
	b, err := os.ReadFile(c.cfg.ProcRoot + "/stat")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			if t, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				c.bootTS = t
			}
			break
		}
	}
	return c.bootTS
}

// systemdUnit 从 cgroup 路径里取 unit 名，形如 .../mysqld.service。
func systemdUnit(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		i := strings.LastIndex(line, ".service")
		if i < 0 {
			continue
		}
		seg := line[:i]
		if j := strings.LastIndexAny(seg, "/"); j >= 0 {
			seg = seg[j+1:]
		}
		if seg != "" && !strings.HasPrefix(seg, "user@") {
			return seg + ".service"
		}
	}
	return ""
}

// socketInodes 读 /proc/PID/fd，返回它持有的 socket inode。
func socketInodes(fdDir string) []uint64 {
	ents, err := os.ReadDir(fdDir)
	if err != nil {
		return nil // 非 root 时读不到别人的 fd，属正常
	}
	var out []uint64
	for _, e := range ents {
		link, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		if err != nil || !strings.HasPrefix(link, "socket:[") {
			continue
		}
		if v, err := strconv.ParseUint(strings.TrimSuffix(link[8:], "]"), 10, 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// listenSockets 解析 /proc/net/{tcp,tcp6}，返回 inode → 监听端口。
func (c *Collector) listenSockets() map[uint64]int {
	out := map[uint64]int{}
	for _, f := range []string{"/net/tcp", "/net/tcp6"} {
		b, err := os.ReadFile(c.cfg.ProcRoot + f)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(b), "\n") {
			if i == 0 {
				continue // 表头
			}
			fs := strings.Fields(line)
			if len(fs) < 10 || fs[3] != "0A" { // 0A = TCP_LISTEN
				continue
			}
			_, portHex, ok := strings.Cut(fs[1], ":")
			if !ok {
				continue
			}
			port, err := strconv.ParseUint(portHex, 16, 32)
			if err != nil {
				continue
			}
			inode, err := strconv.ParseUint(fs[9], 10, 64)
			if err != nil {
				continue
			}
			out[inode] = int(port)
		}
	}
	return out
}

// groupServices 把进程按服务聚合。
func (c *Collector) groupServices(procs []svcProc, listen map[uint64]int, now time.Time) []Service {
	byPID := make(map[int]*svcProc, len(procs))
	for i := range procs {
		byPID[procs[i].pid] = &procs[i]
	}
	// 每个进程的监听端口
	ports := make(map[int][]int, 16)
	for i := range procs {
		for _, in := range procs[i].inodes {
			if p, ok := listen[in]; ok {
				ports[procs[i].pid] = append(ports[procs[i].pid], p)
			}
		}
	}
	// CPU 占用（复用进程采集的快照，避免再扫一遍）
	cpuByPID := map[int]float64{}
	for _, t := range c.snapshotCopy() {
		cpuByPID[t.PID] = t.CPU
	}

	type group struct {
		svc   Service
		procs []*svcProc
	}
	groups := map[string]*group{}
	for i := range procs {
		p := &procs[i]
		name, kind := identify(p, ports[p.pid], c.cfg.ProcRoot)
		if name == "" {
			continue
		}
		// 进视野：有监听端口 / 由 systemd 拉起 / 是自己 / CPU 或内存靠前
		worth := len(ports[p.pid]) > 0 || p.unit != "" || p.self ||
			cpuByPID[p.pid] >= 5 || p.rss >= 128<<20
		if !worth {
			continue
		}
		g := groups[name]
		if g == nil {
			g = &group{svc: Service{Name: name, Exe: p.exe, Kind: kind}}
			groups[name] = g
		}
		g.procs = append(g.procs, p)
	}

	out := make([]Service, 0, len(groups))
	for _, g := range groups {
		// 主进程 = 同组里最老的（nginx worker 重启不算服务重启）
		sort.Slice(g.procs, func(i, j int) bool {
			if g.procs[i].startTS != g.procs[j].startTS {
				return g.procs[i].startTS < g.procs[j].startTS
			}
			return g.procs[i].pid < g.procs[j].pid
		})
		main := g.procs[0]
		s := g.svc
		s.PID, s.StartTS, s.Instances = main.pid, main.startTS, len(g.procs)
		s.Unit = main.unit
		seen := map[int]bool{}
		for _, p := range g.procs {
			s.RSS += p.rss
			s.CPU += cpuByPID[p.pid]
			s.Self = s.Self || p.self
			for _, port := range ports[p.pid] {
				if !seen[port] {
					seen[port] = true
					s.Ports = append(s.Ports, port)
				}
			}
		}
		sort.Ints(s.Ports)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// identify 决定"它是什么"。顺序：exe 查表 > java 命令行 > 端口反查 > systemd unit > exe 名。
// exe 优先于端口：冲突时宁可按 exe 显示，也不要瞎猜成 MySQL 然后误导人。
func identify(p *svcProc, ports []int, procRoot string) (name, kind string) {
	if n, ok := knownExe[p.exe]; ok {
		return n, "exe"
	}
	if p.exe == "java" {
		if n := javaService(procRoot, p.pid); n != "" {
			return n, "exe"
		}
	}
	for _, port := range ports {
		if n, ok := knownPort[port]; ok {
			return n, "port"
		}
	}
	if len(ports) > 0 || p.unit != "" {
		if p.unit != "" {
			return p.exe, "systemd"
		}
		return p.exe, "port"
	}
	return p.exe, "resource"
}

// javaService 从 java 进程的命令行认出 ELK / Kafka 等。只读 cmdline，不执行任何东西。
func javaService(procRoot string, pid int) string {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return ""
	}
	s := string(bytes.ReplaceAll(b, []byte{0}, []byte{' '}))
	for _, m := range javaMain {
		if strings.Contains(s, m.token) {
			return m.name
		}
	}
	return ""
}

// snapshotCopy 取最近一轮进程快照（CPU 复用它，不再扫一遍 /proc）。
func (c *Collector) snapshotCopy() []ProcTop {
	p := &c.procs
	p.topMu.Lock()
	defer p.topMu.Unlock()
	return append([]ProcTop(nil), p.top...)
}
