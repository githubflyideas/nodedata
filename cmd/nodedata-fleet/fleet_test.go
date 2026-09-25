package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseInventory(t *testing.T) {
	in := `# 注释
db-01  10.0.1.11:19999  dc=tokyo1 rack=A03 product=支付 group=mysql-主 tags=核心,SSD
web-01 10.0.1.12        # 没写端口 → 默认 8888
px-01  https://ops:secret@proxy.example/nd-px-01/   rack=B01

bad-1
db-01  10.0.1.13
x!y    10.0.1.14
k-01   10.0.1.15  colour=red
k-02   ftp://10.0.1.16
`
	hs, errs := ParseInventory(strings.NewReader(in))
	if len(hs) != 3 {
		t.Fatalf("应读到 3 台，实得 %d：%+v", len(hs), hs)
	}
	if hs[0].URL != "http://10.0.1.11:19999" || hs[0].Rack != "A03" || strings.Join(hs[0].Tags, "|") != "核心|SSD" {
		t.Errorf("第一台解析错：%+v", hs[0])
	}
	if hs[1].URL != "http://10.0.1.12:8888" {
		t.Errorf("没写端口应补 8888，实得 %s", hs[1].URL)
	}
	if hs[2].URL != "https://ops:secret@proxy.example/nd-px-01" {
		t.Errorf("完整 URL 应原样用（去掉末尾斜杠），实得 %s", hs[2].URL)
	}
	if strings.Contains(hs[2].Addr, "secret") || strings.Contains(hs[2].Addr, "ops") {
		t.Errorf("显示地址不能带用户名密码：%s", hs[2].Addr)
	}
	want := []string{"第 6 行", "第 7 行", "第 8 行", "第 9 行", "第 10 行"}
	if len(errs) != len(want) {
		t.Fatalf("应有 %d 条错误，实得 %d：%v", len(want), len(errs), errs)
	}
	for i, w := range want {
		if !strings.HasPrefix(errs[i], w) {
			t.Errorf("错误 %d 应以 %q 开头：%s", i, w, errs[i])
		}
	}
}

// 已经有一版能用的清单时，新文件只要有一处错，就整份不采用——
// 否则一个笔误会让那一行的机器从大屏上悄悄消失。
func TestInventoryKeepsLastGoodOnError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "host.list")
	write := func(s string, mt time.Time) {
		os.WriteFile(p, []byte(s), 0o644)
		os.Chtimes(p, mt, mt)
	}
	t0 := time.Now().Add(-time.Hour)
	write("a 10.0.0.1\nb 10.0.0.2\n", t0)
	st := newInvStore(p)
	if !st.Reload(t0) || len(st.Get().Hosts) != 2 {
		t.Fatal("首次应读到 2 台")
	}
	write("a 10.0.0.1\nb 10.0.0.2 rakc=A1\nc 10.0.0.3\n", t0.Add(time.Minute))
	if st.Reload(t0.Add(time.Minute)) {
		t.Fatal("坏文件不应被采用")
	}
	g := st.Get()
	if len(g.Hosts) != 2 || !g.Stale || len(g.Errors) != 1 || !strings.Contains(g.Errors[0], "rakc") {
		t.Fatalf("应保留旧的 2 台并挂出错误：%+v", g)
	}
	write("a 10.0.0.1\nb 10.0.0.2 rack=A1\nc 10.0.0.3\n", t0.Add(2*time.Minute))
	if !st.Reload(t0.Add(2*time.Minute)) || len(st.Get().Hosts) != 3 || st.Get().Stale || len(st.Get().Errors) != 0 {
		t.Fatalf("改好后应采用新版：%+v", st.Get())
	}
	os.Remove(p)
	st.Reload(t0.Add(3 * time.Minute))
	if g := st.Get(); len(g.Hosts) != 3 || !g.Stale {
		t.Fatalf("文件没了应继续用上一版并标 stale：%+v", g)
	}
}

