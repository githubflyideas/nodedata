package fleet

import (
	"math"
	"time"
)

// 页面读的整份状态：/api/state。

type rowOut struct {
	Row
	St         string `json:"st"`              // ok / dev / bad / none
	Since      int64  `json:"since,omitempty"` // 不正常的起点（中心看到的，unix 秒）
	SinceKnown bool   `json:"since_known,omitempty"`
}

type hostOut struct {
	Host
	// Status：ok = 最近拉得到；lost = 超过 LostAfter 没拉到过，或者从没拉到过。
	// 失联的机器不带 rows——上一次的读数不是现在的读数，发出去页面就可能把它画成绿的。
	Status  string   `json:"status"`
	Sev     string   `json:"sev"` // 整台最坏：bad / lost / dev / none / ok
	Never   bool     `json:"never,omitempty"`
	LastOK  int64    `json:"last_ok,omitempty"`
	LastTry int64    `json:"last_try,omitempty"`
	Err     string   `json:"err,omitempty"` // 最近一次失败的原因（没失联时也可能有：偶尔一次超时）
	Skew    float64  `json:"skew,omitempty"`
	Agent   string   `json:"agent,omitempty"`
	API     int      `json:"api"`
	Rows    []rowOut `json:"rows,omitempty"`
}

type stateOut struct {
	At        int64     `json:"at"`
	Started   int64     `json:"started"`
	Interval  float64   `json:"interval"`
	LostAfter float64   `json:"lost_after"`
	RoundAt   int64     `json:"round_at,omitempty"`
	RoundMS   int64     `json:"round_ms,omitempty"`
	Inventory Inventory `json:"inventory"`
	LoadedAt  int64     `json:"inventory_loaded_at,omitempty"`
	Hosts     []hostOut `json:"hosts"`
	Events    []Event   `json:"events,omitempty"` // 最近的状态变化，旧的在前
	Version   string    `json:"version"`
}

var sevRank = map[string]int{"ok": 0, "none": 1, "dev": 2, "lost": 3, "bad": 4}

// resources 固定五行的顺序。对方少报一行，这边补成"无数据"，不补成正常。
var resources = []string{"CPU", "内存", "磁盘", "网络", "系统"}

// Snapshot 生成页面要的状态。
func (p *Poller) Snapshot(now time.Time, interval, lostAfter time.Duration, inv Inventory) stateOut {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := stateOut{
		At: now.Unix(), Started: p.started.Unix(),
		Interval: interval.Seconds(), LostAfter: lostAfter.Seconds(),
		Inventory: inv, Version: version,
		Hosts: make([]hostOut, 0, len(p.hosts)),
	}
	if !inv.LoadedAt.IsZero() {
		out.LoadedAt = inv.LoadedAt.Unix()
	}
	out.Events = p.recentEventsLocked()
	if !p.roundAt.IsZero() {
		out.RoundAt, out.RoundMS = p.roundAt.Unix(), p.roundTook.Milliseconds()
	}
	for _, h := range p.hosts {
		s := p.st[h.Name]
		ho := hostOut{Host: h, Status: "lost", Sev: "lost"}
		if s == nil {
			ho.Never = true
			out.Hosts = append(out.Hosts, ho)
			continue
		}
		ho.Err = s.err
		if !s.lastTry.IsZero() {
			ho.LastTry = s.lastTry.Unix()
		}
		if s.lastOK.IsZero() {
			ho.Never = true
			out.Hosts = append(out.Hosts, ho)
			continue
		}
		ho.LastOK = s.lastOK.Unix()
		ho.Agent, ho.API = s.doc.Version, s.doc.V
		if now.Sub(s.lastOK) > lostAfter {
			out.Hosts = append(out.Hosts, ho)
			continue
		}
		ho.Status = "ok"
		ho.Skew = math.Round(s.skew*10) / 10
		by := map[string]Row{}
		for _, r := range s.doc.Rows {
			by[r.Resource] = r
		}
		ho.Sev = "ok"
		for _, name := range resources {
			r, ok := by[name]
			if !ok {
				r = Row{Resource: name, State: "无数据"}
			}
			ro := rowOut{Row: r, St: stCode(r.State)}
			if t, ok := s.since[name]; ok && ro.St != "ok" {
				ro.Since, ro.SinceKnown = t.Unix(), s.sinceKnown[name]
			}
			if sevRank[ro.St] > sevRank[ho.Sev] {
				ho.Sev = ro.St
			}
			ho.Rows = append(ho.Rows, ro)
		}
		out.Hosts = append(out.Hosts, ho)
	}
	return out
}

// Counts 给 /health.txt 用。
func (s stateOut) Counts() (ok, lost, bad, dev int) {
	for _, h := range s.Hosts {
		switch {
		case h.Status == "lost":
			lost++
		case h.Sev == "bad":
			bad++
		case h.Sev == "dev":
			dev++
		default:
			ok++
		}
	}
	return
}
