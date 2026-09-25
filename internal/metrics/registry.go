// Package metrics 是指标注册表：每个指标"是什么"只在这里写一次。
//
// 原来这些知识散在 10 个函数、82 处 strings.HasPrefix/Contains 里，每个函数各猜各的：
// unitOf 靠名字里有没有 "util"、"_drop" 猜单位，classOf 靠前缀猜归哪类，
// minDeltaFor、badDirection、familyOf 又各有一套。加一个指标要改将近十处，
// 漏一处就错一处——v5.16 加 swap.in 时单位被"swap. 开头 = 字节"那条规则截走，
// v5.17 加逐核 5 条序列改了 6 张表。
//
// 现在：登记过的指标全部显式写在 known 里，一行一个；进程类（proc.cpu.<名字>）
// 按前缀登记一次。按命名习惯猜的逻辑只留给**没登记**的指标兜底，而且只猜单位
// （猜错单位会把 14.49% 显示成 "14.49 B"），其余属性一律取中性默认值。
//
// 这次重构不改任何行为：testdata/golden.json 是重构前 10 个旧函数对 167 个指标 ID
// 的输出，登记过的指标必须逐字一致（registry_test.go）。
package metrics

import "strings"

// 诊断归类。值与 internal/diagnosis 里的同名常量一致（那边直接引用这里）。
const (
	ClassCPU = "CPU"
	ClassIO  = "IO"
	ClassMem = "内存"
	ClassNet = "网络"
)

// Info 是一个指标的全部静态属性。
type Info struct {
	// Unit：percent | bytes | bytes/s | ops/s | /s | ms | s | load | count
	Unit string
	// MinDelta：变化小于它就不算"值得一提"（z 再大也不报）。0 = 按单位取默认，见 defaultMinDelta。
	MinDelta float64
	// Primary：主视图指标。
	Primary bool
	// Class：诊断归类（CPU / IO / 内存 / 网络），空 = 不参与归因。
	Class string
	// LowerIsBad：降低才是坏事（可用内存、剩余空间）。默认升高是坏事。
	LowerIsBad bool
	// Family：同族指标一次只报一条（盘延迟的读和写是一件事）。
	Family string
	// EvidenceOnly：只作旁证，不单独成为一条诊断（吞吐、连接数这类"忙"不等于"坏"）。
	EvidenceOnly bool
	// Registered：是否登记过。false = 走了兜底。
	Registered bool
}

// Direction 返回"坏的方向"：+1 升高是坏事，-1 降低是坏事。
func (in Info) Direction() float64 {
	if in.LowerIsBad {
		return -1
	}
	return 1
}

