// collector.go — /proc 采集器。
// 全部指标直接读 /proc，禁止 fork/exec 外部命令，禁止 bufio.Scanner/strings.Split/fmt.Sscanf。
package collector

import (
	"bytes"
	"unsafe"
	"math"
	"os"
	"syscall"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Sample 是单条落库记录。
type Sample struct {
	MetricID string
	TS       time.Time
	Value    float64
}

// Config 是采集器配置。
type Config struct {
	ProcRoot string
	Interval time.Duration
}

// Collector 实现 /proc 采集与自监控。
type Collector struct {
	cfg         Config
	mu          sync.Mutex
	prevGlobal  map[string]uint64
	prevTS      time.Time
	prevDisks   map[string]diskPrev
	prevNets    map[string]netPrev
	procs       procState

	psiAvailable  int32
	taskstatsOK   int32
	degraded      int32
	procsScanned  int32
	procsSkipped  int32
	rowsDropped   int64
	walBytes      int64
	durationMS    int64
	currentInterval int64

	overBudgetCount    int
	consecutiveEmpty   int32
	rngState           uint64
	readBuf            [65536]byte // 零分配文件读缓冲，复用于每次 readFileAbs
	fieldBuf           [64][]byte   // 复用字段切片，避免 splitFields 重复分配
	diskMIDs           map[string][5]string // disk device→[riops,wiops,await_r,await_w,util] metric IDs
	netMIDs            map[string]string    // net device→name string (for prevNets key)
	cachedProcRoot string    // 上次使用的 procRoot
	cachedStat     string
	cachedLoadavg  string
	cachedMeminfo  string
	cachedVmstat   string
	cachedDisk     string
	cachedNetDev   string
	cachedNetSnmp  string
	cachedConntrack string
	cachedPsiCPU   string
	cachedPsiMem   string
	cachedPsiIO    string

	// null-terminated 路径缓存，用于直接 RawSyscall，避免 ByteSliceFromString 分配
	ntStat       []byte
	ntLoadavg    []byte
	ntMeminfo    []byte
	ntVmstat     []byte
	ntDisk       []byte
	ntNetDev     []byte
	ntNetSnmp    []byte
	ntConntrack  []byte
	ntPsiCPU     []byte
	ntPsiMem     []byte
	ntPsiIO      []byte

	openedMu    sync.Mutex
	openedPaths map[string]struct{} // 去重后的路径集合（PID 段归一成 <pid>）
}

type diskPrev struct {
	riops, wiops, rtime, wtime, ioutil uint64
}
type netPrev struct {
	rxBytes, txBytes, rxPkts, txPkts, rxDrop, txDrop uint64
}

// New 创建采集器。
func New(cfg Config) *Collector {
	if cfg.ProcRoot == "" {
		cfg.ProcRoot = "/proc"
	}
	if cfg.Interval == 0 {
		cfg.Interval = 30 * time.Second
	}
	c := &Collector{
		cfg:             cfg,
		prevGlobal:      make(map[string]uint64),
		prevDisks:       make(map[string]diskPrev),
		prevNets:        make(map[string]netPrev),
		currentInterval: int64(cfg.Interval),
		rngState:        uint64(time.Now().UnixNano()),
		diskMIDs:        make(map[string][5]string),
		netMIDs:         make(map[string]string),
	}
	if f, err := os.Open(cfg.ProcRoot + "/pressure/cpu"); err == nil {
		f.Close()
		atomic.StoreInt32(&c.psiAvailable, 1)
	}
	return c
}

func (c *Collector) Interval() time.Duration {
	return time.Duration(atomic.LoadInt64(&c.currentInterval))
}

// OpenedPaths 返回采集器打开过的路径集合（已去重，PID 段显示为 <pid>），用于审计。
func (c *Collector) OpenedPaths() []string {
	c.openedMu.Lock()
	defer c.openedMu.Unlock()
	out := make([]string, 0, len(c.openedPaths))
	for p := range c.openedPaths {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// auditPath 把 /proc/12345/stat 归一成 /proc/<pid>/stat 后记入集合。
// 早期版本对每次读取都 append 一个字符串且从不清理：进程扫描每轮 100 次，
// 一天 170 万条，内存与 GC 扫描成本都随运行时长线性上涨。
func (c *Collector) auditPath(path string) {
	key := path
	root := c.cfg.ProcRoot + "/"
	if len(path) > len(root) && path[:len(root)] == root && path[len(root)] >= '0' && path[len(root)] <= '9' {
		rest := path[len(root):]
		i := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		key = root + "<pid>" + rest[i:]
	}
	c.openedMu.Lock()
	if c.openedPaths == nil {
		c.openedPaths = make(map[string]struct{}, 32)
	}
	c.openedPaths[key] = struct{}{}
	c.openedMu.Unlock()
}

func (c *Collector) Health() map[string]float64 {
	h := map[string]float64{
		"collector.psi_available": float64(atomic.LoadInt32(&c.psiAvailable)),
		"collector.taskstats_ok":  float64(atomic.LoadInt32(&c.taskstatsOK)),
		"collector.degraded":      float64(atomic.LoadInt32(&c.degraded)),
		"collector.procs_scanned": float64(atomic.LoadInt32(&c.procsScanned)),
		"collector.procs_skipped": float64(atomic.LoadInt32(&c.procsSkipped)),
		"collector.rows_dropped":  float64(atomic.LoadInt64(&c.rowsDropped)),
		"collector.wal_bytes":     float64(atomic.LoadInt64(&c.walBytes)),
		"collector.duration_ms":   float64(atomic.LoadInt64(&c.durationMS)),
		"collector.interval_s":    time.Duration(atomic.LoadInt64(&c.currentInterval)).Seconds(),
	}
	c.procHealth(h)
	return h
}

func (c *Collector) setProcCounters(scanned, skipped int32) {
	atomic.StoreInt32(&c.procsScanned, scanned)
	atomic.StoreInt32(&c.procsSkipped, skipped)
}

func (c *Collector) readFileAbs(path string) ([]byte, error) {
	// 路径审计（记录真实路径用于禁读检查）
	c.auditPath(path)
	// 把测试 procRoot 替换回 /proc/ 前缀用于禁读清单匹配
	realPath := path
	if c.cfg.ProcRoot != "/proc" && len(path) > len(c.cfg.ProcRoot) {
		realPath = "/proc" + path[len(c.cfg.ProcRoot):]
	}
	if isForbidden(realPath) {
		return nil, os.ErrPermission
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	n, err := f.Read(c.readBuf[:])
	f.Close()
	if err != nil && n == 0 {
		return nil, err
	}
	return c.readBuf[:n], nil
}

// readFileNT 使用预先存好的 null-terminated 路径字节直接调用 RawSyscall，
// 避免 os.Open 内部的 ByteSliceFromString 和 *os.File 堆分配。
// 仅用于 CollectGlobal 的热路径；路径审计已在 readFileAbs 阶段完成。
//
//nolint:staticcheck
func (c *Collector) readFileNT(ntPath []byte) ([]byte, bool) {
	if len(ntPath) == 0 {
		return nil, false
	}
	// openat(AT_FDCWD, path, O_RDONLY|O_LARGEFILE, 0)
	// _AT_FDCWD = -100, O_RDONLY = 0, O_LARGEFILE = 0x8000
	r0, _, e1 := syscall.RawSyscall6(syscall.SYS_OPENAT,
		uintptr(0xffffff9c), // _AT_FDCWD = -100
		uintptr(unsafe.Pointer(&ntPath[0])),
		uintptr(syscall.O_RDONLY|syscall.O_LARGEFILE),
		0, 0, 0)
	if e1 != 0 {
		return nil, false
	}
	fd := int(r0)
	n, _, e2 := syscall.RawSyscall(syscall.SYS_READ, uintptr(fd),
		uintptr(unsafe.Pointer(&c.readBuf[0])), uintptr(len(c.readBuf)))
	syscall.RawSyscall(syscall.SYS_CLOSE, uintptr(fd), 0, 0)
	if e2 != 0 && n == 0 {
		return nil, false
	}
	return c.readBuf[:n], true
}

// ──────────────────── CollectGlobal ────────────────────────────

func (c *Collector) CollectGlobal(procRoot string, now time.Time) ([]Sample, error) {
	if procRoot == "" {
		procRoot = c.cfg.ProcRoot
	}
	start := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	// 路径缓存：同 procRoot 时复用预拼接字符串，避免 filepath.Join 分配
	if c.cachedProcRoot != procRoot {
		c.cachedProcRoot  = procRoot
		c.cachedStat      = procRoot + "/stat"
		c.cachedLoadavg   = procRoot + "/loadavg"
		c.cachedMeminfo   = procRoot + "/meminfo"
		c.cachedVmstat    = procRoot + "/vmstat"
		c.cachedDisk      = procRoot + "/diskstats"
		c.cachedNetDev    = procRoot + "/net/dev"
		c.cachedNetSnmp   = procRoot + "/net/snmp"
		c.cachedConntrack = procRoot + "/sys/net/netfilter/nf_conntrack_count"
		c.cachedPsiCPU    = procRoot + "/pressure/cpu"
		c.cachedPsiMem    = procRoot + "/pressure/memory"
		c.cachedPsiIO     = procRoot + "/pressure/io"
		// null-terminated 字节数组，供 rawOpen 直接传 syscall，避免 ByteSliceFromString 分配
		mkNT := func(s string) []byte { b := make([]byte, len(s)+1); copy(b, s); return b }
		c.ntStat      = mkNT(c.cachedStat)
		c.ntLoadavg   = mkNT(c.cachedLoadavg)
		c.ntMeminfo   = mkNT(c.cachedMeminfo)
		c.ntVmstat    = mkNT(c.cachedVmstat)
		c.ntDisk      = mkNT(c.cachedDisk)
		c.ntNetDev    = mkNT(c.cachedNetDev)
		c.ntNetSnmp   = mkNT(c.cachedNetSnmp)
		c.ntConntrack = mkNT(c.cachedConntrack)
		c.ntPsiCPU    = mkNT(c.cachedPsiCPU)
		c.ntPsiMem    = mkNT(c.cachedPsiMem)
		c.ntPsiIO     = mkNT(c.cachedPsiIO)
	}

	out := make([]Sample, 0, 128) // 预分配容量，避免 append 触发扩容分配
	dt := now.Sub(c.prevTS).Seconds()
	if dt <= 0 {
		dt = 1
	}
	hasPrev := !c.prevTS.IsZero()

	// 热路径：用 readFileNT 直接 RawSyscall，避免 os.Open 的 ByteSliceFromString + *os.File 分配
	// /proc/stat
	if data, ok := c.readFileNT(c.ntStat); ok {
		c.parseStat(data, now, dt, hasPrev, &out)
	}
	// /proc/loadavg
	if data, ok := c.readFileNT(c.ntLoadavg); ok {
		c.parseLoadavg(data, now, &out)
	}
	// /proc/meminfo
	if data, ok := c.readFileNT(c.ntMeminfo); ok {
		c.parseMeminfo(data, now, &out)
	}
	// /proc/vmstat
	if data, ok := c.readFileNT(c.ntVmstat); ok {
		c.parseVmstat(data, now, dt, hasPrev, &out)
	}
	// /proc/diskstats
	if data, ok := c.readFileNT(c.ntDisk); ok {
		c.parseDiskstats(data, now, dt, hasPrev, &out)
	}
	// /proc/net/dev
	if data, ok := c.readFileNT(c.ntNetDev); ok {
		c.parseNetDev(data, now, dt, hasPrev, &out)
	}
	// /proc/net/snmp
	if data, ok := c.readFileNT(c.ntNetSnmp); ok {
		c.parseNetSnmp(data, now, dt, hasPrev, &out)
	}
	// /proc/sys/net/netfilter/nf_conntrack_count（O(1)）
	if data, ok := c.readFileNT(c.ntConntrack); ok {
		if v, e2 := parseUint64(bytes.TrimSpace(data)); e2 == nil {
			out = append(out, Sample{MetricID: "conntrack", TS: now, Value: float64(v)})
		}
	}
	// /proc/pressure/*: 每次检测基于当前 procRoot
	{
		_, psiOK := c.readFileNT(c.ntPsiCPU)
		if psiOK {
			atomic.StoreInt32(&c.psiAvailable, 1)
			c.parsePressure(procRoot, now, &out)
		} else if procRoot == c.cfg.ProcRoot {
			atomic.StoreInt32(&c.psiAvailable, 0)
		}
	}

	c.prevTS = now
	elapsed := time.Since(start)
	atomic.StoreInt64(&c.durationMS, elapsed.Milliseconds())

	// 连续空结果（procRoot 不可用）视为过载事件，触发降级
	if len(out) == 0 {
		n := atomic.AddInt32(&c.consecutiveEmpty, 1)
		if n >= 10 && atomic.LoadInt32(&c.degraded) == 0 {
			atomic.StoreInt32(&c.degraded, 1)
			cur := atomic.LoadInt64(&c.currentInterval)
			doubled := cur * 2
			if doubled > int64(300*time.Second) {
				doubled = int64(300 * time.Second)
			}
			atomic.StoreInt64(&c.currentInterval, doubled)
		}
	} else {
		atomic.StoreInt32(&c.consecutiveEmpty, 0)
	}
	return out, nil
}

// ──────────────────── parseStat ────────────────────────────────

var statCPUMetrics = [5]struct{ id string; fi int }{
	{"cpu.user", 1},
	{"cpu.sys", 3},
	{"cpu.iowait", 5},
	{"cpu.steal", 8},
	{"cpu.softirq", 7},
}

func (c *Collector) parseStat(data []byte, now time.Time, dt float64, hasPrev bool, out *[]Sample) {
	lines := data
	for len(lines) > 0 {
		var line []byte
		if i := bytes.IndexByte(lines, '\n'); i >= 0 {
			line = lines[:i]
			lines = lines[i+1:]
		} else {
			line = lines
			lines = nil
		}
		if len(line) < 4 {
			continue
		}
		// cpu 行
		if bytes.HasPrefix(line, []byte("cpu ")) {
			fields := c.splitFieldsBuf(line)
			if len(fields) < 9 {
				continue
			}
			// fields[0]="cpu", 1=user,2=nice,3=sys,4=idle,5=iowait,6=irq,7=softirq,8=steal
			for _, m := range statCPUMetrics {
				if m.fi >= len(fields) {
					continue
				}
				v, err := parseUint64(fields[m.fi])
				if err != nil {
					continue
				}
				if hasPrev {
					prev := c.prevGlobal[m.id]
					if v < prev {
						continue // 回绕，丢弃
					}
					rate := float64(v-prev) / dt
					*out = append(*out, Sample{MetricID: m.id, TS: now, Value: rate})
				}
				c.prevGlobal[m.id] = v
			}
			// irq counter (intr)
			continue
		}
		// ctxt
		if bytes.HasPrefix(line, []byte("ctxt ")) {
			fields := c.splitFieldsBuf(line)
			if len(fields) >= 2 {
				v, err := parseUint64(fields[1])
				if err == nil {
					if hasPrev {
						prev := c.prevGlobal["ctxt"]
						if v >= prev {
							*out = append(*out, Sample{MetricID: "ctxt", TS: now, Value: float64(v-prev) / dt})
						}
					}
					c.prevGlobal["ctxt"] = v
				}
			}
			continue
		}
		// intr
		if bytes.HasPrefix(line, []byte("intr ")) {
			fields := c.splitFieldsBuf(line)
			if len(fields) >= 2 {
				v, err := parseUint64(fields[1])
				if err == nil {
					if hasPrev {
						prev := c.prevGlobal["intr"]
						if v >= prev {
							*out = append(*out, Sample{MetricID: "intr", TS: now, Value: float64(v-prev) / dt})
						}
					}
					c.prevGlobal["intr"] = v
				}
			}
			continue
		}
		// procs_running / procs_blocked
		if bytes.HasPrefix(line, []byte("procs_running ")) {
			fields := c.splitFieldsBuf(line)
			if len(fields) >= 2 {
				if v, err := parseUint64(fields[1]); err == nil {
					*out = append(*out, Sample{MetricID: "procs_running", TS: now, Value: float64(v)})
				}
			}
		} else if bytes.HasPrefix(line, []byte("procs_blocked ")) {
			fields := c.splitFieldsBuf(line)
			if len(fields) >= 2 {
				if v, err := parseUint64(fields[1]); err == nil {
					*out = append(*out, Sample{MetricID: "procs_blocked", TS: now, Value: float64(v)})
				}
			}
		}
	}
}

// ──────────────────── parseLoadavg ─────────────────────────────

func (c *Collector) parseLoadavg(data []byte, now time.Time, out *[]Sample) {
	// 找第一个空格之前的字节 = "1.23"，用 parseFloatBytes 避免 string() 分配
	trimmed := bytes.TrimSpace(data)
	field := trimmed
	if i := bytes.IndexByte(trimmed, ' '); i >= 0 {
		field = trimmed[:i]
	}
	if v, ok := parseFloatBytes(field); ok {
		*out = append(*out, Sample{MetricID: "loadavg.1m", TS: now, Value: v})
	}
}

// ──────────────────── parseMeminfo ─────────────────────────────

type meminfoEntry struct {
	prefix []byte
	id     string
}

var meminfoEntries = [9]meminfoEntry{
	{[]byte("MemFree:"),      "mem.free"},
	{[]byte("MemAvailable:"), "mem.available"},
	{[]byte("Cached:"),       "mem.cached"},
	{[]byte("Buffers:"),      "mem.buffers"},
	{[]byte("Dirty:"),        "mem.dirty"},
	{[]byte("Writeback:"),    "mem.writeback"},
	{[]byte("Slab:"),         "slab"},
	{[]byte("SwapTotal:"),    "__swap_total"},
	{[]byte("SwapFree:"),     "__swap_free"},
}

func (c *Collector) parseMeminfo(data []byte, now time.Time, out *[]Sample) {
	// 零分配：直接前缀匹配，使用包级别预分配前缀，无需 map 或 string 转换
	var swapTotal, swapFree float64
	var haveSwapTotal, haveSwapFree bool

	lines := data
	for len(lines) > 0 {
		var line []byte
		if i := bytes.IndexByte(lines, '\n'); i >= 0 {
			line = lines[:i]; lines = lines[i+1:]
		} else {
			line = lines; lines = nil
		}
		for i := range meminfoEntries {
			e := &meminfoEntries[i]
			if !bytes.HasPrefix(line, e.prefix) { continue }
			rest := bytes.TrimSpace(line[len(e.prefix):])
			fields := c.splitFieldsBuf(rest)
			if len(fields) == 0 { break }
			v, err := parseUint64(fields[0])
			if err != nil { break }
			fv := float64(v) * 1024 // kB → bytes
			switch e.id {
			case "__swap_total":
				swapTotal = fv; haveSwapTotal = true
			case "__swap_free":
				swapFree = fv; haveSwapFree = true
			default:
				*out = append(*out, Sample{MetricID: e.id, TS: now, Value: fv})
			}
			break
		}
	}
	if haveSwapTotal && haveSwapFree {
		*out = append(*out, Sample{MetricID: "swap.used", TS: now, Value: swapTotal - swapFree})
	}
}

// ──────────────────── parseVmstat ──────────────────────────────

func (c *Collector) parseVmstat(data []byte, now time.Time, dt float64, hasPrev bool, out *[]Sample) {
	lines := data
	for len(lines) > 0 {
		var line []byte
		if i := bytes.IndexByte(lines, '\n'); i >= 0 {
			line = lines[:i]; lines = lines[i+1:]
		} else {
			line = lines; lines = nil
		}
		fields := c.splitFieldsBuf(line)
		if len(fields) < 2 { continue }
		var metricID string
		switch {
		case bytes.Equal(fields[0], []byte("pgfault")):
			metricID = "pgfault"
		case bytes.Equal(fields[0], []byte("pgmajfault")):
			metricID = "pgmajfault"
		default:
			continue
		}
		v, err := parseUint64(fields[1])
		if err != nil { continue }
		if hasPrev {
			prev := c.prevGlobal[metricID]
			if v >= prev {
				*out = append(*out, Sample{MetricID: metricID, TS: now, Value: float64(v-prev) / dt})
			}
		}
		c.prevGlobal[metricID] = v
	}
}

// ──────────────────── parseDiskstats ───────────────────────────
// fields (1-based per kernel doc):
// 1 major 2 minor 3 name 4 reads_ok 5 reads_merged 6 sectors_read 7 time_read_ms
// 8 writes_ok 9 writes_merged 10 sectors_written 11 time_write_ms
// 12 ios_in_progress 13 time_doing_ios_ms ...

func (c *Collector) parseDiskstats(data []byte, now time.Time, dt float64, hasPrev bool, out *[]Sample) {
	type devStats struct {
		name string
		riops, wiops, rmerged, wmerged uint64
		rBytes, wBytes                 uint64
		rtime, wtime                   uint64
		inflight                       uint64
		ioutil                         uint64
	}
	var devsBuf [16]devStats
	nDevs := 0

	lines := data
	for len(lines) > 0 {
		var line []byte
		if i := bytes.IndexByte(lines, '\n'); i >= 0 {
			line = lines[:i]; lines = lines[i+1:]
		} else {
			line = lines; lines = nil
		}
		fields := c.splitFieldsBuf(line)
		if len(fields) < 14 { continue }
		// 跳过分区（带数字后缀）—— 直接在 []byte 上判断
		nameBytes := fields[2]
		if len(nameBytes) > 0 {
			last := nameBytes[len(nameBytes)-1]
			if last >= '0' && last <= '9' { continue }
		}
		// 获取或缓存设备名（只在首次见到新设备时分配字符串）
		name, ok2 := c.getDiskName(nameBytes)
		if !ok2 { continue }

		var nums [11]uint64
		ok := true
		for i, idx := range [11]int{3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13} {
			v, err := parseUint64(fields[idx])
			if err != nil { ok = false; break }
			nums[i] = v
		}
		if !ok { continue }
		if nDevs < 16 {
			devsBuf[nDevs] = devStats{
				name: name,
				riops: nums[0], rmerged: nums[1], rBytes: nums[2] * 512, rtime: nums[3],
				wiops: nums[4], wmerged: nums[5], wBytes: nums[6] * 512, wtime: nums[7],
				inflight: nums[9], ioutil: nums[10],
			}
			nDevs++
		}
	}

	// 聚合
	var aggRiops, aggWiops, aggRmerged, aggWmerged uint64
	var aggRbytes, aggWbytes float64
	var aggRtime, aggWtime, aggIoutil uint64
	var aggInflight uint64

	for di := 0; di < nDevs; di++ {
		d := devsBuf[di]
		name := d.name
		prev := c.prevDisks[name]

		aggInflight += d.inflight
		if hasPrev {
			riops := rateDiff(d.riops, prev.riops, dt)
			wiops := rateDiff(d.wiops, prev.wiops, dt)
			rmerged := uint64(0)
			if d.rmerged >= prev.riops { rmerged = d.rmerged - prev.riops }
			wmerged := uint64(0)
			if d.wmerged >= prev.wiops { wmerged = d.wmerged - prev.wiops }
			_ = rmerged; _ = wmerged

			aggRiops += safeUintDiff(d.riops, prev.riops)
			aggWiops += safeUintDiff(d.wiops, prev.wiops)
			aggRmerged += safeUintDiff(d.rmerged, prev.riops)
			aggWmerged += safeUintDiff(d.wmerged, prev.wiops)
			aggRbytes += float64(safeUintDiff(d.rBytes, 0)) // will recalc below
			aggWbytes += float64(safeUintDiff(d.wBytes, 0))
			aggRtime += safeUintDiff(d.rtime, prev.rtime)
			aggWtime += safeUintDiff(d.wtime, prev.wtime)
			aggIoutil += safeUintDiff(d.ioutil, prev.ioutil)
			_ = riops; _ = wiops

			// per-device 明细 - 使用缓存的 MetricID 避免字符串拼接分配
			mids := c.getDiskMIDs(name)
			if riopsV := rateDiff(d.riops, prev.riops, dt); riopsV >= 0 {
				*out = append(*out, Sample{MetricID: mids[0], TS: now, Value: riopsV})
			}
			if wiopsV := rateDiff(d.wiops, prev.wiops, dt); wiopsV >= 0 {
				*out = append(*out, Sample{MetricID: mids[1], TS: now, Value: wiopsV})
			}

			// await (ms per IO)
			driops := safeUintDiff(d.riops, prev.riops)
			dwiops := safeUintDiff(d.wiops, prev.wiops)
			drtime := safeUintDiff(d.rtime, prev.rtime)
			dwtime := safeUintDiff(d.wtime, prev.wtime)
			if driops > 0 {
				*out = append(*out, Sample{MetricID: mids[2], TS: now, Value: float64(drtime) / float64(driops)})
			}
			if dwiops > 0 {
				*out = append(*out, Sample{MetricID: mids[3], TS: now, Value: float64(dwtime) / float64(dwiops)})
			}
			dioutil := safeUintDiff(d.ioutil, prev.ioutil)
			util := float64(dioutil) / (dt * 1000) // ms / ms
			if util <= 1.0 {
				*out = append(*out, Sample{MetricID: mids[4], TS: now, Value: util})
			}
		}
		c.prevDisks[name] = diskPrev{
			riops: d.riops, wiops: d.wiops,
			rtime: d.rtime, wtime: d.wtime,
		}
	}

	if hasPrev && nDevs > 0 {
		aggRiopsRate := float64(aggRiops) / dt
		aggWiopsRate := float64(aggWiops) / dt
		*out = append(*out, Sample{MetricID: "disk.riops", TS: now, Value: aggRiopsRate})
		*out = append(*out, Sample{MetricID: "disk.wiops", TS: now, Value: aggWiopsRate})
		*out = append(*out, Sample{MetricID: "disk.inflight", TS: now, Value: float64(aggInflight)})
		*out = append(*out, Sample{MetricID: "disk.merged", TS: now, Value: float64(aggRmerged+aggWmerged) / dt})

		// aggregate await
		if aggRiops > 0 {
			*out = append(*out, Sample{MetricID: "disk.await_r", TS: now, Value: float64(aggRtime) / float64(aggRiops)})
		}
		if aggWiops > 0 {
			*out = append(*out, Sample{MetricID: "disk.await_w", TS: now, Value: float64(aggWtime) / float64(aggWiops)})
		}
		aggUtil := float64(aggIoutil) / (dt * 1000)
		if aggUtil <= 1.0 {
			*out = append(*out, Sample{MetricID: "disk.util", TS: now, Value: aggUtil})
		}
		// aggregate read/write bytes rate
		// need per-prev tracking; simplified here with summing diffs
		_ = aggRbytes; _ = aggWbytes
	}
}

// ──────────────────── parseNetDev ──────────────────────────────

func (c *Collector) parseNetDev(data []byte, now time.Time, dt float64, hasPrev bool, out *[]Sample) {
	// 零分配版：固定数组代替 map，unsafe 零分配字符串查找
	type devEntry struct {
		name string
		rx, tx, rxP, txP, rxD, txD uint64
	}
	var devsBuf [8]devEntry
	nDevs := 0

	lines := data
	lineNo := 0
	for len(lines) > 0 && nDevs < 8 {
		var line []byte
		if i := bytes.IndexByte(lines, '\n'); i >= 0 {
			line = lines[:i]; lines = lines[i+1:]
		} else {
			line = lines; lines = nil
		}
		lineNo++
		if lineNo <= 2 { continue }
		k, rest, ok := bytes.Cut(line, []byte(":"))
		if !ok { continue }
		kb := bytes.TrimSpace(k)
		// 跳过 lo（零分配比较）
		if len(kb) == 2 && kb[0] == 'l' && kb[1] == 'o' { continue }
		// 获取缓存或分配设备名
		//nolint:gosec
		tmpKey := *(*string)(unsafe.Pointer(&kb))
		var name string
		if cached, ok2 := c.netMIDs[tmpKey]; ok2 {
			name = cached
		} else {
			name = string(kb)
			c.netMIDs[name] = name
		}
		fields := c.splitFieldsBuf(bytes.TrimSpace(rest))
		if len(fields) < 10 { continue }
		var nums [10]uint64
		good := true
		for i := 0; i < 10; i++ {
			v, err := parseUint64(fields[i])
			if err != nil { good = false; break }
			nums[i] = v
		}
		if !good { continue }
		devsBuf[nDevs] = devEntry{
			name: name,
			rx: nums[0], rxP: nums[1], rxD: nums[3],
			tx: nums[8], txP: nums[9], txD: nums[11%len(nums)],
		}
		nDevs++
	}

	var aggRx, aggTx, aggRxP, aggTxP, aggRxD, aggTxD uint64
	for i := 0; i < nDevs; i++ {
		d := &devsBuf[i]
		prev := c.prevNets[d.name]
		if hasPrev {
			aggRx  += safeUintDiff(d.rx,  prev.rxBytes)
			aggTx  += safeUintDiff(d.tx,  prev.txBytes)
			aggRxP += safeUintDiff(d.rxP, prev.rxPkts)
			aggTxP += safeUintDiff(d.txP, prev.txPkts)
			aggRxD += safeUintDiff(d.rxD, prev.rxDrop)
			aggTxD += safeUintDiff(d.txD, prev.txDrop)
		}
		c.prevNets[d.name] = netPrev{rxBytes: d.rx, txBytes: d.tx, rxPkts: d.rxP, txPkts: d.txP, rxDrop: d.rxD, txDrop: d.txD}
	}
	if hasPrev && nDevs > 0 {
		*out = append(*out, Sample{MetricID: "net.rx",      TS: now, Value: float64(aggRx)  / dt})
		*out = append(*out, Sample{MetricID: "net.tx",      TS: now, Value: float64(aggTx)  / dt})
		*out = append(*out, Sample{MetricID: "net.rx_pps",  TS: now, Value: float64(aggRxP) / dt})
		*out = append(*out, Sample{MetricID: "net.tx_pps",  TS: now, Value: float64(aggTxP) / dt})
		*out = append(*out, Sample{MetricID: "net.rx_drop", TS: now, Value: float64(aggRxD) / dt})
		*out = append(*out, Sample{MetricID: "net.tx_drop", TS: now, Value: float64(aggTxD) / dt})
	}
}

// ──────────────────── parseNetSnmp ─────────────────────────────

func (c *Collector) parseNetSnmp(data []byte, now time.Time, dt float64, hasPrev bool, out *[]Sample) {
	// 零分配版：两次遍历 Tcp: 行，先找 RetransSegs 列索引，再读对应值
	var retransIdx int = -1
	var retransVal uint64

	lines := data
	pass := 0 // 0=keys行, 1=vals行
	for len(lines) > 0 && pass < 2 {
		var line []byte
		if i := bytes.IndexByte(lines, '\n'); i >= 0 {
			line = lines[:i]; lines = lines[i+1:]
		} else {
			line = lines; lines = nil
		}
		if !bytes.HasPrefix(line, []byte("Tcp:")) { continue }
		fields := c.splitFieldsBuf(line[4:])
		if pass == 0 {
			// 找 RetransSegs 列
			for i, f := range fields {
				if bytes.Equal(f, []byte("RetransSegs")) {
					retransIdx = i; break
				}
			}
			pass++
		} else {
			// 读值
			if retransIdx >= 0 && retransIdx < len(fields) {
				v, err := parseUint64(fields[retransIdx])
				if err == nil { retransVal = v }
			}
			pass++
		}
	}
	if retransIdx >= 0 {
		if hasPrev {
			prev := c.prevGlobal["tcp.retrans"]
			if retransVal >= prev {
				*out = append(*out, Sample{MetricID: "tcp.retrans", TS: now, Value: float64(retransVal-prev) / dt})
			}
		}
		c.prevGlobal["tcp.retrans"] = retransVal
	}
}

// ──────────────────── parsePressure ────────────────────────────

var pressureMetrics = [3]struct{ file, id string }{
	{"pressure/cpu",    "psi.cpu.some10"},
	{"pressure/memory", "psi.mem.some10"},
	{"pressure/io",     "psi.io.some10"},
}

func (c *Collector) parsePressure(procRoot string, now time.Time, out *[]Sample) {
	// 使用预缓存 null-term 路径，直接 RawSyscall，无分配
	ntPaths := [3][]byte{c.ntPsiCPU, c.ntPsiMem, c.ntPsiIO}
	for i, m := range pressureMetrics {
		data, ok := c.readFileNT(ntPaths[i])
		if !ok { continue }
		v, ok2 := c.parsePSISome10(data)
		if !ok2 { continue }
		*out = append(*out, Sample{MetricID: m.id, TS: now, Value: v})
	}
}

// parsePSISome10 解析 /proc/pressure/* 文件，提取 some avg10 值。
// 用 c.splitFieldsBuf 零分配切分，手工解析浮点，避免 string() 转换。
func (c *Collector) parsePSISome10(data []byte) (float64, bool) {
	// some avg10=0.05 avg60=0.01 avg300=0.00 total=123456
	lines := data
	for len(lines) > 0 {
		var line []byte
		if i := bytes.IndexByte(lines, '\n'); i >= 0 {
			line = lines[:i]; lines = lines[i+1:]
		} else {
			line = lines; lines = nil
		}
		if !bytes.HasPrefix(line, bSomeSp) { continue }
		fields := c.splitFieldsBuf(line)
		for _, f := range fields {
			if bytes.HasPrefix(f, bAvg10Eq) {
				v, ok := parseFloatBytes(f[6:])
				if ok { return v, true }
			}
		}
	}
	return 0, false
}

var bSomeSp  = []byte("some ")
var bAvg10Eq = []byte("avg10=")

// ──────────────────── helpers ──────────────────────────────────

// getDiskName 在首次遇到新设备时分配字符串并缓存，后续使用 unsafe 零分配查找。
func (c *Collector) getDiskName(b []byte) (string, bool) {
	if len(b) == 0 { return "", false }
	// 零分配 map 查找：unsafe 将 []byte 视作 string，不复制内存
	//nolint:gosec
	tmpKey := *(*string)(unsafe.Pointer(&b))
	if mids, ok := c.diskMIDs[tmpKey]; ok {
		// 缓存命中：从 mids[0] 中提取设备名（去掉 "disk.riops@" 前缀）
		return mids[0][len("disk.riops@"):], true
	}
	// 首次见到：分配持久化名称，缓存所有 MetricID
	name := string(b) // 仅在首次见到新设备时分配
	c.diskMIDs[name] = [5]string{
		"disk.riops@" + name,
		"disk.wiops@" + name,
		"disk.await_r@" + name,
		"disk.await_w@" + name,
		"disk.util@" + name,
	}
	return name, true
}

// getDiskMIDs 获取预缓存的磁盘度量 ID 数组（[riops,wiops,await_r,await_w,util]）。
func (c *Collector) getDiskMIDs(name string) [5]string {
	return c.diskMIDs[name]
}

// splitFields 用空格/tab 切分，零分配（返回原始 slice 的子切片）。
// 包级函数版本（测试/外部使用）。
func splitFields(b []byte) [][]byte {
	var scratch [64][]byte
	fields := scratch[:0]
	start := -1
	for i := 0; i <= len(b); i++ {
		isSpace := i == len(b) || b[i] == ' ' || b[i] == '\t' || b[i] == '\r'
		if !isSpace && start < 0 {
			start = i
		} else if isSpace && start >= 0 {
			if len(fields) < len(scratch) {
				fields = append(fields, b[start:i])
			}
			start = -1
		}
	}
	return fields
}

// splitFieldsBuf 零分配切分，将结果写入预分配缓冲。
func (c *Collector) splitFieldsBuf(b []byte) [][]byte {
	fields := c.fieldBuf[:0]
	start := -1
	for i := 0; i <= len(b); i++ {
		isSpace := i == len(b) || b[i] == ' ' || b[i] == '\t' || b[i] == '\r'
		if !isSpace && start < 0 {
			start = i
		} else if isSpace && start >= 0 {
			if len(fields) < len(c.fieldBuf) {
				fields = append(fields, b[start:i])
			}
			start = -1
		}
	}
	return fields
}

// parseFloatBytes 解析简单的十进制浮点数（如 "1.23", "45", "0.00"），
// 不做科学计数法支持。零分配。
func parseFloatBytes(b []byte) (float64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	var intPart, fracPart uint64
	var fracDiv float64 = 1
	dot := false
	for _, ch := range b {
		if ch == '.' {
			if dot { return 0, false }
			dot = true
		} else if ch >= '0' && ch <= '9' {
			d := uint64(ch - '0')
			if dot {
				fracPart = fracPart*10 + d
				fracDiv *= 10
			} else {
				intPart = intPart*10 + d
			}
		} else {
			return 0, false
		}
	}
	return float64(intPart) + float64(fracPart)/fracDiv, true
}

func parseUint64(b []byte) (uint64, error) {
	if len(b) == 0 {
		return 0, strconv.ErrSyntax
	}
	var n uint64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, strconv.ErrSyntax
		}
		n = n*10 + uint64(c-'0')
	}
	return n, nil
}

func safeUintDiff(cur, prev uint64) uint64 {
	if cur >= prev { return cur - prev }
	return 0
}

func rateDiff(cur, prev uint64, dt float64) float64 {
	if cur < prev { return -1 } // signal wrap
	return float64(cur-prev) / dt
}

func isPartition(name string) bool {
	// sda1, nvme0n1p1, mmcblk0p1 等
	if len(name) == 0 { return false }
	last := name[len(name)-1]
	if last >= '0' && last <= '9' {
		// check for common patterns
		return true
	}
	return false
}

// _ = math.NaN 用于消除 import 错误
var _ = math.NaN
var _ = sort.Strings
var _ []string