// 首次启动没有旧版可退：能读的先用上，错误照样挂出来。
func TestInventoryFirstLoadPartial(t *testing.T) {
	p := filepath.Join(t.TempDir(), "host.list")
	os.WriteFile(p, []byte("a 10.0.0.1\nbroken\n"), 0o644)
	st := newInvStore(p)
	st.Reload(time.Now())
	if g := st.Get(); len(g.Hosts) != 1 || len(g.Errors) != 1 || g.Stale {
		t.Fatalf("%+v", g)
	}
}

// ── 模拟几台 nodedata

type fakeAgent struct {
	srv   *httptest.Server
	state atomic.Value // map[string]string resource→state
	down  atomic.Bool
	old   bool // 旧版：没有 /api/fleet
	skew  time.Duration
}

func newFakeAgent(t *testing.T, old bool) *fakeAgent {
	a := &fakeAgent{old: old}
	a.state.Store(map[string]string{"CPU": "正常", "内存": "正常", "磁盘": "正常", "网络": "正常", "系统": "正常"})
	rows := func() []Row {
		var rs []Row
		for _, r := range resources {
			rs = append(rs, Row{Resource: r, State: a.state.Load().(map[string]string)[r],
				Facts: []Fact{{Label: "整机忙碌", ID: "cpu.busy_pct", Value: 12, Unit: "percent"}}})
		}
		return rs
	}
	mux := http.NewServeMux()
	if !old {
		mux.HandleFunc("/api/fleet", func(w http.ResponseWriter, r *http.Request) {
			if a.down.Load() {
				http.Error(w, "x", 503)
				return
			}
			json.NewEncoder(w).Encode(agentDoc{V: 1, Host: "x", Version: "v9", At: time.Now().Add(a.skew).Unix(), Rows: rows()})
		})
	}
	mux.HandleFunc("/api/use", func(w http.ResponseWriter, r *http.Request) {
		if a.down.Load() {
			http.Error(w, "x", 503)
			return
		}
		json.NewEncoder(w).Encode(rows())
	})
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)
	return a
}

func (a *fakeAgent) set(res, st string) {
	m := map[string]string{}
	for k, v := range a.state.Load().(map[string]string) {
		m[k] = v
	}
	m[res] = st
	a.state.Store(m)
}

func findHost(st stateOut, name string) hostOut {
	for _, h := range st.Hosts {
		if h.Name == name {
			return h
		}
	}
	return hostOut{}
}

