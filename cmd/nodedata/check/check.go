// Package check 实现 L0 绝对判定：只看当前值与内核硬阈值的关系，不依赖历史。
// 原则：读不到的内核接口一律判 Level 0 并说明原因 —— "不可知" 不等于 "有问题"。
package check

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Check struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Level   int    `json:"level"`
	Message string `json:"message"`
}

type Category struct {
	Name   string  `json:"name"`
	Level  int     `json:"level"`
	Checks []Check `json:"checks"`
	Passed int     `json:"passed"`
	Total  int     `json:"total"`
}

type FullResult struct {
	Timestamp  time.Time  `json:"timestamp"`
	Categories []Category `json:"categories"`
	ExitCode   int        `json:"exit_code"`
}

// ── 判定辅助 ────────────────────────────────────────────────────────────

// grade 按 warn/fail 阈值定级；fail 为 0 表示这项最高只到 warn。
func grade(v, warn, fail float64) int {
	if fail > 0 && v >= fail {
		return 2
	}
	if v >= warn {
		return 1
	}
	return 0
}

func ok(id, name, msg string) Check   { return Check{id, name, 0, msg} }
func skip(id, name, why string) Check { return Check{id, name, 0, "未检查：" + why} }

// judge 生成一项带阈值的检查：v 超过 warn/fail 即升级，消息里带上实测值。
func judge(id, name string, v, warn, fail float64, msg string) Check {
	return Check{ID: id, Name: name, Level: grade(v, warn, fail), Message: msg}
}

// judgeH 同 judge，但后果说明只在真的触发时才拼进消息，
// 免得 0% 也挂着一句 "已经在拖慢任务"。
func judgeH(id, name string, v, warn, fail float64, msg, hint string) Check {
	c := judge(id, name, v, warn, fail, msg)
	if c.Level > 0 {
		c.Message += "；" + hint
	}
	return c
}

// ── /proc /sys 读取 ────────────────────────────────────────────────────

var procRoot = "/proc"
var sysRoot = "/sys"

// SetRoots 便于测试时指向假的 /proc、/sys。
func SetRoots(proc, sys string) { procRoot, sysRoot = proc, sys }

func readTrim(p string) (string, bool) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

func readInt(p string) (int64, bool) {
	s, okk := readTrim(p)
	if !okk {
		return 0, false
	}
	if i := strings.IndexAny(s, " \t\n"); i > 0 {
		s = s[:i]
	}
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}

// meminfo 返回 /proc/meminfo 的字节值（内核给的是 kB）。
func meminfo() map[string]float64 {
	out := map[string]float64{}
	b, err := os.ReadFile(procRoot + "/meminfo")
	if err != nil {
		return out
	}
	for _, ln := range strings.Split(string(b), "\n") {
		i := strings.IndexByte(ln, ':')
		if i <= 0 {
			continue
		}
		f := strings.Fields(ln[i+1:])
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			continue
		}
		if len(f) > 1 && f[1] == "kB" {
			v *= 1024
		}
		out[ln[:i]] = v
	}
	return out
}

// cpuJiffies 读 /proc/stat 第一行 cpu 的各态 jiffies。
func cpuJiffies() ([]float64, bool) {
	b, err := os.ReadFile(procRoot + "/stat")
	if err != nil {
		return nil, false
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(ln, "cpu ") {
			continue
		}
		f := strings.Fields(ln)[1:]
		out := make([]float64, 0, len(f))
		for _, x := range f {
			v, err := strconv.ParseFloat(x, 64)
			if err != nil {
				return nil, false
			}
			out = append(out, v)
		}
		if len(out) < 8 {
			return nil, false
		}
		return out, true
	}
	return nil, false
}

// cpuShare 采样两次，返回各态占比（%）。索引同 /proc/stat：
// 0 user 1 nice 2 system 3 idle 4 iowait 5 irq 6 softirq 7 steal
func cpuShare(gap time.Duration) ([]float64, bool) {
	a, okA := cpuJiffies()
	if !okA {
		return nil, false
	}
	time.Sleep(gap)
	b, okB := cpuJiffies()
	if !okB || len(b) != len(a) {
		return nil, false
	}
	var tot float64
	d := make([]float64, len(a))
	for i := range a {
		d[i] = b[i] - a[i]
		if d[i] < 0 {
			d[i] = 0
		}
		tot += d[i]
	}
	if tot <= 0 {
		return nil, false
	}
	for i := range d {
		d[i] = d[i] / tot * 100
	}
	return d, true
}

func nproc() int {
	b, err := os.ReadFile(procRoot + "/stat")
	if err != nil {
		return 0
	}
	n := 0
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(ln, "cpu") && len(ln) > 3 && ln[3] >= '0' && ln[3] <= '9' {
			n++
		}
	}
	return n
}

