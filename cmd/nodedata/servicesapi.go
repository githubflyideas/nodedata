// servicesapi.go — /api/services：服务标签页的数据。
package main

import (
	"sort"
	"time"
)

// svcCols 与对比表的时间列对齐，看的人不用换脑子。
var svcCols = []struct {
	Name string
	Ago  time.Duration
}{
	{"1h", time.Hour}, {"6h", 6 * time.Hour}, {"12h", 12 * time.Hour},
	{"1d", 24 * time.Hour}, {"3d", 72 * time.Hour}, {"7d", 7 * 24 * time.Hour},
	{"14d", 14 * 24 * time.Hour},
}

type svcRow struct {
	Name      string   `json:"name"`
	Exe       string   `json:"exe"`
	Kind      string   `json:"kind"`
	PID       int      `json:"pid"`
	Instances int      `json:"instances"`
	StartTS   int64    `json:"start_ts"`
	UptimeS   int64    `json:"uptime_s"`
	Ports     []int    `json:"ports"`
	Unit      string   `json:"unit"`
	CPU       float64  `json:"cpu"`
	RSS       uint64   `json:"rss"`
	Alive     bool     `json:"alive"`
	Self      bool     `json:"self,omitempty"`
	Past      []string `json:"past"` // 与 svcCols 对齐：yes/no/unknown/restart
}

type ServicesJSON struct {
	At   int64    `json:"at"`
	Cols []string `json:"cols"`
	Rows []svcRow `json:"rows"`
}

// servicesJSON 组装当前服务 + 保留期内消失的服务（后者必须留在表里，
// 一个消失的服务如果从表里也消失了，这张表就白做了）。
func servicesJSON(l *ServiceLog, now time.Time) *ServicesJSON {
	out := &ServicesJSON{At: now.Unix()}
	for _, c := range svcCols {
		out.Cols = append(out.Cols, c.Name)
	}
	past := func(id string) []string {
		st := make([]string, len(svcCols))
		for i, c := range svcCols {
			at := now.Add(-c.Ago)
			s := l.StateAt(id, at)
			// 那时在、之后又重启过 —— 标出来，这正是"老的但换过一次"
			if s == stYes && l.RestartedSince(id, at) {
				s = stRestart
			}
			st[i] = s
		}
		return st
	}
	for _, s := range l.Current() {
		out.Rows = append(out.Rows, svcRow{
			Name: s.Name, Exe: s.Exe, Kind: s.Kind, PID: s.PID, Instances: s.Instances,
			StartTS: s.StartTS, UptimeS: now.Unix() - s.StartTS, Ports: s.Ports, Unit: s.Unit,
			CPU: s.CPU, RSS: s.RSS, Alive: true, Self: s.Self, Past: past(s.ID()),
		})
	}
	for _, e := range l.Vanished() {
		out.Rows = append(out.Rows, svcRow{
			Name: e.Name, Exe: e.Exe, Ports: e.Ports, Alive: false, Past: past(e.ID),
		})
	}
	sort.Slice(out.Rows, func(i, j int) bool {
		if out.Rows[i].Alive != out.Rows[j].Alive {
			return out.Rows[i].Alive // 活着的在前，消失的沉底但仍可见
		}
		return out.Rows[i].Name < out.Rows[j].Name
	})
	return out
}
