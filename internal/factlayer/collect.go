package factlayer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────────
// Time helpers
// ──────────────────────────────────────────────────────────────────────────────

var cachedBootID string

func bootID() string {
	if cachedBootID != "" {
		return cachedBootID
	}
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "unknown"
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 8 {
		s = s[:8]
	}
	cachedBootID = s
	return s
}

func monoNow() float64 {
	var ts syscall.Timespec
	if err := syscall.ClockGettime(syscall.CLOCK_MONOTONIC, &ts); err != nil {
		return 0
	}
	return float64(ts.Sec) + float64(ts.Nsec)/1e9
}

func wallNow() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

// newFact builds a valid fact with all time fields filled in.
func newFact(path string, v any, src string) Fact {
	return Fact{
		T:    wallNow(),
		Mono: monoNow(),
		Boot: bootID(),
		Path: path,
		V:    v,
		Src:  src,
	}
}

// missingFact builds a fact representing "could not read this path".
func missingFact(path, src, reason string) Fact {
	return Fact{
		T:    wallNow(),
		Mono: monoNow(),
		Boot: bootID(),
		Path: path,
		V:    nil,
		Err:  reason,
		Src:  src,
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Low-level readers — all return (value, ok); on !ok they return a missingFact.
// ──────────────────────────────────────────────────────────────────────────────

func readFileTrim(filePath string) (string, error) {
	b, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func readFirstField(filePath string) (string, error) {
	s, err := readFileTrim(filePath)
	if err != nil {
		return "", err
	}
	f := strings.Fields(s)
	if len(f) == 0 {
		return "", fmt.Errorf("empty file")
	}
	return f[0], nil
}

// collectFile reads a single sysfs/procfs file into a Fact.
// The value is stored as a string (trimmed). Numeric parsing is left to L1+.
func collectFile(factPath, filePath string) Fact {
	s, err := readFileTrim(filePath)
	if err != nil {
		return missingFact(factPath, filePath, errReason(err))
	}
	return newFact(factPath, s, filePath)
}

// collectIntFile reads a file that contains a single integer.
func collectIntFile(factPath, filePath string) Fact {
	s, err := readFirstField(filePath)
	if err != nil {
		return missingFact(factPath, filePath, errReason(err))
	}
	var v int64
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		return missingFact(factPath, filePath, "parse int: "+err.Error())
	}
	return newFact(factPath, v, filePath)
}

func errReason(err error) string {
	if os.IsPermission(err) {
		return "EACCES"
	}
	if os.IsNotExist(err) {
		return "ENOENT"
	}
	return err.Error()
}

// ──────────────────────────────────────────────────────────────────────────────
// Collector: collects all P0 facts defined in the design doc.
// ──────────────────────────────────────────────────────────────────────────────

// Config allows overriding /proc and /sys roots for testing.
type Config struct {
	ProcRoot string // default "/proc"
	SysRoot  string // default "/sys"
}

func (c Config) proc(parts ...string) string {
	root := c.ProcRoot
	if root == "" {
		root = "/proc"
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

func (c Config) sys(parts ...string) string {
	root := c.SysRoot
	if root == "" {
		root = "/sys"
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

// Collect runs all P0 collectors and returns a StateTree.
func Collect(cfg Config) *StateTree {
	var facts []Fact
	facts = append(facts, collectKernelIdentity(cfg)...)
	facts = append(facts, collectTime(cfg)...)
	facts = append(facts, collectCPU(cfg)...)
	facts = append(facts, collectMemory(cfg)...)
	facts = append(facts, collectStorage(cfg)...)
	facts = append(facts, collectNetwork(cfg)...)
	facts = append(facts, collectCgroups(cfg)...)
	facts = append(facts, collectKmsg(cfg)...)
	facts = append(facts, collectPCIe(cfg)...)
	return newStateTree(facts, time.Now())
}

// ──────────────────────────────────────────────────────────────────────────────
// Kernel identity
// ──────────────────────────────────────────────────────────────────────────────

func collectKernelIdentity(cfg Config) []Fact {
	var out []Fact
	out = append(out, collectFile("kernel.version", cfg.proc("version")))
	out = append(out, collectFile("kernel.cmdline", cfg.proc("cmdline")))
	out = append(out, collectFile("kernel.boot_id", cfg.proc("sys/kernel/random/boot_id")))

	// uptime (seconds since boot)
	out = append(out, collectFile("kernel.uptime_raw", cfg.proc("uptime")))

	// tainted: decode the bitmap value
	out = append(out, collectIntFile("kernel.tainted", cfg.proc("sys/kernel/tainted")))

	// loaded modules: just record count and names (first 50)
	if b, err := os.ReadFile(cfg.proc("modules")); err == nil {
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		names := make([]string, 0, len(lines))
		for _, l := range lines {
			f := strings.Fields(l)
			if len(f) > 0 {
				names = append(names, f[0])
			}
		}
		out = append(out, newFact("kernel.modules.count", int64(len(names)), cfg.proc("modules")))
		preview := names
		if len(preview) > 50 {
			preview = preview[:50]
		}
		out = append(out, newFact("kernel.modules.names", strings.Join(preview, " "), cfg.proc("modules")))
	} else {
		out = append(out, missingFact("kernel.modules.count", cfg.proc("modules"), errReason(err)))
		out = append(out, missingFact("kernel.modules.names", cfg.proc("modules"), errReason(err)))
	}

	// CPU vulnerabilities
	vulnDir := cfg.sys("devices/system/cpu/vulnerabilities")
	if entries, err := os.ReadDir(vulnDir); err == nil {
		for _, e := range entries {
			fpath := filepath.Join(vulnDir, e.Name())
			factPath := "cpu.vulnerabilities." + e.Name()
			out = append(out, collectFile(factPath, fpath))
		}
	} else {
		out = append(out, missingFact("cpu.vulnerabilities", vulnDir, errReason(err)))
	}

	return out
}

// ──────────────────────────────────────────────────────────────────────────────
// Time / clock source
// ──────────────────────────────────────────────────────────────────────────────

func collectTime(cfg Config) []Fact {
	var out []Fact
	out = append(out, collectFile("time.clocksource.current",
		cfg.sys("devices/system/clocksource/clocksource0/current_clocksource")))
	out = append(out, collectFile("time.clocksource.available",
		cfg.sys("devices/system/clocksource/clocksource0/available_clocksource")))

	// Timezone
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		// Usually /usr/share/zoneinfo/Asia/Tokyo
		if i := strings.Index(link, "zoneinfo/"); i >= 0 {
			out = append(out, newFact("time.timezone", link[i+9:], "/etc/localtime"))
		} else {
			out = append(out, newFact("time.timezone", link, "/etc/localtime"))
		}
	} else {
		out = append(out, missingFact("time.timezone", "/etc/localtime", errReason(err)))
	}

	// TSC stability
	out = append(out, collectFile("time.tsc.stable",
		cfg.sys("devices/system/cpu/cpu0/tsc_freq_khz")))

	return out
}

// ──────────────────────────────────────────────────────────────────────────────
// CPU
// ──────────────────────────────────────────────────────────────────────────────

func collectCPU(cfg Config) []Fact {
	var out []Fact

	// Model, core counts
	out = append(out, collectFile("cpu.info_raw", cfg.proc("cpuinfo")))

	// microcode version (first CPU)
	if b, err := os.ReadFile(cfg.proc("cpuinfo")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "microcode") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					out = append(out, newFact("cpu.microcode", strings.TrimSpace(parts[1]), cfg.proc("cpuinfo")))
				}
				break
			}
		}
	}

	// scaling governor (cpu0)
	out = append(out, collectFile("cpu.scaling_governor",
		cfg.sys("devices/system/cpu/cpu0/cpufreq/scaling_governor")))

	// max freq
	out = append(out, collectIntFile("cpu.max_freq_khz",
		cfg.sys("devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq")))

	// isolcpus / nohz_full from cmdline (derive from kernel.cmdline)
	if b, err := os.ReadFile(cfg.proc("cmdline")); err == nil {
		cmdline := string(b)
		for _, param := range []string{"isolcpus", "nohz_full", "rcu_nocbs"} {
			val := extractCmdlineParam(cmdline, param)
			if val != "" {
				out = append(out, newFact("cpu.cmdline."+param, val, cfg.proc("cmdline")))
			} else {
				out = append(out, newFact("cpu.cmdline."+param, nil, cfg.proc("cmdline")))
			}
		}
	}

	// NUMA nodes
	numaDir := cfg.sys("devices/system/node")
	if entries, err := os.ReadDir(numaDir); err == nil {
		count := 0
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "node") && e.IsDir() {
				count++
			}
		}
		out = append(out, newFact("cpu.numa.nodes", int64(count), numaDir))
	} else {
		out = append(out, missingFact("cpu.numa.nodes", numaDir, errReason(err)))
	}

	return out
}