// pressureSome 读 /proc/pressure/<kind> 的 some avg10。
func pressureSome(kind string) (float64, bool) {
	b, err := os.ReadFile(procRoot + "/pressure/" + kind)
	if err != nil {
		return 0, false
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(ln, "some") {
			continue
		}
		for _, f := range strings.Fields(ln) {
			if strings.HasPrefix(f, "avg10=") {
				v, err := strconv.ParseFloat(f[6:], 64)
				return v, err == nil
			}
		}
	}
	return 0, false
}

// kvLine 在多行 "key value..." 文本里取某 key 的第 n 个字段。
func kvField(path, key string, n int) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, ln := range strings.Split(string(b), "\n") {
		f := strings.Fields(ln)
		if len(f) > n && f[0] == key {
			v, err := strconv.ParseFloat(f[n], 64)
			return v, err == nil
		}
	}
	return 0, false
}

// snmpVal 解析 /proc/net/snmp 与 /proc/net/netstat 的 "表头行 / 数值行" 成对格式。
func snmpVal(path, prefix, field string) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	lines := strings.Split(string(b), "\n")
	for i := 0; i+1 < len(lines); i++ {
		if !strings.HasPrefix(lines[i], prefix+":") {
			continue
		}
		head := strings.Fields(lines[i])
		val := strings.Fields(lines[i+1])
		if len(val) == 0 || val[0] != prefix+":" {
			continue
		}
		for j := 1; j < len(head) && j < len(val); j++ {
			if head[j] == field {
				v, err := strconv.ParseFloat(val[j], 64)
				return v, err == nil
			}
		}
	}
	return 0, false
}

// diskLines 返回 /proc/diskstats 里真实块设备（跳过分区与 loop/ram）的字段切片。
func diskLines() [][]string {
	b, err := os.ReadFile(procRoot + "/diskstats")
	if err != nil {
		return nil
	}
	var out [][]string
	for _, ln := range strings.Split(string(b), "\n") {
		f := strings.Fields(ln)
		if len(f) < 14 {
			continue
		}
		name := f[2]
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") ||
			strings.HasPrefix(name, "dm-") || strings.HasPrefix(name, "zram") {
			continue
		}
		if _, err := os.Stat(sysRoot + "/block/" + name); err != nil {
			continue // 是分区而不是整盘
		}
		out = append(out, f)
	}
	return out
}

type mountEnt struct{ dev, dir, fstype, opts string }

func mounts() []mountEnt {
	b, err := os.ReadFile(procRoot + "/mounts")
	if err != nil {
		return nil
	}
	var out []mountEnt
	for _, ln := range strings.Split(string(b), "\n") {
		f := strings.Fields(ln)
		if len(f) >= 4 {
			out = append(out, mountEnt{f[0], f[1], f[2], f[3]})
		}
	}
	return out
}

// realFS 判断是否是需要关心空间的真实文件系统。
func realFS(t string) bool {
	switch t {
	case "ext2", "ext3", "ext4", "xfs", "btrfs", "f2fs", "jfs", "reiserfs",
		"zfs", "overlay", "vfat", "ntfs3":
		return true
	}
	return false
}

type fsUsage struct{ spacePct, inodePct float64 }

func statfsUsage(dir string) (fsUsage, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fsUsage{}, false
	}
	var u fsUsage
	if st.Blocks > 0 {
		u.spacePct = float64(st.Blocks-st.Bavail) / float64(st.Blocks) * 100
	}
	if st.Files > 0 {
		u.inodePct = float64(st.Files-st.Ffree) / float64(st.Files) * 100
	}
	return u, true
}

func pct(a, b float64) float64 {
	if b <= 0 {
		return 0
	}
	return a / b * 100
}