func TestPollerStatesLostAndSince(t *testing.T) {
	a := newFakeAgent(t, false)
	a.skew = 30 * time.Second
	old := newFakeAgent(t, true)
	a.set("磁盘", "偏离") // 巡视台第一次看到时就已经不正常

	// 一个肯定连不上的端口
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	deadAddr := l.Addr().String()
	l.Close()

	hosts, _ := ParseInventory(strings.NewReader(
		"a " + a.srv.URL + "\nold " + old.srv.URL + "\ndead " + deadAddr + "\n"))
	p := NewPoller(2*time.Second, 4, "test")
	p.SetHosts(hosts)
	iv, lost := 15*time.Second, 45*time.Second
	ctx := context.Background()

	p.Round(ctx)
	st := p.Snapshot(time.Now(), iv, lost, Inventory{})
	ha, ho, hd := findHost(st, "a"), findHost(st, "old"), findHost(st, "dead")
	if ha.Status != "ok" || ha.API != 1 || ha.Agent != "v9" || len(ha.Rows) != 5 {
		t.Fatalf("a 应正常拉到：%+v", ha)
	}
	if ha.Skew < 28 || ha.Skew > 32 {
		t.Errorf("时钟偏差应约 +30s，实得 %v", ha.Skew)
	}
	if ha.Sev != "dev" || ha.Rows[2].St != "dev" || ha.Rows[2].Since == 0 || ha.Rows[2].SinceKnown {
		t.Errorf("磁盘一开始就偏离：起点只能标成'巡视台开始看时已是'：%+v", ha.Rows[2])
	}
	if ho.Status != "ok" || ho.API != 0 || len(ho.Rows) != 5 {
		t.Errorf("旧版应退回 /api/use 拉到：%+v", ho)
	}
	if hd.Status != "lost" || !hd.Never || len(hd.Rows) != 0 || !strings.Contains(hd.Err, "连接被拒绝") {
		t.Errorf("连不上的应是'从没拉到过'且说明原因：%+v", hd)
	}

	// 第二轮：CPU 变坏 → 起点是亲眼看到的
	a.set("CPU", "异常")
	p.Round(ctx)
	st = p.Snapshot(time.Now(), iv, lost, Inventory{})
	ha = findHost(st, "a")
	if ha.Sev != "bad" || !ha.Rows[0].SinceKnown || ha.Rows[0].Since == 0 {
		t.Errorf("CPU 在两轮之间变坏，起点应标为已知：%+v", ha.Rows[0])
	}
	// 偏离 → 异常不重置起点；恢复后起点清掉
	a.set("CPU", "正常")
	p.Round(ctx)
	st = p.Snapshot(time.Now(), iv, lost, Inventory{})
	if r := findHost(st, "a").Rows[0]; r.St != "ok" || r.Since != 0 {
		t.Errorf("恢复后起点应清掉：%+v", r)
	}

	// 对方开始报错：一次失败不算失联，但要带着原因
	a.down.Store(true)
	p.Round(ctx)
	st = p.Snapshot(time.Now(), iv, lost, Inventory{})
	ha = findHost(st, "a")
	if ha.Status != "ok" || ha.Err == "" {
		t.Errorf("一次失败不应算失联，但要写原因：%+v", ha)
	}
	// 超过 lost-after 没拉到：失联，且不能带着旧读数
	st = p.Snapshot(time.Now().Add(lost+time.Second), iv, lost, Inventory{})
	ha = findHost(st, "a")
	if ha.Status != "lost" || ha.Sev != "lost" || len(ha.Rows) != 0 || ha.Never {
		t.Errorf("失联的机器不能带旧读数：%+v", ha)
	}
}

// 对方少报一行、或报了认不出的状态，都是"无数据"，绝不是正常。
func TestMissingOrUnknownRowIsNotOK(t *testing.T) {
	a := newFakeAgent(t, false)
	a.set("网络", "维护中")
	hosts, _ := ParseInventory(strings.NewReader("a " + a.srv.URL + "\n"))
	p := NewPoller(2*time.Second, 1, "t")
	p.SetHosts(hosts)
	p.Round(context.Background())
	// 手动删掉一行，模拟对方少报
	p.mu.Lock()
	s := p.st["a"]
	s.doc.Rows = s.doc.Rows[:4]
	p.mu.Unlock()
	h := findHost(p.Snapshot(time.Now(), 15*time.Second, 45*time.Second, Inventory{}), "a")
	if h.Rows[3].St != "none" || h.Rows[4].St != "none" || h.Rows[4].State != "无数据" {
		t.Fatalf("网络/系统应是无数据：%+v %+v", h.Rows[3], h.Rows[4])
	}
	if h.Sev != "none" {
		t.Errorf("整台最坏应是 none，不能是 ok：%s", h.Sev)
	}
}

// 改清单（换机柜、加标签）不能让已有主机的状态清零。
func TestSetHostsKeepsState(t *testing.T) {
	a := newFakeAgent(t, false)
	hosts, _ := ParseInventory(strings.NewReader("a " + a.srv.URL + " rack=A1\n"))
	p := NewPoller(2*time.Second, 1, "t")
	p.SetHosts(hosts)
	p.Round(context.Background())
	hosts2, _ := ParseInventory(strings.NewReader("a " + a.srv.URL + " rack=B7\nb 127.0.0.1:1\n"))
	p.SetHosts(hosts2)
	st := p.Snapshot(time.Now(), 15*time.Second, 45*time.Second, Inventory{})
	if h := findHost(st, "a"); h.Status != "ok" || h.Rack != "B7" {
		t.Errorf("a 应保留状态并换到 B7：%+v", h)
	}
	if h := findHost(st, "b"); h.Status != "lost" || !h.Never {
		t.Errorf("新加的 b 在拉到之前是'从没拉到过'：%+v", h)
	}
}