func extractCmdlineParam(cmdline, param string) string {
	// Look for "param=value" or just "param"
	for _, token := range strings.Fields(cmdline) {
		if token == param {
			return "1"
		}
		if strings.HasPrefix(token, param+"=") {
			return token[len(param)+1:]
		}
	}
	return ""
}

// ──────────────────────────────────────────────────────────────────────────────
// Memory
// ──────────────────────────────────────────────────────────────────────────────

func collectMemory(cfg Config) []Fact {
	var out []Fact

	// Raw meminfo — parse into individual facts
	if b, err := os.ReadFile(cfg.proc("meminfo")); err == nil {
		src := cfg.proc("meminfo")
		for _, line := range strings.Split(string(b), "\n") {
			i := strings.IndexByte(line, ':')
			if i <= 0 {
				continue
			}
			key := strings.TrimSpace(line[:i])
			rest := strings.TrimSpace(line[i+1:])
			fields := strings.Fields(rest)
			if len(fields) == 0 {
				continue
			}
			var v int64
			if _, err := fmt.Sscanf(fields[0], "%d", &v); err == nil {
				if len(fields) > 1 && fields[1] == "kB" {
					v *= 1024
				}
				out = append(out, newFact("mm."+strings.ToLower(key), v, src))
			}
		}
	} else {
		out = append(out, missingFact("mm.meminfo", cfg.proc("meminfo"), errReason(err)))
	}

	// THP
	out = append(out, collectFile("mm.thp.enabled",
		cfg.sys("kernel/mm/transparent_hugepage/enabled")))
	out = append(out, collectFile("mm.thp.defrag",
		cfg.sys("kernel/mm/transparent_hugepage/defrag")))

	// Hugepages
	out = append(out, collectIntFile("mm.hugepages.nr",
		cfg.proc("sys/vm/nr_hugepages")))

	// Key sysctl
	for _, entry := range []struct{ fact, file string }{
		{"mm.overcommit_memory", cfg.proc("sys/vm/overcommit_memory")},
		{"mm.overcommit_ratio", cfg.proc("sys/vm/overcommit_ratio")},
		{"mm.swappiness", cfg.proc("sys/vm/swappiness")},
		{"mm.min_free_kbytes", cfg.proc("sys/vm/min_free_kbytes")},
		{"mm.zone_reclaim_mode", cfg.proc("sys/vm/zone_reclaim_mode")},
		{"mm.dirty_ratio", cfg.proc("sys/vm/dirty_ratio")},
		{"mm.dirty_background_ratio", cfg.proc("sys/vm/dirty_background_ratio")},
	} {
		out = append(out, collectIntFile(entry.fact, entry.file))
	}

	return out
}