func hb(v float64) string {
	u := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for v >= 1024 && i < len(u)-1 {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", v, u[i])
}

// ── T：时间 / 时钟源 ───────────────────────────────────────────────────

func checkTimeCategory(ctx context.Context) Category {
	cat := Category{Name: "Time/Sync"}
	now := time.Now()

	c1 := Check{ID: "T01", Name: "系统时间合理性"}
	if now.Year() < 2020 || now.Year() > 2050 {
		c1.Level, c1.Message = 2, "系统时间 "+now.Format("2006-01-02")+" 明显不对，证书与日志时序都会出问题"
	} else {
		c1.Message = now.Format("2006-01-02 15:04:05 -0700")
	}

	var c2 Check
	if up, okk := readTrim(procRoot + "/uptime"); okk {
		f := strings.Fields(up)
		sec, _ := strconv.ParseFloat(f[0], 64)
		lvl := 0
		msg := fmt.Sprintf("已运行 %s", (time.Duration(sec) * time.Second).Truncate(time.Minute))
		if sec < 600 {
			lvl = 1
			msg = fmt.Sprintf("刚启动 %.0f 秒，基线与累计计数器都还不可信", sec)
		}
		c2 = Check{ID: "T02", Name: "开机时长", Level: lvl, Message: msg}
	} else {
		c2 = skip("T02", "开机时长", "读不到 /proc/uptime")
	}

	var c3 Check
	cs, okk := readTrim(sysRoot + "/devices/system/clocksource/clocksource0/current_clocksource")
	switch {
	case !okk:
		c3 = skip("T03", "时钟源", "内核未导出 clocksource")
	case cs == "jiffies" || cs == "acpi_pm":
		c3 = Check{ID: "T03", Name: "时钟源", Level: 1,
			Message: "当前时钟源 " + cs + "，精度差，延迟类指标会失真"}
	default:
		c3 = ok("T03", "时钟源", cs)
	}

	cat.Checks = []Check{c1, c2, c3}
	updateCategoryLevel(&cat)
	return cat
}

// ── C：CPU ─────────────────────────────────────────────────────────────

func checkCPUCategory(ctx context.Context) Category {
	cat := Category{Name: "CPU"}
	n := nproc()
	cs := make([]Check, 0, 7)

	if n == 0 {
		cs = append(cs, Check{ID: "C01", Name: "CPU 核数", Level: 2, Message: "/proc/stat 里读不到任何 cpuN"})
	} else {
		cs = append(cs, ok("C01", "CPU 核数", fmt.Sprintf("%d 核", n)))
	}

	// C02 运行队列：1 分钟负载 / 核数
	if la, okk := readTrim(procRoot + "/loadavg"); okk && n > 0 {
		f := strings.Fields(la)
		l1, _ := strconv.ParseFloat(f[0], 64)
		r := l1 / float64(n)
		cs = append(cs, judge("C02", "运行队列压力", r, 2, 4,
			fmt.Sprintf("load1 %.2f / %d 核 = %.2f 每核", l1, n, r)))
	} else {
		cs = append(cs, skip("C02", "运行队列压力", "读不到 /proc/loadavg"))
	}

	sh, shOK := cpuShare(120 * time.Millisecond)
	mk := func(id, name string, idx int, warn, fail float64, tail string) Check {
		if !shOK {
			return skip(id, name, "读不到 /proc/stat")
		}
		return judge(id, name, sh[idx], warn, fail,
			fmt.Sprintf("%.1f%%%s", sh[idx], tail))
	}
	cs = append(cs,
		mk("C03", "steal 时间", 7, 5, 15, "（被宿主抢走的 CPU）"),
		mk("C04", "iowait", 4, 20, 40, "（CPU 卡在等 I/O）"),
		mk("C05", "softirq", 6, 15, 30, "（软中断，多为网络收包）"),
		mk("C06", "system 态", 2, 30, 60, "（内核态占比）"),
	)

	if p, okk := pressureSome("cpu"); okk {
		cs = append(cs, judgeH("C07", "PSI cpu 阻塞", p, 20, 50,
			fmt.Sprintf("some avg10 = %.2f%%", p), "有任务在排队等 CPU"))
	} else {
		cs = append(cs, skip("C07", "PSI cpu 阻塞", "内核未开 CONFIG_PSI"))
	}

	cat.Checks = cs
	updateCategoryLevel(&cat)
	return cat
}

// ── M：内存 ────────────────────────────────────────────────────────────

func checkMemoryCategory(ctx context.Context) Category {
	cat := Category{Name: "Memory"}
	m := meminfo()
	cs := make([]Check, 0, 8)
	total := m["MemTotal"]

	if total <= 0 {
		for i, nm := range []string{"可用内存", "Swap 使用", "脏页", "回写堆积",
			"OOM 击杀", "Commit 超额", "Slab 占比", "PSI mem 阻塞"} {
			cs = append(cs, skip(fmt.Sprintf("M%02d", i+1), nm, "读不到 /proc/meminfo"))
		}
		cat.Checks = cs
		updateCategoryLevel(&cat)
		return cat
	}

	used := pct(total-m["MemAvailable"], total)
	cs = append(cs, judge("M01", "可用内存", used, 85, 95,
		fmt.Sprintf("已用 %.1f%%，可用 %s / 共 %s", used, hb(m["MemAvailable"]), hb(total))))

	if m["SwapTotal"] > 0 {
		sw := pct(m["SwapTotal"]-m["SwapFree"], m["SwapTotal"])
		cs = append(cs, judgeH("M02", "Swap 使用", sw, 20, 60,
			fmt.Sprintf("%.1f%%（%s / %s）", sw, hb(m["SwapTotal"]-m["SwapFree"]), hb(m["SwapTotal"])),
			"换页会直接拉高尾延迟"))
	} else {
		cs = append(cs, ok("M02", "Swap 使用", "未配置 swap"))
	}

	d := pct(m["Dirty"], total)
	cs = append(cs, judge("M03", "脏页", d, 5, 15,
		fmt.Sprintf("%s 占内存 %.2f%%", hb(m["Dirty"]), d)))

	cs = append(cs, judge("M04", "回写堆积", m["Writeback"], 64<<20, 512<<20,
		fmt.Sprintf("Writeback %s（正在刷盘、还没落地的数据）", hb(m["Writeback"]))))

	if v, okk := kvField(procRoot+"/vmstat", "oom_kill", 1); okk {
		lvl, msg := 0, "累计 0 次"
		if v > 0 {
			lvl = 2
			msg = fmt.Sprintf("累计 %.0f 次 OOM 击杀，本机确实杀过进程", v)
		}
		cs = append(cs, Check{ID: "M05", Name: "OOM 击杀", Level: lvl, Message: msg})
	} else {
		cs = append(cs, skip("M05", "OOM 击杀", "内核未导出 vmstat.oom_kill"))
	}

	if m["CommitLimit"] > 0 {
		c := pct(m["Committed_AS"], m["CommitLimit"])
		cs = append(cs, judge("M06", "Commit 超额", c, 90, 120,
			fmt.Sprintf("已承诺 %s / 上限 %s = %.1f%%", hb(m["Committed_AS"]), hb(m["CommitLimit"]), c)))
	} else {
		cs = append(cs, skip("M06", "Commit 超额", "读不到 CommitLimit"))
	}

	sl := pct(m["Slab"], total)
	cs = append(cs, judge("M07", "Slab 占比", sl, 20, 35,
		fmt.Sprintf("内核对象占 %s = %.1f%%", hb(m["Slab"]), sl)))

	if p, okk := pressureSome("memory"); okk {
		cs = append(cs, judgeH("M08", "PSI mem 阻塞", p, 5, 20,
			fmt.Sprintf("some avg10 = %.2f%%", p), "回收内存已经在拖慢任务"))
	} else {
		cs = append(cs, skip("M08", "PSI mem 阻塞", "内核未开 CONFIG_PSI"))
	}

	cat.Checks = cs
	updateCategoryLevel(&cat)
	return cat
}

// ── D：磁盘 / IO ───────────────────────────────────────────────────────

func checkDiskIOCategory(ctx context.Context) Category {
	cat := Category{Name: "Disk/IO"}
	cs := make([]Check, 0, 8)

	if u, okk := statfsUsage("/"); okk {
		cs = append(cs,
			judge("D01", "根分区空间", u.spacePct, 85, 95, fmt.Sprintf("已用 %.1f%%", u.spacePct)),
			judge("D02", "根分区 inode", u.inodePct, 85, 95, fmt.Sprintf("已用 %.1f%%", u.inodePct)))
	} else {
		cs = append(cs, skip("D01", "根分区空间", "statfs / 失败"),
			skip("D02", "根分区 inode", "statfs / 失败"))
	}

	// D03 只读重挂：真实文件系统被内核踢成 ro 是硬故障
	var ro []string
	for _, mt := range mounts() {
		if !realFS(mt.fstype) {
			continue
		}
		for _, o := range strings.Split(mt.opts, ",") {
			if o == "ro" {
				ro = append(ro, mt.dir)
			}
		}
	}
	if len(ro) > 0 {
		cs = append(cs, Check{ID: "D03", Name: "只读重挂", Level: 2,
			Message: "以下挂载点是只读的：" + strings.Join(ro, " ")})
	} else {
		cs = append(cs, ok("D03", "只读重挂", "没有真实文件系统被挂成 ro"))
	}

	if p, okk := pressureSome("io"); okk {
		cs = append(cs, judgeH("D04", "PSI io 阻塞", p, 20, 50,
			fmt.Sprintf("some avg10 = %.2f%%", p), "任务正卡在 I/O 上"))
	} else {
		cs = append(cs, skip("D04", "PSI io 阻塞", "内核未开 CONFIG_PSI"))
	}

	dl := diskLines()
	if len(dl) == 0 {
		cs = append(cs, skip("D05", "块设备", "/proc/diskstats 里没有整盘设备"),
			skip("D06", "在途 I/O", "同上"), skip("D07", "平均等待", "同上"))
	} else {
		names := make([]string, 0, len(dl))
		var inflight float64
		for _, f := range dl {
			names = append(names, f[2])
			v, _ := strconv.ParseFloat(f[11], 64) // field 9: I/Os currently in progress
			if v > inflight {
				inflight = v
			}
		}
		cs = append(cs, ok("D05", "块设备", fmt.Sprintf("%d 个：%s", len(dl), strings.Join(names, " "))))
		cs = append(cs, judge("D06", "在途 I/O", inflight, 64, 256,
			fmt.Sprintf("单盘最高 %.0f 个请求在途", inflight)))

		// D07 用两次采样算真实 await：等待毫秒增量 / 完成请求增量
		time.Sleep(120 * time.Millisecond)
		var worst float64
		var worstDev string
		for _, a := range dl {
			for _, b := range diskLines() {
				if b[2] != a[2] {
					continue
				}
				rd0, _ := strconv.ParseFloat(a[3], 64)
				wr0, _ := strconv.ParseFloat(a[7], 64)
				t0, _ := strconv.ParseFloat(a[6], 64)
				t0w, _ := strconv.ParseFloat(a[10], 64)
				rd1, _ := strconv.ParseFloat(b[3], 64)
				wr1, _ := strconv.ParseFloat(b[7], 64)
				t1, _ := strconv.ParseFloat(b[6], 64)
				t1w, _ := strconv.ParseFloat(b[10], 64)
				ios := (rd1 - rd0) + (wr1 - wr0)
				ms := (t1 - t0) + (t1w - t0w)
				if ios > 0 && ms/ios > worst {
					worst, worstDev = ms/ios, b[2]
				}
			}
		}
		if worstDev == "" {
			cs = append(cs, ok("D07", "平均等待", "采样窗口内没有 I/O 完成，无法计算"))
		} else {
			cs = append(cs, judge("D07", "平均等待", worst, 50, 200,
				fmt.Sprintf("%s 最慢，平均 %.1f ms/请求", worstDev, worst)))
		}
	}

	if v, okk := kvField(procRoot+"/stat", "procs_blocked", 1); okk {
		cs = append(cs, judge("D08", "阻塞进程数", v, 4, 16,
			fmt.Sprintf("%.0f 个进程处于不可中断等待（D 状态）", v)))
	} else {
		cs = append(cs, skip("D08", "阻塞进程数", "读不到 /proc/stat procs_blocked"))
	}

	cat.Checks = cs
	updateCategoryLevel(&cat)
	return cat
}

// ── N：网络 ────────────────────────────────────────────────────────────

func checkNetworkCategory(ctx context.Context) Category {
	cat := Category{Name: "Network"}
	cs := make([]Check, 0, 6)

	// N01 至少一块非 lo 网卡是 up
	var up []string
	if ents, err := os.ReadDir(sysRoot + "/class/net"); err == nil {
		for _, e := range ents {
			if e.Name() == "lo" {
				continue
			}
			if st, okk := readTrim(sysRoot + "/class/net/" + e.Name() + "/operstate"); okk &&
				(st == "up" || st == "unknown") {
				up = append(up, e.Name())
			}
		}
	}
	switch {
	case len(up) == 0:
		cs = append(cs, Check{ID: "N01", Name: "网卡状态", Level: 2, Message: "没有一块非 lo 网卡处于 up"})
	default:
		cs = append(cs, ok("N01", "网卡状态", "up: "+strings.Join(up, " ")))
	}

	// N02/N03 收发错误与丢包占比（累计值，看的是比例而不是绝对值）
	var rxP, rxE, rxD, txP, txE, txD float64
	if b, err := os.ReadFile(procRoot + "/net/dev"); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			i := strings.IndexByte(ln, ':')
			if i <= 0 || strings.TrimSpace(ln[:i]) == "lo" {
				continue
			}
			f := strings.Fields(ln[i+1:])
			if len(f) < 12 {
				continue
			}
			g := func(k int) float64 { v, _ := strconv.ParseFloat(f[k], 64); return v }
			rxP, rxE, rxD = rxP+g(1), rxE+g(2), rxD+g(3)
			txP, txE, txD = txP+g(9), txE+g(10), txD+g(11)
		}
	}
	if rxP > 0 {
		r := pct(rxE+rxD, rxP)
		cs = append(cs, judge("N02", "收包错误/丢包", r, 0.01, 0.1,
			fmt.Sprintf("%.0f 错 + %.0f 丢 / %.0f 收 = %.4f%%", rxE, rxD, rxP, r)))
	} else {
		cs = append(cs, skip("N02", "收包错误/丢包", "还没有收包计数"))
	}
	if txP > 0 {
		r := pct(txE+txD, txP)
		cs = append(cs, judge("N03", "发包错误/丢包", r, 0.01, 0.1,
			fmt.Sprintf("%.0f 错 + %.0f 丢 / %.0f 发 = %.4f%%", txE, txD, txP, r)))
	} else {
		cs = append(cs, skip("N03", "发包错误/丢包", "还没有发包计数"))
	}

	// N04 TCP 重传率
	out, ok1 := snmpVal(procRoot+"/net/snmp", "Tcp", "OutSegs")
	rt, ok2 := snmpVal(procRoot+"/net/snmp", "Tcp", "RetransSegs")
	if ok1 && ok2 && out > 0 {
		r := pct(rt, out)
		cs = append(cs, judge("N04", "TCP 重传率", r, 1, 5,
			fmt.Sprintf("%.0f / %.0f = %.3f%%", rt, out, r)))
	} else {
		cs = append(cs, skip("N04", "TCP 重传率", "读不到 /proc/net/snmp Tcp"))
	}

	// N05 listen 队列溢出：一旦非零就是真丢过连接
	ov, ok3 := snmpVal(procRoot+"/net/netstat", "TcpExt", "ListenOverflows")
	dp, ok4 := snmpVal(procRoot+"/net/netstat", "TcpExt", "ListenDrops")
	if ok3 || ok4 {
		lvl, msg := 0, "累计 0 次"
		if ov+dp > 0 {
			lvl = 1
			msg = fmt.Sprintf("累计溢出 %.0f、丢弃 %.0f —— accept 队列曾经满过", ov, dp)
		}
		cs = append(cs, Check{ID: "N05", Name: "listen 队列溢出", Level: lvl, Message: msg})
	} else {
		cs = append(cs, skip("N05", "listen 队列溢出", "读不到 /proc/net/netstat TcpExt"))
	}

	// N06 邻居表水位
	th3, ok5 := readInt(procRoot + "/sys/net/ipv4/neigh/default/gc_thresh3")
	if b, err := os.ReadFile(procRoot + "/net/arp"); err == nil && ok5 && th3 > 0 {
		n := float64(strings.Count(strings.TrimSpace(string(b)), "\n")) // 减表头
		r := pct(n, float64(th3))
		cs = append(cs, judge("N06", "邻居表水位", r, 70, 90,
			fmt.Sprintf("%.0f / %d = %.1f%%", n, th3, r)))
	} else {
		cs = append(cs, skip("N06", "邻居表水位", "读不到 arp 表或 gc_thresh3"))
	}

	cat.Checks = cs
	updateCategoryLevel(&cat)
	return cat
}

