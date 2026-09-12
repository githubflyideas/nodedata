// servicelog.go — 服务事件流：出现 / 消失 / 重启，外加 nodedata 自身的启停。
//
// 为什么存事件而不是周期快照：服务集合绝大多数时候一动不动，连着几千个快照完全一样。
// 一台稳定的机器 14 天可能就十几条事件，几十 KB —— 比指标数据小三个数量级。
//
// 三种状态，缺一不可：
//
//	yes      那时在
//	no       那时确实不在（nodedata 在跑，没看见它）
//	unknown  不知道（nodedata 当时没在跑）
//
// 把 unknown 画成 no，就等于告诉人"你的 MySQL 那天挂了"，而实际上只是我们没在看。
// 这跟对比表里"没数据显示 — 而不是 0"是同一个原则。
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
)

// 事件类型。
const (
	evAppear  = "+"    // 服务出现
	evVanish  = "-"    // 服务消失
	evRestart = "~"    // 名字还在，starttime 变了
	evUp      = "up"   // nodedata 开始观察
	evDown    = "down" // nodedata 停止观察（正常退出时写）
)

// svcEvent 是事件流里的一行。
type svcEvent struct {
	TS      int64  `json:"ts"`
	Kind    string `json:"k"`
	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
	Exe     string `json:"exe,omitempty"`
	PID     int    `json:"pid,omitempty"`
	StartTS int64  `json:"start,omitempty"`
	Ports   []int  `json:"ports,omitempty"`
}

// vanishConfirm：连续这么多轮没看见才算消失。一轮抖动（读 /proc 撞上进程重启）不算。
const vanishConfirm = 3

// ServiceLog 维护当前服务集合、事件流与落盘。
type ServiceLog struct {
	path   string
	retain time.Duration

	mu      sync.Mutex
	cur     map[string]collector.Service // ID → 服务
	missing map[string]int               // ID → 连续未见轮数
	events  []svcEvent
	learned bool      // 学习期是否已结束
	startAt time.Time // 本次开始观察的时刻
}

// learnPeriod：启动后这段时间内只建立基线，不产生"出现"事件。
// 否则新装一台机器，所有服务都是"刚出现"，满屏噪声。
const learnPeriod = 15 * time.Minute

func NewServiceLog(path string, retain time.Duration) *ServiceLog {
	return &ServiceLog{path: path, retain: retain,
		cur: map[string]collector.Service{}, missing: map[string]int{}}
}

// Load 读回历史事件（截断行跳过），并写一条 up 事件表示"从此刻起我们在看"。
func (l *ServiceLog) Load(now time.Time) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.startAt = now
	f, err := os.Open(l.path)
	if err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var e svcEvent
			if json.Unmarshal(sc.Bytes(), &e) == nil && e.TS != 0 && e.Kind != "" {
				l.events = append(l.events, e)
			}
		}
		f.Close()
	} else if !os.IsNotExist(err) {
		return 0, err
	}
	l.expireLocked(now)
	n := len(l.events)
	l.appendLocked(svcEvent{TS: now.Unix(), Kind: evUp})
	return n, nil
}

// Close 写一条 down 事件，之后的时间段在查询时算 unknown 而不是"服务消失"。
func (l *ServiceLog) Close(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.appendLocked(svcEvent{TS: now.Unix(), Kind: evDown})
}

