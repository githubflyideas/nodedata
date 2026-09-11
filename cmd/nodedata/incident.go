// incident.go — 事故留证：L4 出现带归因的结论时，自动把当时的现场存成一个 JSON 文件。
//
// 凌晨三点出问题，人登上去时进程已经退出、D 状态早已恢复、dmesg 被刷掉。
// 这里在结论出现的那一刻留下：结论本身、偏离表、进程快照、责任进程的线程级 CPU 与内核栈、
// 全部 D 状态进程、最近的内核日志、原始 pressure/meminfo/vmstat/diskstats/net 文件。
//
// 不采集 /proc/PID/cmdline：命令行参数里常有密码，而证据会经 HTTP 提供。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/githubflyideas/nodedata/internal/diagnosis"
)

const (
	incidentKeep       = 100
	incidentSigCool    = 30 * time.Minute // 同一类 + 同一责任方
	incidentMinGap     = time.Minute      // 同一类别任意两次之间（不同类别互不阻挡）
	incidentThreadTop  = 10
	incidentStackDepth = 24
	incidentKmsgLines  = 100
)

// Recorder 负责触发与写盘。
type Recorder struct {
	dir      string
	procRoot string
	kmsgPath string
	host     string
	run      func() *diagnosis.Chain
	procs    func() []diagnosis.Proc
	devs     func() []diagnosis.Deviation

	mu        sync.Mutex
	lastSig   map[string]time.Time
	lastClass map[string]time.Time
}

func NewRecorder(dir, procRoot string, run func() *diagnosis.Chain, procs func() []diagnosis.Proc, devs func() []diagnosis.Deviation) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	return &Recorder{dir: dir, procRoot: procRoot, kmsgPath: "/dev/kmsg", host: host,
		run: run, procs: procs, devs: devs, lastSig: map[string]time.Time{}, lastClass: map[string]time.Time{}}, nil
}

// Loop 周期性执行 L4；与页面是否打开无关。
func (r *Recorder) Loop(every time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			r.Tick(now)
		}
	}
}

func signature(it diagnosis.Item) string {
	sig := it.Class
	if len(it.Culprits) > 0 {
		c := it.Culprits[0]
		sig += "|" + c.Kind + "|" + c.Name + "|" + strconv.Itoa(c.PID)
	}
	return sig
}

// Tick 跑一次 L4，对需要的结论留证，返回写出的证据 ID。
func (r *Recorder) Tick(now time.Time) []string {
	chain := r.run()
	if chain == nil {
		return nil
	}
	var ids []string
	for _, it := range chain.Items {
		if it.Class == "" || it.Level < diagnosis.Warning {
			continue // 只对带归因的结论留证；L0 的静态状态（磁盘 96%）不反复留
		}
		sig := signature(it)
		r.mu.Lock()
		// 按类别限流：实测里 IO 故障先触发一份 CPU 证据（写入本身吃 CPU），
		// 若用全局间隔，紧随其后的 IO 结论会被挡掉，真正的 IO 现场就丢了。
		cool := now.Sub(r.lastSig[sig]) < incidentSigCool || now.Sub(r.lastClass[it.Class]) < incidentMinGap
		if !cool {
			r.lastSig[sig], r.lastClass[it.Class] = now, now
		}
		r.mu.Unlock()
		if cool {
			continue
		}
		if id, err := r.Capture(it, chain, now); err == nil {
			ids = append(ids, id)
		} else {
			fmt.Fprintf(os.Stderr, "warn: 事故留证失败: %v\n", err)
		}
	}
	return ids
}

// Evidence 是一份事故证据。
type Evidence struct {
	ID         string                 `json:"id"`
	Host       string                 `json:"host"`
	Time       time.Time              `json:"time"`
	Version    string                 `json:"version"`
	Trigger    diagnosis.Item         `json:"trigger"`
	Chain      *diagnosis.Chain       `json:"chain"`
	Deviations []devRow               `json:"deviations"`
	Procs      []diagnosis.Proc       `json:"procs"`
	Threads    map[string][]threadRow `json:"threads"`     // PID → 线程
	ProcDetail map[string]procDetail  `json:"proc_detail"` // PID → 详情
	DState     []dRow                 `json:"d_state"`
	Kernel     []string               `json:"kernel_log"`
	System     map[string]string      `json:"system"`
	Errors     []string               `json:"errors,omitempty"`
}

