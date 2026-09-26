package fleet

import "time"

// Event 是一次状态变化：巡视台亲眼看到的，不是推断的。
//
//	state：某台某资源从 from 变成 to（ok / dev / bad / none）
//	lost ：超过 lost-after 没拉到
//	back ：失联后又拉到了
//
// 只记"变化"，而且只记巡视台看见它发生的那些：启动时就已经不正常的不算变化，
// 从没拉到过的机器也不会有 lost 事件（它从来没"在"过）。
type Event struct {
	At   int64  `json:"at"`
	Host string `json:"host"`
	Kind string `json:"kind"`
	Res  string `json:"res,omitempty"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

const (
	maxEvents  = 200 // 内存里留多少条
	sendEvents = 60  // /api/state 带多少条
)

func (p *Poller) addEventLocked(e Event) {
	p.events = append(p.events, e)
	if n := len(p.events); n > maxEvents {
		copy(p.events, p.events[n-maxEvents:])
		p.events = p.events[:maxEvents]
	}
}

// markLostLocked 在每轮结束时检查：刚跨过失联线的机器记一条 lost。
func (p *Poller) markLostLocked(now time.Time) {
	if p.lostAfter <= 0 {
		return
	}
	for _, h := range p.hosts {
		s := p.st[h.Name]
		if s == nil || s.lastOK.IsZero() || s.lostMarked {
			continue
		}
		if now.Sub(s.lastOK) > p.lostAfter {
			s.lostMarked = true
			p.addEventLocked(Event{At: now.Unix(), Host: h.Name, Kind: "lost"})
		}
	}
}

// recentEventsLocked 返回最近的若干条（旧的在前）。
func (p *Poller) recentEventsLocked() []Event {
	n := len(p.events)
	if n > sendEvents {
		return append([]Event(nil), p.events[n-sendEvents:]...)
	}
	return append([]Event(nil), p.events...)
}