// ── F：文件系统 ────────────────────────────────────────────────────────

func checkFilesystemCategory(ctx context.Context) Category {
	cat := Category{Name: "Filesystem"}
	cs := make([]Check, 0, 3)

	// F01 真的写一次，而不是只看挂载选项
	tmp, err := os.CreateTemp("", ".nodedata-probe-*")
	if err == nil {
		_, werr := tmp.WriteString("probe")
		cerr := tmp.Close()
		os.Remove(tmp.Name())
		if werr != nil || cerr != nil {
			cs = append(cs, Check{ID: "F01", Name: "临时目录可写", Level: 2,
				Message: fmt.Sprintf("能建文件但写不进去：%v", werr)})
		} else {
			cs = append(cs, ok("F01", "临时目录可写", "实际写入并删除成功"))
		}
	} else {
		cs = append(cs, Check{ID: "F01", Name: "临时目录可写", Level: 2,
			Message: fmt.Sprintf("建临时文件失败：%v", err)})
	}

	// F02 除根以外的真实挂载点，取最紧张的一个
	var worst float64
	var worstDir string
	for _, mt := range mounts() {
		if !realFS(mt.fstype) || mt.dir == "/" {
			continue
		}
		if u, okk := statfsUsage(mt.dir); okk && u.spacePct > worst {
			worst, worstDir = u.spacePct, mt.dir
		}
	}
	if worstDir == "" {
		cs = append(cs, ok("F02", "其他分区空间", "除根以外没有独立的真实分区"))
	} else {
		cs = append(cs, judge("F02", "其他分区空间", worst, 85, 95,
			fmt.Sprintf("%s 已用 %.1f%%（最紧张的一个）", worstDir, worst)))
	}

	// F03 全局打开文件数水位
	if s, okk := readTrim(procRoot + "/sys/fs/file-nr"); okk {
		f := strings.Fields(s)
		if len(f) >= 3 {
			cur, _ := strconv.ParseFloat(f[0], 64)
			max, _ := strconv.ParseFloat(f[2], 64)
			if max > 1e12 { // 容器里常见的 "等于没设上限"
				cs = append(cs, ok("F03", "打开文件数水位",
					fmt.Sprintf("已打开 %.0f，file-max 未设实际上限", cur)))
			} else {
				r := pct(cur, max)
				cs = append(cs, judge("F03", "打开文件数水位", r, 70, 90,
					fmt.Sprintf("%.0f / %.0f = %.2f%%", cur, max, r)))
			}
		} else {
			cs = append(cs, skip("F03", "打开文件数水位", "file-nr 格式异常"))
		}
	} else {
		cs = append(cs, skip("F03", "打开文件数水位", "读不到 /proc/sys/fs/file-nr"))
	}

	cat.Checks = cs
	updateCategoryLevel(&cat)
	return cat
}