type devRow struct {
	Metric string     `json:"metric"`
	Value  float64    `json:"value"`
	Unit   string     `json:"unit"`
	Z      []*float64 `json:"z"`
}

type threadRow struct {
	TID   int      `json:"tid"`
	Comm  string   `json:"comm"`
	State string   `json:"state"`
	CPU   float64  `json:"cpu"` // 1 秒采样，占一个核的百分比
	Wchan string   `json:"wchan,omitempty"`
	Stack []string `json:"stack,omitempty"`
}

type procDetail struct {
	Exe     string `json:"exe"`
	Status  string `json:"status"`
	IO      string `json:"io,omitempty"`
	FDCount int    `json:"fd_count"`
}

type dRow struct {
	PID   int      `json:"pid"`
	Comm  string   `json:"comm"`
	Wchan string   `json:"wchan,omitempty"`
	Stack []string `json:"stack,omitempty"`
}

// Capture 写一份证据；trigger 可以是手动构造的条目。
func (r *Recorder) Capture(trigger diagnosis.Item, chain *diagnosis.Chain, now time.Time) (string, error) {
	cls := trigger.Class
	if cls == "" {
		cls = "manual"
	}
	id := now.UTC().Format("20060102T150405Z") + "-" + classSlug(cls)
	ev := &Evidence{ID: id, Host: r.host, Time: now, Version: version, Trigger: trigger, Chain: chain,
		Threads: map[string][]threadRow{}, ProcDetail: map[string]procDetail{}, System: map[string]string{},
		Deviations: []devRow{}, Procs: []diagnosis.Proc{}, DState: []dRow{}, Kernel: []string{}}
	if r.procs != nil {
		if ps := r.procs(); ps != nil {
			ev.Procs = ps
		}
	}
	if r.devs != nil {
		for _, d := range r.devs() {
			row := devRow{Metric: d.MetricID, Value: d.Value, Unit: d.Unit}
			keep := false
			for _, z := range d.Z {
				if z != z { // NaN = 未就绪
					row.Z = append(row.Z, nil)
					continue
				}
				v := z
				row.Z = append(row.Z, &v)
				keep = keep || v >= 2 || v <= -2
			}
			if keep {
				ev.Deviations = append(ev.Deviations, row)
			}
		}
	}
	// 责任进程：线程级 CPU（1 秒两次采样）、内核栈、详情
	var pids []int
	for _, c := range trigger.Culprits {
		if c.PID > 0 && len(pids) < 3 {
			pids = append(pids, c.PID)
		}
	}
	if len(pids) > 0 {
		before := map[int]map[int]uint64{}
		for _, pid := range pids {
			before[pid] = r.threadTicks(pid)
		}
		time.Sleep(time.Second)
		for _, pid := range pids {
			ev.Threads[strconv.Itoa(pid)] = r.threads(pid, before[pid])
			ev.ProcDetail[strconv.Itoa(pid)] = r.detail(pid)
		}
	}
	if d := r.dstate(); d != nil {
		ev.DState = d
	}
	if kl, err := r.kernelLog(); err != nil {
		ev.Errors = append(ev.Errors, "kernel_log: "+err.Error())
	} else if kl != nil {
		ev.Kernel = kl
	}
	for _, f := range []string{"loadavg", "uptime", "meminfo", "vmstat", "pressure/cpu", "pressure/io",
		"pressure/memory", "diskstats", "net/dev", "net/snmp"} {
		if b, err := os.ReadFile(filepath.Join(r.procRoot, f)); err == nil {
			ev.System[f] = string(b)
		}
	}
	b, err := json.MarshalIndent(ev, "", " ")
	if err != nil {
		return "", err
	}
	tmp := filepath.Join(r.dir, "."+id+".tmp")
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, filepath.Join(r.dir, id+".json")); err != nil {
		return "", err
	}
	r.expire()
	return id, nil
}