// known 按基础名登记（"disk.util@sda" 的基础名是 "disk.util"，逐设备成员继承基础名的属性）。
// MinDelta 只在跟单位默认值不同时才写。
var known = map[string]Info{
	// collector
	"collector.degraded":      {Unit: "count"},
	"collector.duration_ms":   {Unit: "ms"},
	"collector.interval_s":    {Unit: "s"},
	"collector.procs_scan_ms": {Unit: "ms"},
	"collector.procs_scanned": {Unit: "count"},
	"collector.procs_skipped": {Unit: "count"},
	"collector.psi_available": {Unit: "count"},
	"collector.rows_dropped":  {Unit: "/s", MinDelta: 1},
	"collector.taskstats_ok":  {Unit: "count"},
	"collector.wal_bytes":     {Unit: "bytes"},
	// conntrack
	"conntrack":          {Unit: "count", Class: ClassNet},
	"conntrack.used_pct": {Unit: "percent"},
	// cpu
	"cpu.busy_pct": {Unit: "percent", Primary: true},
	// 逐核：最忙那个核天然抖得厉害——"N 个核里的最大值"是极值统计，闲机器上也在
	// 3% 和 30% 之间来回跳。按整机的 3 个百分点算，每次小抖动都会变成"显著偏离"。
	// 真打满是从几十涨到 100，远超 20。三条 top 归同一族：一次单核打满不该变成三条。
	"cpu.core_softirq_max": {Unit: "percent", MinDelta: 20, Primary: true},
	"cpu.core_top1":        {Unit: "percent", MinDelta: 20, Primary: true, Family: "单核"},
	"cpu.core_top2":        {Unit: "percent", MinDelta: 20, Primary: true, Family: "单核"},
	"cpu.core_top3":        {Unit: "percent", MinDelta: 20, Primary: true, Family: "单核"},
	"cpu.iowait":           {Unit: "percent", Primary: true, Class: ClassIO},
	"cpu.softirq":          {Unit: "percent", Primary: true, Class: ClassCPU},
	"cpu.steal":            {Unit: "percent", Primary: true, Class: ClassCPU},
	"cpu.sys":              {Unit: "percent", Primary: true, Class: ClassCPU},
	"cpu.user":             {Unit: "percent", Primary: true, Class: ClassCPU},
	// ctxt
	"ctxt": {Unit: "count", EvidenceOnly: true},
	// disk
	"disk.await_r":  {Unit: "ms", Primary: true, Class: ClassIO, Family: "盘延迟"},
	"disk.await_w":  {Unit: "ms", Primary: true, Class: ClassIO, Family: "盘延迟"},
	"disk.inflight": {Unit: "count", Primary: true, Class: ClassIO},
	"disk.merged":   {Unit: "count", Primary: true, Class: ClassIO},
	"disk.rbytes":   {Unit: "bytes/s", Primary: true, Class: ClassIO, EvidenceOnly: true},
	"disk.riops":    {Unit: "ops/s", Primary: true, Class: ClassIO, EvidenceOnly: true},
	"disk.util":     {Unit: "percent", Primary: true, Class: ClassIO},
	"disk.wbytes":   {Unit: "bytes/s", Primary: true, Class: ClassIO, EvidenceOnly: true},
	"disk.wiops":    {Unit: "ops/s", Primary: true, Class: ClassIO, EvidenceOnly: true},
	// fs
	"fs.avail":          {Unit: "bytes", LowerIsBad: true, Family: "根分区"},
	"fs.inode_used_pct": {Unit: "percent"},
	"fs.used_pct":       {Unit: "percent", Family: "根分区"},
	// intr
	"intr": {Unit: "count", EvidenceOnly: true},
	// loadavg
	// 负载只作旁证（v5.20）：Linux 把在等 IO 的进程（D 状态）也算进负载，
	// 单凭它领头会把磁盘卡住说成 CPU 劣化。它照常显示，只是不单独成为一条线索。
	"loadavg.1m": {Unit: "load", Primary: true, Class: ClassCPU, Family: "CPU 压力", EvidenceOnly: true},
	// mem
	"mem.available": {Unit: "bytes", Primary: true, Class: ClassMem, LowerIsBad: true, Family: "内存余量"},
	"mem.buffers":   {Unit: "bytes", Primary: true, Class: ClassMem, EvidenceOnly: true},
	"mem.cached":    {Unit: "bytes", Primary: true, Class: ClassMem, LowerIsBad: true, EvidenceOnly: true},
	"mem.dirty":     {Unit: "bytes", Primary: true, Class: ClassIO},
	"mem.free":      {Unit: "bytes", Primary: true, Class: ClassMem, LowerIsBad: true, Family: "内存余量"},
	"mem.used_pct":  {Unit: "percent", Primary: true, Class: ClassMem, Family: "内存余量"},
	"mem.writeback": {Unit: "bytes", Primary: true, Class: ClassIO},
	// net
	"net.rx": {Unit: "bytes/s", Primary: true, Class: ClassNet, Family: "网卡吞吐", EvidenceOnly: true},
	// 错误类（丢包、错误、重传、UDP 溢出）：本该是 0，出现 1 次/秒 就值得看。
	"net.rx_drop":         {Unit: "/s", MinDelta: 1, Primary: true, Class: ClassNet, Family: "丢包"},
	"net.rx_errs":         {Unit: "/s", MinDelta: 1, Primary: true, Class: ClassNet, Family: "丢包"},
	"net.rx_pps":          {Unit: "ops/s", Primary: true, Class: ClassNet, Family: "网卡吞吐", EvidenceOnly: true},
	"net.softnet_drop":    {Unit: "/s", MinDelta: 1, Primary: true, Class: ClassNet},
	"net.softnet_squeeze": {Unit: "count", Primary: true, Class: ClassNet},
	"net.tx":              {Unit: "bytes/s", Primary: true, Class: ClassNet, Family: "网卡吞吐", EvidenceOnly: true},
	"net.tx_drop":         {Unit: "/s", MinDelta: 1, Primary: true, Class: ClassNet, Family: "丢包"},
	"net.tx_errs":         {Unit: "/s", MinDelta: 1, Primary: true, Class: ClassNet, Family: "丢包"},
	"net.tx_pps":          {Unit: "ops/s", Primary: true, Class: ClassNet, Family: "网卡吞吐", EvidenceOnly: true},
	// pgfault
	"pgfault": {Unit: "/s", Class: ClassMem, EvidenceOnly: true},
	// pgmajfault
	"pgmajfault": {Unit: "/s"},
	// procs_blocked
	// 进程数：两个以内的起落是常态。
	"procs_blocked": {Unit: "count", MinDelta: 2, Primary: true, Class: ClassIO},
	// procs_running
	"procs_running": {Unit: "count", MinDelta: 2, Primary: true, Class: ClassCPU, Family: "CPU 压力"},
	// psi
	"psi.cpu.some10": {Unit: "percent", Primary: true, Class: ClassCPU, Family: "CPU 压力"},
	"psi.io.some10":  {Unit: "percent", Primary: true, Class: ClassIO},
	"psi.mem.some10": {Unit: "percent", Primary: true, Class: ClassMem},
	// self
	"self.cpu_pct": {Unit: "percent"},
	"self.rss_mb":  {Unit: "count"},
	// slab
	"slab": {Unit: "bytes", Primary: true, Class: ClassMem, EvidenceOnly: true},
	// sock
	"sock.tcp_alloc":  {Unit: "count"},
	"sock.tcp_inuse":  {Unit: "count", EvidenceOnly: true},
	"sock.tcp_orphan": {Unit: "count"},
	"sock.tcp_tw":     {Unit: "count"},
	"sock.udp_inuse":  {Unit: "count", EvidenceOnly: true},
	"sock.used":       {Unit: "count", EvidenceOnly: true},
	// softnet
	"softnet.drop":    {Unit: "count"},
	"softnet.squeeze": {Unit: "count"},
	// swap
	"swap.in":  {Unit: "bytes/s", Primary: true, Class: ClassMem},
	"swap.out": {Unit: "bytes/s", Primary: true, Class: ClassMem},
	// swap 存量：32MB 以内的变动不值一提。真正疼的是换入换出的速率（swap.in/out）。
	"swap.used": {Unit: "bytes", MinDelta: 32 << 20, Primary: true, Class: ClassMem},
	// tcp
	"tcp.attempt_fails": {Unit: "/s", MinDelta: 1, Class: ClassNet},
	"tcp.estab":         {Unit: "count", Class: ClassNet, EvidenceOnly: true},
	"tcp.passive_opens": {Unit: "/s", Class: ClassNet},
	"tcp.retrans":       {Unit: "/s", MinDelta: 1, Class: ClassNet},
	// udp
	"udp.in_errors":     {Unit: "/s", MinDelta: 1},
	"udp.in_pps":        {Unit: "ops/s"},
	"udp.no_ports":      {Unit: "/s", MinDelta: 1},
	"udp.out_pps":       {Unit: "ops/s"},
	"udp.rcvbuf_errors": {Unit: "/s", MinDelta: 1},
	"udp.sndbuf_errors": {Unit: "/s", MinDelta: 1},
}

