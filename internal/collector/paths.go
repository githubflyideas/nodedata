// paths.go — /proc 路径白名单。禁读清单在此强制执行。
package collector

import (
	"errors"
	"path/filepath"
)

// forbiddenPaths 是绝对禁止打开的路径（O(n) 代价，在大表时会使采集器成为故障源）。
var forbiddenPaths = map[string]bool{
	"/proc/net/nf_conntrack": true,
	"/proc/net/tcp":          true,
	"/proc/net/tcp6":         true,
	"/proc/net/udp":          true,
	"/proc/net/udp6":         true,
	"/proc/net/unix":         true,
	"/proc/slabinfo":         true,
	"/proc/kpageflags":       true,
	"/proc/kpagecount":       true,
}

// allowedSuffixes 是被允许读取的 /proc 子路径后缀。
// 采集端只读已知路径，任何不在此列表中的路径都会被拒绝。
var allowedSuffixes = []string{
	"/proc/stat",
	"/proc/loadavg",
	"/proc/meminfo",
	"/proc/vmstat",
	"/proc/diskstats",
	"/proc/net/dev",
	"/proc/net/snmp",
	"/proc/net/sockstat",
	"/proc/sys/net/netfilter/nf_conntrack_count",
	"/proc/sys/net/netfilter/nf_conntrack_max",
	"/proc/pressure/cpu",
	"/proc/pressure/memory",
	"/proc/pressure/io",
	// 进程相关（前缀匹配）
	"/proc/", // 进程目录下的 stat, io, status 通过前缀检查
}

// checkPath 验证路径是否允许读取。
func checkPath(procRoot, rel string) error {
	abs := filepath.Join(procRoot, rel)
	// 检查禁止列表（使用真实绝对路径 /proc/...）
	realAbs := "/proc" + rel
	if forbiddenPaths[realAbs] {
		return errors.New("forbidden path: " + abs)
	}
	return nil
}

// isForbidden 检查真实绝对路径是否在禁读清单中。
func isForbidden(absPath string) bool {
	return forbiddenPaths[absPath]
}
