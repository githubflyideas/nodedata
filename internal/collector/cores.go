// cores.go — 逐核 CPU：只把"最忙的 3 个核"和"软中断最高的核"写进历史。
//
// 为什么不是每核一条序列：128 核 × 若干字段就是几百条序列，存储和页面都会被撑爆，
// 而且"第 57 号核三天前多忙"几乎没人问。真正要回答的是三个形状问题：
//
//	100 / 12 / 10   一个单线程打满一个核，或网卡中断全压在一个核上
//	100 / 100 / 98  三个线程（或三个网卡队列）都满了
//	 45 / 44 / 43   负载很匀，瓶颈不在单核
//
// 另存一条同口径的整机忙碌率 cpu.busy_pct，拿最忙那个核一比就是偏斜程度。
//
// 代价：每轮的"第 1 名"可能是不同的核，这几条序列回答的是"最忙的几个核有多忙"，
// 不是"某个固定核有多忙"。中断绑核那种核号固定的情况，要看实时的逐核明细。
//
// 开销：cpuN 行本来就在同一次 /proc/stat 读取里，不多一次系统调用；
// 选前 3 是线性扫描，128 核也在微秒级。节拍缓冲按核数复用；每轮唯一的分配是
// 交给页面的那份只读快照（并发读取需要一份独立的副本）。
package collector

import "time"

// coreTicks 是某个核一轮的累计节拍。
type coreTicks struct {
	total, idle, softirq uint64
	ok                   bool
}

// CoreStat 是某个核最近一轮的占用，供页面画实时逐核明细。
type CoreStat struct {
	CPU     int     `json:"cpu"`
	Busy    float64 `json:"busy"`    // 0–100，不含 idle 与 iowait
	Softirq float64 `json:"softirq"` // 0–100
}

// coreTopN 是写进历史的"最忙的几个核"个数。
const coreTopN = 3

// coreMetricIDs 预先驻留，避免每轮拼字符串。
var coreMetricIDs = [coreTopN]string{"cpu.core_top1", "cpu.core_top2", "cpu.core_top3"}

// noteCoreLine 记下一行 "cpuN user nice system idle iowait irq softirq steal ..." 的节拍。
// fields[0] 是 "cpuN"。不合法的行直接忽略——宁可少一个核，也不要让采集失败。
func (c *Collector) noteCoreLine(fields [][]byte) {
	if len(fields) < 9 {
		return
	}
	idx := 0
	for _, ch := range fields[0][3:] {
		if ch < '0' || ch > '9' {
			return
		}
		idx = idx*10 + int(ch-'0')
	}
	if idx > 8192 { // 防御：离谱的核号不值得为它扩容
		return
	}
	var v [9]uint64
	for i := 1; i <= 8; i++ {
		x, err := parseUint64(fields[i])
		if err != nil {
			return
		}
		v[i] = x
	}
	// 1=user 2=nice 3=system 4=idle 5=iowait 6=irq 7=softirq 8=steal。
	// guest/guest_nice 已经算在 user/nice 里了，不能再加一遍。
	total := v[1] + v[2] + v[3] + v[4] + v[5] + v[6] + v[7] + v[8]
	for len(c.coreCur) <= idx {
		c.coreCur = append(c.coreCur, coreTicks{})
	}
	// iowait 算空闲：它是"核没事干、在等盘"，不是"核在忙"。
	c.coreCur[idx] = coreTicks{total: total, idle: v[4] + v[5], softirq: v[7], ok: true}
}

// finishCores 在一轮 /proc/stat 解析结束后调用：算出每核占用，写出 top3 与软中断最高值，
// 并把本轮节拍存成下一轮的基准。
func (c *Collector) finishCores(now time.Time, hasPrev bool, out *[]Sample) {
	n := len(c.coreCur)
	if n == 0 {
		return
	}
	if cap(c.coreStats) < n {
		c.coreStats = make([]CoreStat, 0, n)
	}
	c.coreStats = c.coreStats[:0]

	var top [coreTopN]float64
	have := 0
	softMax, softOK := 0.0, false
	busySum, busyN := 0.0, 0

	for i := 0; i < n; i++ {
		cur := c.coreCur[i]
		if !cur.ok {
			continue
		}
		if !hasPrev || i >= len(c.corePrev) || !c.corePrev[i].ok {
			continue
		}
		prev := c.corePrev[i]
		// 回绕或核被下线又上线：节拍倒退，这一轮跳过这个核
		if cur.total <= prev.total || cur.idle < prev.idle || cur.softirq < prev.softirq {
			continue
		}
		dt := float64(cur.total - prev.total)
		busy := 100 * (dt - float64(cur.idle-prev.idle)) / dt
		soft := 100 * float64(cur.softirq-prev.softirq) / dt
		if busy < 0 {
			busy = 0
		}
		c.coreStats = append(c.coreStats, CoreStat{CPU: i, Busy: busy, Softirq: soft})
		busySum += busy
		busyN++

		// 插入式维护前 3 名：线性、零分配
		if have < coreTopN {
			top[have] = busy
			have++
			for j := have - 1; j > 0 && top[j] > top[j-1]; j-- {
				top[j], top[j-1] = top[j-1], top[j]
			}
		} else if busy > top[coreTopN-1] {
			top[coreTopN-1] = busy
			for j := coreTopN - 1; j > 0 && top[j] > top[j-1]; j-- {
				top[j], top[j-1] = top[j-1], top[j]
			}
		}
		if !softOK || soft > softMax {
			softMax, softOK = soft, true
		}
	}

	// 2 核机器只有 top1/top2：不编一个不存在的第 3 核
	for i := 0; i < have; i++ {
		*out = append(*out, Sample{MetricID: coreMetricIDs[i], TS: now, Value: top[i]})
	}
	if softOK {
		*out = append(*out, Sample{MetricID: "cpu.core_softirq_max", TS: now, Value: softMax})
	}
	// 整机忙碌率，0–100，跟逐核同一口径。整机 cpu.* 是"占一个核的百分比"之和
	// （16 核满载 = 1600%），跟"最忙单核 100%"放在一起会让人看不懂；
	// 两个数同口径，一比就知道偏不偏：整机 7%、单核 100% = 一个核被钉死了。
	if busyN > 0 {
		*out = append(*out, Sample{MetricID: "cpu.busy_pct", TS: now, Value: busySum / float64(busyN)})
	}

	// 本轮 → 下一轮基准。交换两个切片，不分配。
	c.corePrev, c.coreCur = c.coreCur, c.corePrev[:0]
	c.coreSnap.Store(append([]CoreStat(nil), c.coreStats...))
}

// Cores 返回最近一轮的逐核占用（按核号排列）。没有数据时返回 nil。
// 这份数据只在内存里，不落盘：历史里只有 top3 与软中断最高值。
func (c *Collector) Cores() []CoreStat {
	v, _ := c.coreSnap.Load().([]CoreStat)
	return v
}
