// poller.go — 每轮并发去各台拉一次 USE 五行。
//
// 只拉不推：各台不需要知道中心在哪，也不主动往外发东西。
// 判断在各台已经用各自的基线做完了，这里只收"判完的状态"，不收原始曲线，
// 所以 100 台一轮也就几百 KB，中心和各台都不会因为巡视而变慢。
package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ── 从各台读回来的东西。只声明用得到的字段：对方多了字段不影响，少了字段就是零值。

type Fact struct {
	Kind  string   `json:"kind"`
	Label string   `json:"label"`
	ID    string   `json:"id"`
	Value float64  `json:"value"`
	Unit  string   `json:"unit"`
	Base  *float64 `json:"base,omitempty"`
	Z     float64  `json:"z,omitempty"`
	Note  string   `json:"note,omitempty"`
	// v5.25：Notable=这条读数构成偏离；Change=显著但不是问题（"高于平时"/"低于平时"）。旧版 nodedata 没有这两个字段。
	Notable bool   `json:"notable,omitempty"`
	Change  string `json:"change,omitempty"`
}

type Check struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Level   int    `json:"level"`
	Message string `json:"message"`
}

type Mover struct {
	Name   string  `json:"name"`
	Unit   string  `json:"unit"`
	Now    float64 `json:"now"`
	PID    int     `json:"pid,omitempty"`
	Parent string  `json:"parent,omitempty"`
}

type Row struct {
	Resource string   `json:"resource"`
	State    string   `json:"state"` // 正常 / 偏离 / 异常 / 无数据（各台原文）
	Facts    []Fact   `json:"facts,omitempty"`
	Checks   []Check  `json:"checks,omitempty"`
	Movers   []Mover  `json:"movers,omitempty"`
	Checked  []string `json:"checked,omitempty"`
	L0Total  int      `json:"l0_total,omitempty"`
	L0Pass   int      `json:"l0_pass,omitempty"`
}

type agentDoc struct {
	V       int    `json:"v"`
	Host    string `json:"host"`
	Version string `json:"version"`
	At      int64  `json:"at"`
	Rows    []Row  `json:"rows"`
}

// stCode 把各台的中文状态映射成页面用的代码。认不出的一律当"无数据"，绝不当正常。
func stCode(s string) string {
	switch s {
	case "正常":
		return "ok"
	case "偏离":
		return "dev"
	case "异常":
		return "bad"
	}
	return "none"
}

// ── 每台的状态

type hostState struct {
	lastTry, lastOK time.Time
	err             string
	doc             agentDoc
	skew            float64 // 对方时钟 - 中心时钟，秒
	// since：这个资源从什么时候开始不正常。sinceKnown=false 表示中心第一次看到它时就已经不正常了
	// （比如中心刚重启），这时的 since 只是"中心开始看的时间"，页面不能把它当成起点。
	since      map[string]time.Time
	sinceKnown map[string]bool
	prev       map[string]string
	lostMarked bool // 已经记过一条 lost 事件，等拉到了再记 back
}

// Poller 持有所有主机的最新状态。
type Poller struct {
	client   *http.Client
	timeout  time.Duration
	parallel int
	ua       string

	mu        sync.RWMutex
	hosts     []Host
	st        map[string]*hostState
	started   time.Time
	roundAt   time.Time
	roundTook time.Duration

	lostAfter time.Duration // 用于记 lost 事件；0 = 不记
	events    []Event
}

func NewPoller(timeout time.Duration, parallel int, ua string) *Poller {
	tr := &http.Transport{
		Proxy:               nil, // 内网直连；清单里写反代 URL 的照样能走
		DialContext:         (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
	}
	return &Poller{
		client:   &http.Client{Transport: tr, Timeout: timeout},
		timeout:  timeout,
		parallel: parallel,
		ua:       ua,
		st:       map[string]*hostState{},
		started:  time.Now(),
	}
}

// SetHosts 换清单。同名主机的状态保留：改了机柜、加了标签不该让它的"失联多久"清零。
func (p *Poller) SetHosts(hs []Host) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hosts = append([]Host(nil), hs...)
	keep := map[string]*hostState{}
	for _, h := range hs {
		if s, ok := p.st[h.Name]; ok {
			keep[h.Name] = s
		} else {
			keep[h.Name] = &hostState{since: map[string]time.Time{}, sinceKnown: map[string]bool{}, prev: map[string]string{}}
		}
	}
	p.st = keep
}