func classSlug(cls string) string {
	switch cls {
	case diagnosis.ClassCPU:
		return "cpu"
	case diagnosis.ClassIO:
		return "io"
	case diagnosis.ClassMem:
		return "mem"
	case diagnosis.ClassNet:
		return "net"
	}
	return "manual"
}

// threadTicks 读某进程全部线程的 utime+stime。
func (r *Recorder) threadTicks(pid int) map[int]uint64 {
	out := map[int]uint64{}
	base := filepath.Join(r.procRoot, strconv.Itoa(pid), "task")
	ents, _ := os.ReadDir(base)
	for _, e := range ents {
		tid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if _, _, cpu, ok := readTaskStat(filepath.Join(base, e.Name(), "stat")); ok {
			out[tid] = cpu
		}
	}
	return out
}

// readTaskStat 解析 stat 的 comm、state、utime+stime（以最后一个 ')' 为界）。
func readTaskStat(path string) (comm, state string, cpu uint64, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	l, r := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if l < 0 || r <= l || r+2 > len(b) {
		return
	}
	f := strings.Fields(string(b[r+2:]))
	if len(f) < 13 {
		return
	}
	ut, _ := strconv.ParseUint(f[11], 10, 64)
	st, _ := strconv.ParseUint(f[12], 10, 64)
	return string(b[l+1 : r]), f[0], ut + st, true
}

func (r *Recorder) threads(pid int, before map[int]uint64) []threadRow {
	base := filepath.Join(r.procRoot, strconv.Itoa(pid), "task")
	var rows []threadRow
	for tid, cpu0 := range before {
		dir := filepath.Join(base, strconv.Itoa(tid))
		comm, state, cpu1, ok := readTaskStat(filepath.Join(dir, "stat"))
		if !ok {
			continue
		}
		d := 0.0
		if cpu1 >= cpu0 {
			d = float64(cpu1 - cpu0) // jiffies 每秒 = 占一个核的百分比（USER_HZ=100）
		}
		rows = append(rows, threadRow{TID: tid, Comm: comm, State: state, CPU: d})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CPU != rows[j].CPU {
			return rows[i].CPU > rows[j].CPU
		}
		return rows[i].TID < rows[j].TID
	})
	if len(rows) > incidentThreadTop {
		rows = rows[:incidentThreadTop]
	}
	for i := range rows {
		dir := filepath.Join(base, strconv.Itoa(rows[i].TID))
		rows[i].Wchan = readTrim(filepath.Join(dir, "wchan"))
		if i < 3 || rows[i].State == "D" {
			rows[i].Stack = readLines(filepath.Join(dir, "stack"), incidentStackDepth)
		}
	}
	return rows
}

func (r *Recorder) detail(pid int) procDetail {
	dir := filepath.Join(r.procRoot, strconv.Itoa(pid))
	exe, _ := os.Readlink(filepath.Join(dir, "exe"))
	fds, _ := os.ReadDir(filepath.Join(dir, "fd"))
	return procDetail{Exe: exe, Status: readTrim(filepath.Join(dir, "status")),
		IO: readTrim(filepath.Join(dir, "io")), FDCount: len(fds)}
}

func (r *Recorder) dstate() []dRow {
	ents, _ := os.ReadDir(r.procRoot)
	var rows []dRow
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(r.procRoot, e.Name())
		comm, state, _, ok := readTaskStat(filepath.Join(dir, "stat"))
		if !ok || state != "D" {
			continue
		}
		rows = append(rows, dRow{PID: pid, Comm: comm, Wchan: readTrim(filepath.Join(dir, "wchan")),
			Stack: readLines(filepath.Join(dir, "stack"), incidentStackDepth)})
		if len(rows) >= 20 {
			break
		}
	}
	return rows
}