func TestHealthLine(t *testing.T) {
	now := time.Unix(1000, 0)
	st := stateOut{Interval: 15, RoundAt: 990, Hosts: []hostOut{{Status: "ok", Sev: "ok"}, {Status: "lost"}, {Status: "ok", Sev: "bad"}}}
	if l := healthLine(st, now); !strings.HasPrefix(l, "ok hosts=3 ok=1 dev=0 bad=1 lost=1") {
		t.Errorf("%s", l)
	}
	st.RoundAt = 900
	if l := healthLine(st, now); !strings.HasPrefix(l, "crit") || !strings.Contains(l, "拉取停了") {
		t.Errorf("超过 3 轮没拉完应 crit：%s", l)
	}
	// 刚启动、第一轮还没拉完：不是 crit
	st2 := stateOut{Interval: 15, Started: 998}
	if l := healthLine(st2, now); !strings.HasPrefix(l, "starting") {
		t.Errorf("启动 2 秒、第一轮未完成应是 starting：%s", l)
	}
	st2.Started = 900
	if l := healthLine(st2, now); !strings.HasPrefix(l, "crit") {
		t.Errorf("启动 100 秒还没拉完一轮应 crit：%s", l)
	}
	st.RoundAt = 990
	st.Inventory.Errors = []string{"x"}
	if l := healthLine(st, now); !strings.HasPrefix(l, "warn") {
		t.Errorf("清单有错应 warn：%s", l)
	}
}

// 页面里不能留原型的模拟数据，也不能从外网加载任何东西（机房 iPad 多半上不了外网）。
func TestPageIsSelfContained(t *testing.T) {
	s := string(pageHTML)
	for _, bad := range []string{"sampleInventory", "tickSim", "示例数据", "https://", "http://"} {
		if strings.Contains(s, bad) {
			t.Errorf("页面里不应出现 %q", bad)
		}
	}
	for _, need := range []string{"api/state", "巡视台断开", "不代表正常"} {
		if !strings.Contains(s, need) {
			t.Errorf("页面里应有 %q", need)
		}
	}
}

// 契约：用一份真 nodedata 的 /api/fleet 输出（testdata，v5.22 采的）确认巡视台读得懂。
// nodedata 那边改了 UseRow 的字段名，这里就会失败。
func TestDecodeRealAgentOutput(t *testing.T) {
	b, err := os.ReadFile("testdata/agent_fleet_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var d agentDoc
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	if d.V != 1 || d.At == 0 || len(d.Rows) != 5 {
		t.Fatalf("v=%d at=%d rows=%d", d.V, d.At, len(d.Rows))
	}
	for i, r := range d.Rows {
		if r.Resource != resources[i] {
			t.Errorf("第 %d 行应是 %s，实得 %s", i, resources[i], r.Resource)
		}
		if stCode(r.State) == "none" && r.State != "无数据" {
			t.Errorf("%s 的状态 %q 认不出", r.Resource, r.State)
		}
	}
	if len(d.Rows[0].Facts) == 0 || d.Rows[0].Facts[0].Unit == "" || d.Rows[0].Facts[0].Label == "" {
		t.Errorf("CPU 行应带读数：%+v", d.Rows[0].Facts)
	}
}

// 仓库里给的样例清单必须能原样读通。
func TestExampleInventoryParses(t *testing.T) {
	f, err := os.Open("host.list.example")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	hs, errs := ParseInventory(f)
	if len(errs) > 0 || len(hs) != 5 {
		t.Fatalf("hosts=%d errs=%v", len(hs), errs)
	}
}