// Round 并发拉一轮。
func (p *Poller) Round(ctx context.Context) {
	p.mu.RLock()
	hs := append([]Host(nil), p.hosts...)
	p.mu.RUnlock()

	start := time.Now()
	sem := make(chan struct{}, p.parallel)
	var wg sync.WaitGroup
	for _, h := range hs {
		wg.Add(1)
		sem <- struct{}{}
		go func(h Host) {
			defer wg.Done()
			defer func() { <-sem }()
			t0 := time.Now()
			doc, err := p.fetch(ctx, h.URL)
			t1 := time.Now()
			p.record(h.Name, doc, err, t0, t1)
		}(h)
	}
	wg.Wait()
	p.mu.Lock()
	p.roundAt, p.roundTook = time.Now(), time.Since(start)
	p.markLostLocked(p.roundAt)
	p.mu.Unlock()
}

func (p *Poller) record(name string, doc agentDoc, err error, t0, t1 time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.st[name]
	if !ok {
		return // 这一轮中间清单换了，这台已经不在了
	}
	s.lastTry = t1
	if err != nil {
		s.err = explain(err)
		return
	}
	s.err = ""
	firstOK := s.lastOK.IsZero()
	if s.lostMarked {
		s.lostMarked = false
		p.addEventLocked(Event{At: t1.Unix(), Host: name, Kind: "back"})
	}
	s.lastOK = t1
	s.doc = doc
	s.skew = 0
	if doc.At > 0 {
		mid := t0.Add(t1.Sub(t0) / 2)
		s.skew = float64(doc.At) - float64(mid.UnixNano())/1e9
	}
	cur := map[string]bool{}
	for _, r := range doc.Rows {
		code := stCode(r.State)
		cur[r.Resource] = true
		if old, had := s.prev[r.Resource]; had && !firstOK && old != code {
			p.addEventLocked(Event{At: t1.Unix(), Host: name, Kind: "state", Res: r.Resource, From: old, To: code})
		}
		if code == "ok" {
			delete(s.since, r.Resource)
			delete(s.sinceKnown, r.Resource)
		} else if _, has := s.since[r.Resource]; !has {
			s.since[r.Resource] = t1
			// 上一次成功拉取时它是正常的，才知道它是"这一轮之间"变坏的
			s.sinceKnown[r.Resource] = !firstOK && s.prev[r.Resource] == "ok"
		}
		s.prev[r.Resource] = code
	}
	for res := range s.prev {
		if !cur[res] {
			delete(s.prev, res)
			delete(s.since, res)
			delete(s.sinceKnown, res)
		}
	}
}

const maxBody = 4 << 20

func (p *Poller) fetch(ctx context.Context, base string) (agentDoc, error) {
	var doc agentDoc
	code, err := p.get(ctx, base+"/api/fleet", &doc)
	if err == nil {
		return doc, nil
	}
	// v5.21 及更早的 nodedata 没有 /api/fleet，退回 /api/use（光秃秃一个数组，没有版本和时间）
	if code == http.StatusNotFound {
		var rows []Row
		if _, err2 := p.get(ctx, base+"/api/use", &rows); err2 != nil {
			return doc, err2
		}
		return agentDoc{V: 0, Rows: rows}, nil
	}
	return doc, err
}

func (p *Poller) get(ctx context.Context, url string, into any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", p.ua)
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return resp.StatusCode, fmt.Errorf("对方返回 HTTP %d", resp.StatusCode)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxBody))
	if err := dec.Decode(into); err != nil {
		return resp.StatusCode, fmt.Errorf("返回的不是 nodedata 的数据（%v）", err)
	}
	return resp.StatusCode, nil
}

// explain 把网络错误翻译成部署时最可能的原因。只翻译认得出的，认不出的原样给。
func explain(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "连接被拒绝：对方 nodedata 没在跑，或只监听了 127.0.0.1（需要 --listen 0.0.0.0）"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "网络不通：路由或防火墙"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "超时：防火墙丢包，或对方机器卡住了"
	case strings.Contains(err.Error(), "no such host"):
		return "域名解析失败"
	}
	return err.Error()
}