// Update 用一轮采集结果更新集合，产生事件。返回本轮新增的事件。
func (l *ServiceLog) Update(svcs []collector.Service, now time.Time) []svcEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	learning := now.Sub(l.startAt) < learnPeriod
	seen := make(map[string]bool, len(svcs))
	var out []svcEvent

	for _, s := range svcs {
		id := s.ID()
		seen[id] = true
		delete(l.missing, id)
		prev, had := l.cur[id]
		l.cur[id] = s
		switch {
		case !had:
			// 学习期内只建基线：新装一台机器时所有服务都是"刚出现"，那是噪声不是事件。
			// 但事件照记（时间戳记成开始观察的时刻），否则重启后历史里查不到它。
			e := svcEvent{TS: now.Unix(), Kind: evAppear, ID: id, Name: s.Name,
				Exe: s.Exe, PID: s.PID, StartTS: s.StartTS, Ports: s.Ports}
			if learning {
				e.TS = l.startAt.Unix()
				l.appendLocked(e)
			} else {
				out = append(out, l.appendLocked(e))
			}
		case prev.StartTS != s.StartTS:
			out = append(out, l.appendLocked(svcEvent{TS: now.Unix(), Kind: evRestart,
				ID: id, Name: s.Name, Exe: s.Exe, PID: s.PID, StartTS: s.StartTS, Ports: s.Ports}))
		}
	}
	for id, s := range l.cur {
		if seen[id] {
			continue
		}
		if l.missing[id]++; l.missing[id] < vanishConfirm {
			continue
		}
		delete(l.cur, id)
		delete(l.missing, id)
		out = append(out, l.appendLocked(svcEvent{TS: now.Unix(), Kind: evVanish,
			ID: id, Name: s.Name, Exe: s.Exe, Ports: s.Ports}))
	}
	if len(out) > 0 {
		l.expireLocked(now)
	}
	return out
}

func (l *ServiceLog) appendLocked(e svcEvent) svcEvent {
	l.events = append(l.events, e)
	if b, err := json.Marshal(e); err == nil {
		if f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			f.Write(append(b, '\n'))
			f.Close()
		}
	}
	return e
}

// expireLocked 丢掉保留期之外的事件并重写文件。
func (l *ServiceLog) expireLocked(now time.Time) {
	cut := now.Add(-l.retain).Unix()
	i := 0
	for i < len(l.events) && l.events[i].TS < cut {
		i++
	}
	if i == 0 {
		return
	}
	l.events = append([]svcEvent(nil), l.events[i:]...)
	var b strings.Builder
	for _, e := range l.events {
		if j, err := json.Marshal(e); err == nil {
			b.Write(j)
			b.WriteByte('\n')
		}
	}
	tmp := l.path + ".tmp"
	if os.WriteFile(tmp, []byte(b.String()), 0o644) == nil {
		os.Rename(tmp, l.path)
	}
}

// 历史状态。
const (
	stYes     = "yes"
	stNo      = "no"
	stUnknown = "unknown"
	stRestart = "restart"
)

// StateAt 回答"某服务在某时刻在不在"。
// 先看那个时刻 nodedata 自己在不在跑：不在就是 unknown，绝不当成 no。
func (l *ServiceLog) StateAt(id string, at time.Time) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stateAtLocked(id, at.Unix())
}

func (l *ServiceLog) stateAtLocked(id string, ts int64) string {
	watching := false
	haveWatchInfo := false
	state := stNo
	known := false
	for _, e := range l.events {
		if e.TS > ts {
			break
		}
		switch e.Kind {
		case evUp:
			watching, haveWatchInfo = true, true
		case evDown:
			watching, haveWatchInfo = false, true
		case evAppear:
			if e.ID == id {
				state, known = stYes, true
			}
		case evRestart:
			if e.ID == id {
				state, known = stYes, true
			}
		case evVanish:
			if e.ID == id {
				state, known = stNo, true
			}
		}
	}
	if haveWatchInfo && !watching {
		return stUnknown // 那时我们没在看
	}
	if !known {
		return stUnknown // 那个时刻之前从没见过它
	}
	return state
}

// RestartedSince 判断某服务在 [at, now] 之间是否重启过（用于把列标成"重启"）。
func (l *ServiceLog) RestartedSince(id string, at time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := at.Unix()
	for _, e := range l.events {
		if e.TS >= ts && e.Kind == evRestart && e.ID == id {
			return true
		}
	}
	return false
}

// Current 返回当前服务集合（按名字排序）。
func (l *ServiceLog) Current() []collector.Service {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]collector.Service, 0, len(l.cur))
	for _, s := range l.cur {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Vanished 返回保留期内消失、且现在仍不在的服务（这些行必须留在表里）。
func (l *ServiceLog) Vanished() []svcEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	last := map[string]svcEvent{}
	for _, e := range l.events {
		switch e.Kind {
		case evAppear, evRestart, evVanish:
			last[e.ID] = e
		}
	}
	var out []svcEvent
	for id, e := range last {
		if e.Kind == evVanish {
			if _, alive := l.cur[id]; !alive {
				out = append(out, e)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