// kernelLog 非阻塞读 /dev/kmsg 的整个环形缓冲，保留最后 N 条。
// 记录格式 "pri,seq,ts_usec,flags;message"，续行以空格开头（结构化字段）跳过。
func (r *Recorder) kernelLog() ([]string, error) {
	if fi, err := os.Stat(r.kmsgPath); err == nil && fi.Mode().IsRegular() { // 测试用普通文件
		return readKmsgFile(r.kmsgPath)
	}
	// 必须用裸 syscall：os.File 会把可 poll 的字符设备（/dev/kmsg）交给 Go 的 netpoller，
	// 非阻塞读到 EAGAIN 时不返回，而是挂起等"下一条内核消息"——实测留证 goroutine 卡死 4 分钟，
	// 之后所有留证全部停摆。
	fd, err := syscall.Open(r.kmsgPath, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	var ring []string
	push := func(rec []byte) {
		if l := formatKmsg(rec); l != "" {
			ring = append(ring, l)
			if len(ring) > incidentKmsgLines {
				ring = ring[1:]
			}
		}
	}
	buf := make([]byte, 8192)
	for i := 0; i < 1<<16; i++ { // 环形缓冲最多几万条，上限只是防御
		n, err := syscall.Read(fd, buf)
		if n > 0 {
			push(buf[:n])
		}
		switch {
		case err == nil && n == 0, err == syscall.EAGAIN: // 读完
			return ring, nil
		case err == syscall.EPIPE, err == syscall.EINTR: // 记录被覆盖 / 被信号打断，继续
			continue
		case err != nil:
			return ring, err
		}
	}
	return ring, nil
}

// readKmsgFile 读普通文件形式的 kmsg 记录（测试用），续行（空格开头）跳过。
func readKmsgFile(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" || line[0] == ' ' {
			continue
		}
		if l := formatKmsg([]byte(line)); l != "" {
			out = append(out, l)
		}
	}
	if len(out) > incidentKmsgLines {
		out = out[len(out)-incidentKmsgLines:]
	}
	return out, nil
}

// formatKmsg 把 "pri,seq,ts_usec,flags;message" 变成 "[秒.微秒] message"。
func formatKmsg(rec []byte) string {
	head, msg, ok := bytes.Cut(rec, []byte(";"))
	if !ok {
		return ""
	}
	if nl := bytes.IndexByte(msg, '\n'); nl >= 0 {
		msg = msg[:nl]
	}
	if p := bytes.Split(head, []byte(",")); len(p) >= 3 {
		if us, err := strconv.ParseUint(string(p[2]), 10, 64); err == nil {
			return fmt.Sprintf("[%d.%06d] %s", us/1e6, us%1e6, msg)
		}
	}
	return string(msg)
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readLines(path string, max int) []string {
	s := readTrim(path)
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > max {
		lines = lines[:max]
	}
	return lines
}

// IncidentMeta 是列表里的一行。
type IncidentMeta struct {
	ID      string             `json:"id"`
	Time    time.Time          `json:"time"`
	Title   string             `json:"title"`
	Class   string             `json:"class"`
	Level   diagnosis.Level    `json:"level"`
	Culprit *diagnosis.Culprit `json:"culprit,omitempty"`
}

// List 返回全部证据的摘要（新的在前）。
func (r *Recorder) List() []IncidentMeta {
	files, _ := filepath.Glob(filepath.Join(r.dir, "*.json"))
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	out := []IncidentMeta{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var ev struct {
			ID      string         `json:"id"`
			Time    time.Time      `json:"time"`
			Trigger diagnosis.Item `json:"trigger"`
		}
		if json.Unmarshal(b, &ev) != nil {
			continue
		}
		m := IncidentMeta{ID: ev.ID, Time: ev.Time, Title: ev.Trigger.Title, Class: ev.Trigger.Class, Level: ev.Trigger.Level}
		if len(ev.Trigger.Culprits) > 0 {
			c := ev.Trigger.Culprits[0]
			m.Culprit = &c
		}
		out = append(out, m)
	}
	return out
}

// Get 返回一份证据原文；id 只允许 List 里出现过的格式，防路径穿越。
func (r *Recorder) Get(id string) ([]byte, error) {
	if id == "" || strings.ContainsAny(id, "/\\.") {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(filepath.Join(r.dir, id+".json"))
}

func (r *Recorder) expire() {
	files, _ := filepath.Glob(filepath.Join(r.dir, "*.json"))
	sort.Strings(files)
	for len(files) > incidentKeep {
		os.Remove(files[0])
		files = files[1:]
	}
}