// ── CT：conntrack ──────────────────────────────────────────────────────

func checkConntrackCategory(ctx context.Context) Category {
	cat := Category{Name: "Conntrack"}
	cs := make([]Check, 0, 2)

	max, okMax := readInt(procRoot + "/sys/net/netfilter/nf_conntrack_max")
	cnt, okCnt := readInt(procRoot + "/sys/net/netfilter/nf_conntrack_count")

	switch {
	case !okMax && !okCnt:
		// 没加载 nf_conntrack 模块是正常状态，不是告警
		cs = append(cs, ok("CT01", "conntrack 水位", "本机未启用 nf_conntrack，无表可满"))
	case !okCnt || max <= 0:
		cs = append(cs, skip("CT01", "conntrack 水位", "读到 max 但读不到 count"))
	default:
		r := pct(float64(cnt), float64(max))
		cs = append(cs, judgeH("CT01", "conntrack 水位", r, 80, 95,
			fmt.Sprintf("%d / %d = %.1f%%", cnt, max, r), "表满之后新连接会被直接丢掉"))
	}

	// drop 列是每 CPU 一行的十六进制表；逐行累加
	if b, err := os.ReadFile(procRoot + "/net/stat/nf_conntrack"); err == nil {
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		var drop, insertFail float64
		if len(lines) > 1 {
			head := strings.Fields(lines[0])
			di, ii := -1, -1
			for i, h := range head {
				switch h {
				case "drop":
					di = i
				case "insert_failed":
					ii = i
				}
			}
			for _, ln := range lines[1:] {
				f := strings.Fields(ln)
				if di >= 0 && di < len(f) {
					v, _ := strconv.ParseUint(f[di], 16, 64)
					drop += float64(v)
				}
				if ii >= 0 && ii < len(f) {
					v, _ := strconv.ParseUint(f[ii], 16, 64)
					insertFail += float64(v)
				}
			}
		}
		lvl, msg := 0, "累计 0 次丢弃与插入失败"
		if drop+insertFail > 0 {
			lvl = 1
			msg = fmt.Sprintf("累计丢弃 %.0f、插入失败 %.0f", drop, insertFail)
		}
		cs = append(cs, Check{ID: "CT02", Name: "conntrack 丢弃", Level: lvl, Message: msg})
	} else {
		cs = append(cs, ok("CT02", "conntrack 丢弃", "未启用 nf_conntrack，无统计可读"))
	}

	cat.Checks = cs
	updateCategoryLevel(&cat)
	return cat
}