// prefixed 登记"一族名字不固定"的指标：proc.cpu.<进程名> 之类。
var prefixed = []struct {
	prefix string
	info   Info
}{
	{"proc.cpu.", Info{Unit: "percent", Class: ClassCPU}},
	{"proc.io.", Info{Unit: "bytes/s", Class: ClassIO}},
	{"proc.rss.", Info{Unit: "bytes"}},
}

// Base 去掉逐设备后缀："disk.util@sda" → "disk.util"。
func Base(id string) string {
	if i := strings.IndexByte(id, '@'); i > 0 {
		return id[:i]
	}
	return id
}

// Domain 是名字第一段："disk.util@sda" → "disk"，"procs_running" → "procs_running"。
func Domain(id string) string {
	if i := strings.IndexByte(id, '.'); i > 0 {
		return id[:i]
	}
	return id
}

// Lookup 返回指标的静态属性。MinDelta 已按单位填好默认值。
func Lookup(id string) Info {
	b := Base(id)
	in, ok := known[b]
	if !ok {
		for _, p := range prefixed {
			if strings.HasPrefix(b, p.prefix) && len(b) > len(p.prefix) {
				in, ok = p.info, true
				break
			}
		}
	}
	if ok {
		in.Registered = true
	} else {
		in = Info{Unit: guessUnit(id)}
		in.Primary = primaryDomains[Domain(id)]
	}
	if in.MinDelta == 0 {
		in.MinDelta = defaultMinDelta(in.Unit)
	}
	return in
}