// ──────────────────────────────────────────────────────────────────────────────
// Storage
// ──────────────────────────────────────────────────────────────────────────────

func collectStorage(cfg Config) []Fact {
	var out []Fact

	blockDir := cfg.sys("block")
	entries, err := os.ReadDir(blockDir)
	if err != nil {
		out = append(out, missingFact("block", blockDir, errReason(err)))
		return out
	}

	for _, e := range entries {
		name := e.Name()
		// Skip partitions and virtual devices
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") ||
			strings.HasPrefix(name, "zram") {
			continue
		}
		devBase := "block." + name

		queueDir := cfg.sys("block", name, "queue")
		for _, qf := range []string{"scheduler", "nr_requests", "rotational",
			"read_ahead_kb", "rq_affinity", "max_sectors_kb", "write_cache"} {
			out = append(out, collectFile(devBase+".queue."+qf, filepath.Join(queueDir, qf)))
		}

		// Write cache
		out = append(out, collectFile(devBase+".queue.write_cache",
			filepath.Join(queueDir, "write_cache")))
	}

	// mountinfo (more complete than /proc/mounts)
	out = append(out, collectFile("mount.info_raw", cfg.proc("self/mountinfo")))

	return out
}

// ──────────────────────────────────────────────────────────────────────────────
// Network
// ──────────────────────────────────────────────────────────────────────────────