// sockstat 从 /proc/net/sockstat 的 "TCP: inuse 5 orphan 0 tw 0 ..." 里取指定字段。
func sockstat(prefix, field string) (float64, bool) {
	b, err := os.ReadFile(procRoot + "/net/sockstat")
	if err != nil {
		return 0, false
	}
	for _, ln := range strings.Split(string(b), "\n") {
		f := strings.Fields(ln)
		if len(f) == 0 || f[0] != prefix {
			continue
		}
		for i := 1; i+1 < len(f); i += 2 {
			if f[i] == field {
				v, err := strconv.ParseFloat(f[i+1], 64)
				return v, err == nil
			}
		}
	}
	return 0, false
}

// ── S：socket ──────────────────────────────────────────────────────────

func checkSocketCategory(ctx context.Context) Category {
	cat := Category{Name: "Socket"}
	cs := make([]Check, 0, 2)

	// /proc/net/sockstat 形如：TCP: inuse 5 orphan 0 tw 0 alloc 8 mem 1
	tw, okTW := sockstat("TCP:", "tw")
	maxTW, okMax := readInt(procRoot + "/sys/net/ipv4/tcp_max_tw_buckets")
	if okTW && okMax && maxTW > 0 {
		r := pct(tw, float64(maxTW))
		cs = append(cs, judge("S01", "TIME_WAIT 水位", r, 70, 90,
			fmt.Sprintf("%.0f / %d = %.1f%%", tw, maxTW, r)))
	} else {
		cs = append(cs, skip("S01", "TIME_WAIT 水位", "读不到 sockstat.tw 或 tcp_max_tw_buckets"))
	}

	orph, okO := sockstat("TCP:", "orphan")
	maxO, okMO := readInt(procRoot + "/sys/net/ipv4/tcp_max_orphans")
	if okO && okMO && maxO > 0 {
		r := pct(orph, float64(maxO))
		cs = append(cs, judge("S02", "孤儿 socket 水位", r, 50, 80,
			fmt.Sprintf("%.0f / %d = %.1f%%", orph, maxO, r)))
	} else {
		cs = append(cs, skip("S02", "孤儿 socket 水位", "读不到 sockstat.orphan 或 tcp_max_orphans"))
	}

	cat.Checks = cs
	updateCategoryLevel(&cat)
	return cat
}

