// sockets.go — 套接字与 UDP 错误：/proc/net/sockstat 与 /proc/net/snmp。
//
// 补上此前整块缺失的一类指标。对 UDP 服务（DNS、NTP、syslog）来说，
// 最先出问题的往往不是 CPU 或内存，而是这两项：
//
//   - **UDP 接收缓冲区溢出**（snmp Udp: RcvbufErrors）。应用来不及收包，内核直接丢。
//     网卡层的 rx_drop 一个都不涨，业务侧却已经在超时 —— 只看 net.rx_drop 看不到。
//   - **TCP 会话水位**（snmp Tcp: CurrEstab、sockstat TCP tw/orphan）。
//     TIME_WAIT 堆积、orphan 上涨是端口耗尽与连接泄漏的早期信号。
//
// 另外把"条目数 / 上限"配齐：conntrack 与 socket 这类有硬上限的资源，
// 绝对值本身没有意义，占上限的百分比才是能设阈值的东西。
package collector

import (
	"bytes"
	"time"
)

// parseSockstat 解析 /proc/net/sockstat：
//
//	sockets: used 11
//	TCP: inuse 6 orphan 0 tw 0 alloc 6 mem 0
//	UDP: inuse 0 mem 0
//
// 全是瞬时量（gauge），不做差分。
func (c *Collector) parseSockstat(data []byte, now time.Time, out *[]Sample) {
	emit := func(id string, v uint64) {
		*out = append(*out, Sample{MetricID: id, TS: now, Value: float64(v)})
	}
	for len(data) > 0 {
		var line []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			line, data = data, nil
		}
		f := c.splitFieldsBuf(line)
		if len(f) < 3 {
			continue
		}
		// 键在 f[1]、f[3]、f[5]…，值在其后一个字段
		get := func(key string) (uint64, bool) {
			for i := 1; i+1 < len(f); i += 2 {
				if string(f[i]) == key {
					v, err := parseUint64(f[i+1])
					return v, err == nil
				}
			}
			return 0, false
		}
		switch string(f[0]) {
		case "sockets:":
			if v, ok := get("used"); ok {
				emit("sock.used", v)
			}
		case "TCP:":
			for key, id := range map[string]string{"inuse": "sock.tcp_inuse", "tw": "sock.tcp_tw", "orphan": "sock.tcp_orphan", "alloc": "sock.tcp_alloc"} {
				if v, ok := get(key); ok {
					emit(id, v)
				}
			}
		case "UDP:":
			if v, ok := get("inuse"); ok {
				emit("sock.udp_inuse", v)
			}
		}
	}
}

// snmpWanted 是要从 /proc/net/snmp 取的字段。rate=true 的做差分（累计计数器），
// 否则按瞬时量输出。
var snmpWanted = map[string]map[string]struct {
	id   string
	rate bool
}{
	"Tcp:": {
		"RetransSegs":  {"tcp.retrans", true},
		"CurrEstab":    {"tcp.estab", false},
		"PassiveOpens": {"tcp.passive_opens", true},
		"AttemptFails": {"tcp.attempt_fails", true},
	},
	"Udp:": {
		// InErrors：校验和错、无缓冲等；RcvbufErrors：接收缓冲区满，应用来不及收
		"InErrors":     {"udp.in_errors", true},
		"RcvbufErrors": {"udp.rcvbuf_errors", true},
		"SndbufErrors": {"udp.sndbuf_errors", true},
		"NoPorts":      {"udp.no_ports", true},
		"InDatagrams":  {"udp.in_pps", true},
		"OutDatagrams": {"udp.out_pps", true},
	},
}

// parseNetSnmpV2 解析 /proc/net/snmp。文件是"表头行 + 数值行"成对出现，
// 列的位置随内核版本变化，所以必须按表头名取，不能按固定下标。
func (c *Collector) parseNetSnmpV2(data []byte, now time.Time, dt float64, hasPrev bool, out *[]Sample) {
	var hdr [][]byte
	var hdrPrefix string
	for len(data) > 0 {
		var line []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			line, data = data, nil
		}
		f := c.splitFieldsBuf(line)
		if len(f) < 2 {
			continue
		}
		prefix := string(f[0])
		want, ok := snmpWanted[prefix]
		if !ok {
			continue
		}
		if _, err := parseUint64(f[1]); err != nil { // 非数字 = 表头行
			hdr = append(hdr[:0], f...)
			hdrPrefix = prefix
			continue
		}
		if hdrPrefix != prefix || len(hdr) != len(f) {
			continue // 表头与数值行不配对，跳过而不是错位取值
		}
		for i := 1; i < len(f); i++ {
			w, ok := want[string(hdr[i])]
			if !ok {
				continue
			}
			v, err := parseUint64(f[i])
			if err != nil {
				continue
			}
			if !w.rate {
				*out = append(*out, Sample{MetricID: w.id, TS: now, Value: float64(v)})
				continue
			}
			prev, seen := c.prevGlobal[w.id]
			c.prevGlobal[w.id] = v
			if hasPrev && seen && dt > 0 {
				*out = append(*out, Sample{MetricID: w.id, TS: now, Value: udiff(v, prev) / dt})
			}
		}
	}
}