func collectNetwork(cfg Config) []Fact {
	var out []Fact

	netDir := cfg.sys("class/net")
	ifaces, err := os.ReadDir(netDir)
	if err != nil {
		out = append(out, missingFact("net.iface", netDir, errReason(err)))
	} else {
		for _, e := range ifaces {
			name := e.Name()
			if name == "lo" {
				continue
			}
			base := "net.iface." + name
			ifDir := filepath.Join(netDir, name)

			for _, attr := range []string{"operstate", "carrier", "carrier_changes",
				"speed", "duplex", "mtu", "address", "tx_queue_len"} {
				out = append(out, collectFile(base+"."+attr, filepath.Join(ifDir, attr)))
			}

			// driver info
			driverLink := filepath.Join(ifDir, "device/driver")
			if target, err := os.Readlink(driverLink); err == nil {
				out = append(out, newFact(base+".driver.name", filepath.Base(target), driverLink))
			} else {
				out = append(out, missingFact(base+".driver.name", driverLink, errReason(err)))
			}
		}
	}

	// Key network sysctl
	for _, entry := range []struct{ fact, file string }{
		{"net.core.somaxconn", cfg.proc("sys/net/core/somaxconn")},
		{"net.core.netdev_max_backlog", cfg.proc("sys/net/core/netdev_max_backlog")},
		{"net.ipv4.tcp_max_syn_backlog", cfg.proc("sys/net/ipv4/tcp_max_syn_backlog")},
		{"net.ipv4.tcp_congestion_control", cfg.proc("sys/net/ipv4/tcp_congestion_control")},
		{"net.ipv4.tcp_tw_reuse", cfg.proc("sys/net/ipv4/tcp_tw_reuse")},
		{"net.ipv4.ip_local_port_range", cfg.proc("sys/net/ipv4/ip_local_port_range")},
		{"net.ipv4.rp_filter", cfg.proc("sys/net/ipv4/conf/all/rp_filter")},
		{"net.nf_conntrack.max", cfg.proc("sys/net/netfilter/nf_conntrack_max")},
		{"net.nf_conntrack.count", cfg.proc("sys/net/netfilter/nf_conntrack_count")},
	} {
		out = append(out, collectFile(entry.fact, entry.file))
	}

	// /proc/net/snmp and netstat (errors/retrans — raw values, L1 computes rates)
	out = append(out, collectFile("net.snmp_raw", cfg.proc("net/snmp")))
	out = append(out, collectFile("net.netstat_raw", cfg.proc("net/netstat")))

	return out
}

// ──────────────────────────────────────────────────────────────────────────────
// Cgroups (v2)
// ──────────────────────────────────────────────────────────────────────────────

func collectCgroups(cfg Config) []Fact {
	var out []Fact

	// Walk cgroup v2 hierarchy (unified hierarchy at /sys/fs/cgroup)
	cgRoot := "/sys/fs/cgroup"
	if _, err := os.Stat(cgRoot + "/cgroup.controllers"); err != nil {
		// Not cgroup v2
		out = append(out, missingFact("cgroup.v2", cgRoot, "cgroup v2 not available"))
		return out
	}

	out = append(out, collectCgroupDir(cgRoot, "")...)

	// PSI (pressure stall information)
	for _, kind := range []string{"cpu", "memory", "io"} {
		out = append(out, collectFile("psi."+kind, cfg.proc("pressure/"+kind)))
	}

	return out
}