// ── E：错误计数 ────────────────────────────────────────────────────────

func checkErrorsCategory(ctx context.Context) Category {
	cat := Category{Name: "Errors"}
	cs := make([]Check, 0, 2)

	// E01 内核 taint：非零说明内核被污染（外部模块、曾经 oops 等）
	if t, okk := readInt(procRoot + "/sys/kernel/tainted"); okk {
		lvl, msg := 0, "0（干净）"
		if t != 0 {
			lvl = 1
			var why []string
			for bit, name := range map[uint]string{
				0: "非 GPL 模块", 4: "曾经 oops/BUG", 7: "曾经 machine check",
				9: "曾经 warn", 12: "外部编译模块", 13: "未签名模块",
			} {
				if t&(1<<bit) != 0 {
					why = append(why, name)
				}
			}
			msg = fmt.Sprintf("tainted=%d", t)
			if len(why) > 0 {
				msg += "（" + strings.Join(why, "、") + "）"
			}
		}
		cs = append(cs, Check{ID: "E01", Name: "内核 taint", Level: lvl, Message: msg})
	} else {
		cs = append(cs, skip("E01", "内核 taint", "读不到 /proc/sys/kernel/tainted"))
	}

	// E02 PID 水位：fork 失败前的最后一道可观测指标
	maxPID, okM := readInt(procRoot + "/sys/kernel/pid_max")
	cur := 0.0
	if ents, err := os.ReadDir(procRoot); err == nil {
		for _, e := range ents {
			if c := e.Name()[0]; c >= '0' && c <= '9' {
				cur++
			}
		}
	}
	if okM && maxPID > 0 && cur > 0 {
		r := pct(cur, float64(maxPID))
		cs = append(cs, judge("E02", "PID 水位", r, 70, 90,
			fmt.Sprintf("%.0f 个进程 / pid_max %d = %.2f%%", cur, maxPID, r)))
	} else {
		cs = append(cs, skip("E02", "PID 水位", "读不到 pid_max 或进程列表"))
	}

	cat.Checks = cs
	updateCategoryLevel(&cat)
	return cat
}

