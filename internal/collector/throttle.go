// throttle.go — cgroup CPU 配额限流的识别。
//
// 为什么必须有这个：实机注入验证时，把一个进程关进 0.2 核的 cgroup，
// nodedata 报的是"CPU 劣化 — 责任方 stress-ng（PID 402）"。
// **但这个进程是受害者，不是元凶** —— 真正的原因是配额，而 cpu.stat 显示它
// 一分钟内被限流 1607 次、累计 128 秒。照着那条结论去 kill 进程，方向完全反了。
//
// 只对快照里的那几十个进程读 cpu.stat（每轮几十次 read），不对全部 PID 读。
package collector

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// throttleStat 是一个 cgroup 的限流计数（累计值）。
type throttleStat struct {
	periods   uint64
	throttled uint64
	nsec      uint64 // 累计被限流的纳秒数
}

// cpuCgroupPath 从 /proc/PID/cgroup 找出该进程的 cpu 子系统路径。
// 同时兼容 v1（形如 "4:cpu,cpuacct:/foo"）与 v2（"0::/foo"）。
func cpuCgroupPath(procRoot string, pid int) (path string, v2 bool) {
	b, err := os.ReadFile(procRoot + "/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.SplitN(line, ":", 3)
		if len(f) != 3 {
			continue
		}
		if f[0] == "0" && f[1] == "" { // v2 统一层级
			return f[2], true
		}
		for _, c := range strings.Split(f[1], ",") {
			if c == "cpu" {
				return f[2], false
			}
		}
	}
	return "", false
}

// readThrottle 读某 cgroup 的 cpu.stat。v1 的单位是纳秒，v2 是微秒。
func readThrottle(sysRoot, path string, v2 bool) (throttleStat, bool) {
	var st throttleStat
	p := sysRoot + "/fs/cgroup/cpu" + path + "/cpu.stat"
	if v2 {
		p = sysRoot + "/fs/cgroup" + path + "/cpu.stat"
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return st, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		if err != nil {
			continue
		}
		switch k {
		case "nr_periods":
			st.periods = n
		case "nr_throttled":
			st.throttled = n
		case "throttled_time": // v1：纳秒
			st.nsec = n
		case "throttled_usec": // v2：微秒
			st.nsec = n * 1000
		}
	}
	return st, st.periods > 0 || st.throttled > 0
}

// enrichThrottle 给快照里的进程补上限流信息（只算本轮增量）。
func (c *Collector) enrichThrottle(top []ProcTop, now time.Time, dt float64) {
	if len(top) == 0 || dt <= 0 {
		return
	}
	if c.throttlePrev == nil {
		c.throttlePrev = map[string]throttleStat{}
	}
	seen := make(map[string]bool, len(top))
	for i := range top {
		path, v2 := cpuCgroupPath(c.cfg.ProcRoot, top[i].PID)
		if path == "" || path == "/" {
			continue // 不在受限的 cgroup 里
		}
		cur, ok := readThrottle(c.cfg.SysRoot, path, v2)
		if !ok {
			continue
		}
		seen[path] = true
		prev, had := c.throttlePrev[path]
		c.throttlePrev[path] = cur
		if !had {
			continue // 首轮只记基线
		}
		dPeriods := udiff(cur.periods, prev.periods)
		dThrottled := udiff(cur.throttled, prev.throttled)
		top[i].ThrottledPerS = dThrottled / dt
		top[i].ThrottledFrac = 0
		if dPeriods > 0 {
			top[i].ThrottledFrac = dThrottled / dPeriods
		}
		// 被限流掉的时间占这段墙钟的比例：0.5 表示有一半时间在等配额
		top[i].ThrottledRatio = udiff(cur.nsec, prev.nsec) / 1e9 / dt
		top[i].CGroup = path
	}
	for p := range c.throttlePrev {
		if !seen[p] {
			delete(c.throttlePrev, p)
		}
	}
}