func collectCgroupDir(cgRoot, relPath string) []Fact {
	var out []Fact

	dir := cgRoot
	factBase := "cgroup"
	if relPath != "" {
		dir = filepath.Join(cgRoot, relPath)
		factBase = "cgroup." + strings.ReplaceAll(relPath, "/", ".")
	}

	for _, file := range []string{
		"memory.max", "memory.high", "memory.current",
		"cpu.max", "pids.max", "memory.events", "cpu.stat",
	} {
		fpath := filepath.Join(dir, file)
		if _, err := os.Stat(fpath); err == nil {
			factKey := factBase + "." + strings.ReplaceAll(file, ".", "_")
			out = append(out, collectFile(factKey, fpath))
		}
	}

	// Walk one level of subdirectories (system.slice, user.slice, etc.)
	if relPath == "" {
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				name := e.Name()
				if name == "init.scope" || strings.HasPrefix(name, ".") {
					continue
				}
				sub := filepath.Join(relPath, name)
				out = append(out, collectCgroupDir(cgRoot, sub)...)
			}
		}
	}

	return out
}

// ──────────────────────────────────────────────────────────────────────────────
// Kernel ring buffer (kmsg) — events only, not full log
// ──────────────────────────────────────────────────────────────────────────────

var kmsgPatterns = []string{
	"Out of memory",
	"oom_kill",
	"hung_task_timeout_secs",
	"Call Trace",
	"Hardware Error",
	"EDAC",
	"mce:",
	"NETDEV WATCHDOG",
	"nf_conntrack: table full",
	"blocked for more than",
	"EXT4-fs error",
	"XFS",
	"BTRFS error",
}

// collectKmsg reads /dev/kmsg to extract event lines matching known patterns.
// We use a non-blocking read with a short deadline so we don't block forever.
func collectKmsg(cfg Config) []Fact {
	var out []Fact

	f, err := os.OpenFile("/dev/kmsg", os.O_RDONLY|os.O_NONBLOCK, 0)
	if err != nil {
		out = append(out, missingFact("kmsg.events", "/dev/kmsg", errReason(err)))
		return out
	}
	defer f.Close()

	buf := make([]byte, 8192)
	var matches []string
	deadline := time.Now().Add(200 * time.Millisecond)

	for time.Now().Before(deadline) {
		n, err := f.Read(buf)
		if err != nil {
			break // EAGAIN = no more messages
		}
		line := string(buf[:n])
		for _, pat := range kmsgPatterns {
			if strings.Contains(line, pat) {
				matches = append(matches, strings.TrimSpace(line))
				break
			}
		}
		if len(matches) >= 50 { // cap at 50 matching lines
			break
		}
	}

	if len(matches) == 0 {
		out = append(out, newFact("kmsg.events.count", int64(0), "/dev/kmsg"))
	} else {
		out = append(out, newFact("kmsg.events.count", int64(len(matches)), "/dev/kmsg"))
		out = append(out, newFact("kmsg.events.last50", strings.Join(matches, "\n"), "/dev/kmsg"))
	}

	return out
}

// ──────────────────────────────────────────────────────────────────────────────
// PCIe link status — detect degraded slots
// ──────────────────────────────────────────────────────────────────────────────

func collectPCIe(cfg Config) []Fact {
	var out []Fact

	pciDir := cfg.sys("bus/pci/devices")
	entries, err := os.ReadDir(pciDir)
	if err != nil {
		out = append(out, missingFact("pcie", pciDir, errReason(err)))
		return out
	}

	for _, e := range entries {
		devDir := filepath.Join(pciDir, e.Name())
		base := "pcie." + e.Name()

		for _, attr := range []string{
			"current_link_speed", "current_link_width",
			"max_link_speed", "max_link_width",
		} {
			fpath := filepath.Join(devDir, attr)
			f := collectFile(base+"."+attr, fpath)
			if f.V == nil {
				continue // most PCI devices don't have these; skip silently
			}
			out = append(out, f)
		}
	}

	return out
}