// primaryDomains 只给没登记的指标兜底用；登记过的直接看 Primary。
var primaryDomains = map[string]bool{
	"cpu": true, "mem": true, "disk": true, "net": true, "psi": true, "loadavg": true,
	"swap": true, "procs_running": true, "procs_blocked": true, "slab": true,
}

// defaultMinDelta 是各单位"值得一提的最小变化量"。
func defaultMinDelta(unit string) float64 {
	switch unit {
	case "percent":
		return 3 // 百分点
	case "bytes":
		return 128 << 20 // 内存类：128 MB 以下的波动是缓存在动
	case "bytes/s":
		return 1 << 20 // 1 MB/s
	case "ops/s":
		return 100 // 包/秒、IOPS
	case "/s":
		return 5
	case "ms":
		return 2
	case "load":
		return 0.5
	case "count":
		return 10
	}
	return 0
}

// guessUnit 只给没登记的指标用：按命名习惯猜单位。登记过的指标永远不走这里。
//
// 顺序要紧（这是从旧 unitOf 搬来的教训）：百分比规则必须排在 mem./swap. 前缀之前，
// 否则 mem.used_pct 会被显示成 "14.49 B"。
func guessUnit(id string) string {
	switch {
	case strings.HasSuffix(id, "_pct"), strings.Contains(id, "util"),
		strings.HasPrefix(id, "cpu."), strings.HasPrefix(id, "psi"):
		return "percent"
	case strings.HasSuffix(id, "_ms"), strings.Contains(id, "await"):
		return "ms"
	case strings.HasSuffix(id, "_s"), strings.HasSuffix(id, "_sec"):
		return "s"
	case strings.Contains(id, "_drop"), strings.Contains(id, "_errs"), strings.Contains(id, "_errors"):
		return "/s"
	case strings.Contains(id, "bytes"), strings.HasPrefix(id, "mem."), strings.HasPrefix(id, "swap."):
		return "bytes"
	case strings.Contains(id, "iops"), strings.Contains(id, "pps"), strings.HasSuffix(id, "_per_s"):
		return "ops/s"
	case strings.HasPrefix(id, "loadavg"):
		return "load"
	}
	return "count"
}