// ── 调度与输出 ─────────────────────────────────────────────────────────

func updateCategoryLevel(cat *Category) {
	cat.Total = len(cat.Checks)
	cat.Passed = 0
	cat.Level = 0
	for _, c := range cat.Checks {
		if c.Level == 0 {
			cat.Passed++
		}
		if c.Level > cat.Level {
			cat.Level = c.Level
		}
	}
}

// Run 并发跑完 9 类判定；类的顺序固定，页面不会每次刷新都跳位。
func Run(timeoutStr string) (FullResult, int) {
	timeout, _ := time.ParseDuration(timeoutStr)
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	fns := []func(context.Context) Category{
		checkTimeCategory, checkCPUCategory, checkMemoryCategory,
		checkDiskIOCategory, checkNetworkCategory, checkFilesystemCategory,
		checkConntrackCategory, checkSocketCategory, checkErrorsCategory,
	}
	names := []string{"Time/Sync", "CPU", "Memory", "Disk/IO", "Network",
		"Filesystem", "Conntrack", "Socket", "Errors"}

	out := make([]Category, len(fns))
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Add(1)
		go func(i int, fn func(context.Context) Category) {
			defer wg.Done()
			out[i] = fn(ctx)
		}(i, fn)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}

	res := FullResult{Timestamp: time.Now()}
	for i := range out {
		if out[i].Total == 0 { // 超时没跑完的类，如实标出来
			out[i] = Category{Name: names[i], Level: 1, Total: 1, Passed: 0,
				Checks: []Check{{ID: names[i], Name: names[i], Level: 1, Message: "判定超时未完成"}}}
		}
		res.Categories = append(res.Categories, out[i])
		if out[i].Level > res.ExitCode {
			res.ExitCode = out[i].Level
		}
	}
	return res, res.ExitCode
}

func PrintResults(result FullResult) {
	fmt.Printf("nodedata L0 绝对判定 — %s\n", result.Timestamp.Format("2006-01-02 15:04:05"))
	fmt.Println(strings.Repeat("=", 78))
	for _, cat := range result.Categories {
		sym, st := "✓", "OK"
		switch cat.Level {
		case 1:
			sym, st = "!", "WARN"
		case 2:
			sym, st = "x", "FAIL"
		}
		fmt.Printf("[%s] %-12s %-5s (%d/%d)\n", sym, cat.Name, st, cat.Passed, cat.Total)
		for _, c := range cat.Checks {
			cs := " "
			if c.Level == 1 {
				cs = "!"
			} else if c.Level >= 2 {
				cs = "x"
			}
			fmt.Printf("    %s %-4s %-18s %s\n", cs, c.ID, c.Name, c.Message)
		}
	}
	fmt.Println(strings.Repeat("=", 78))
	switch result.ExitCode {
	case 0:
		fmt.Println("✓ 全部通过")
	case 1:
		fmt.Println("! 有告警")
	default:
		fmt.Println("x 有失败")
	}
}
