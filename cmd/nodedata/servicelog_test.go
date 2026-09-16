package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
)

func svc(name string, pid int, start int64, ports ...int) collector.Service {
	return collector.Service{Name: name, Exe: name, PID: pid, StartTS: start, Ports: ports, Instances: 1}
}

// 走完一台机器的真实经历：装上 → 观察 → MySQL 停掉 → Redis 重启 → nodedata 自己重启。
func TestServiceLogLifecycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.jsonl")
	day0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	l := NewServiceLog(path, 14*24*time.Hour)
	if _, err := l.Load(day0); err != nil {
		t.Fatal(err)
	}
	mysql := svc("MySQL", 1200, day0.Add(-72*time.Hour).Unix(), 3306)
	redis := svc("Redis", 4021, day0.Add(-48*time.Hour).Unix(), 6379)
	// 学习期内：建立基线，不报"出现"。连续看见 minSeenRounds 轮才进表。
	for i := 0; i < minSeenRounds; i++ {
		if ev := l.Update([]collector.Service{mysql, redis}, day0.Add(time.Duration(i)*time.Minute)); len(ev) != 0 {
			t.Fatalf("学习期内不该产生事件: %+v", ev)
		}
	}
	if len(l.Current()) != 2 {
		t.Fatalf("基线应有 2 个服务")
	}

	// 第 2 天：Nginx 新装（启动时刻取 minServiceAge 之前，否则会被"短命命令"规则挡掉）
	d2 := day0.Add(48 * time.Hour)
	nginx := svc("Nginx", 890, d2.Add(-30*time.Minute).Unix(), 80, 443)
	var ev []svcEvent
	for i := 0; i < minSeenRounds; i++ {
		ev = l.Update([]collector.Service{mysql, redis, nginx}, d2.Add(time.Duration(i)*time.Minute))
	}
	if len(ev) != 1 || ev[0].Kind != evAppear || ev[0].Name != "Nginx" {
		t.Fatalf("应报 Nginx 出现: %+v", ev)
	}

	// 第 3 天：MySQL 停掉 —— 要连续 3 轮才确认，一轮抖动不算
	d3 := day0.Add(72 * time.Hour)
	for i := 0; i < vanishConfirm-1; i++ {
		if ev := l.Update([]collector.Service{redis, nginx}, d3); len(ev) != 0 {
			t.Fatalf("第 %d 轮就报消失，太急: %+v", i+1, ev)
		}
	}
	ev = l.Update([]collector.Service{redis, nginx}, d3)
	if len(ev) != 1 || ev[0].Kind != evVanish || ev[0].Name != "MySQL" {
		t.Fatalf("第 3 轮应确认 MySQL 消失: %+v", ev)
	}

	// 第 4 天：Redis 重启（名字在、starttime 变）
	d4 := day0.Add(96 * time.Hour)
	redis2 := svc("Redis", 9999, d4.Add(-30*time.Minute).Unix(), 6379)
	ev = l.Update([]collector.Service{redis2, nginx}, d4)
	if len(ev) != 1 || ev[0].Kind != evRestart {
		t.Fatalf("应报 Redis 重启: %+v", ev)
	}

	// 历史查询：站在第 5 天回看
	d5 := day0.Add(120 * time.Hour)
	for _, tc := range []struct {
		id   string
		at   time.Time
		want string
	}{
		{"MySQL:3306", day0.Add(24 * time.Hour), stYes},       // 那时在
		{"MySQL:3306", d5, stNo},                              // 现在不在
		{"Nginx:80,443", day0.Add(24 * time.Hour), stUnknown}, // 那时还没装过，不能说"不在"
		{"Nginx:80,443", d5, stYes},
		{"Redis:6379", d5, stYes},
	} {
		if got := l.StateAt(tc.id, tc.at); got != tc.want {
			t.Errorf("StateAt(%s, %v) = %s, want %s", tc.id, tc.at.Format("01-02"), got, tc.want)
		}
	}
	// 重启标记
	if !l.RestartedSince("Redis:6379", day0.Add(72*time.Hour)) {
		t.Errorf("Redis 在第 4 天重启过，应标出来")
	}
	// 消失的服务必须留在表里
	van := l.Vanished()
	if len(van) != 1 || van[0].Name != "MySQL" {
		t.Fatalf("消失的服务必须可见: %+v", van)
	}

	// ── nodedata 自己停一天再起来
	l.Close(d5)
	l2 := NewServiceLog(path, 14*24*time.Hour)
	d6 := day0.Add(144 * time.Hour)
	n, err := l2.Load(d6)
	if err != nil || n == 0 {
		t.Fatalf("历史应能读回: n=%d err=%v", n, err)
	}
	// 停机那段时间是"不知道"，绝不能说成"服务不在"
	gap := d5.Add(12 * time.Hour)
	for _, id := range []string{"Redis:6379", "Nginx:80,443", "MySQL:3306"} {
		if got := l2.StateAt(id, gap); got != stUnknown {
			t.Errorf("nodedata 没在跑的时段，%s 应是 unknown，got %s", id, got)
		}
	}
	// 重启之前的历史还在
	if got := l2.StateAt("MySQL:3306", day0.Add(24*time.Hour)); got != stYes {
		t.Errorf("重启后历史丢了: MySQL 第 1 天应为 yes，got %s", got)
	}
}

// 保留期外的事件要被裁掉，文件不能无限增长。
func TestServiceLogExpiry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.jsonl")
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)
	var lines []byte
	for _, e := range []string{
		`{"ts":` + itoa64(old.Unix()) + `,"k":"+","id":"Old:1","name":"Old"}`,
		`{"ts":` + itoa64(now.Add(-time.Hour).Unix()) + `,"k":"+","id":"New:2","name":"New"}`,
		`{"ts":123,"k":`, // 崩溃留下的半行
	} {
		lines = append(lines, []byte(e+"\n")...)
	}
	os.WriteFile(path, lines, 0o644)

	l := NewServiceLog(path, 14*24*time.Hour)
	if _, err := l.Load(now); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if c := string(b); contains(c, "Old:1") {
		t.Errorf("保留期外的事件应被裁掉: %s", c)
	} else if !contains(c, "New:2") {
		t.Errorf("保留期内的事件不能丢: %s", c)
	}
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// 判据是"我们连续看见它多久"，不是"它自己活了多久"。这个测试锁住三件事：
// 短命命令挡掉、崩溃循环的服务必须进表、无端口的常驻进程不能漏。
func TestLedgerAdmission(t *testing.T) {
	l := NewServiceLog(t.TempDir()+"/s.jsonl", 7*24*time.Hour)
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	l.Load(now)
	l.startAt = now.Add(-time.Hour) // 跳过学习期

	tick := func(i int, svcs ...collector.Service) []svcEvent {
		return l.Update(svcs, now.Add(time.Duration(i)*time.Minute))
	}
	mysql := collector.Service{Name: "MySQL", Exe: "mysqld", PID: 1, Ports: []int{3306},
		StartTS: now.Add(-24 * time.Hour).Unix()}
	// 无端口、非 systemd 的常驻工作进程：真实生产负载，不能漏
	worker := collector.Service{Name: "etl-worker", Exe: "etl-worker", PID: 7,
		StartTS: now.Add(-6 * time.Hour).Unix()}
	// 跑三秒的命令：只会被看见一轮
	aptGet := collector.Service{Name: "apt-get", Exe: "apt-get", PID: 2, StartTS: now.Unix()}

	tick(0, mysql, worker, aptGet)
	tick(1, mysql, worker)
	if len(l.Current()) != 0 {
		t.Fatalf("不足 %d 轮时不该进表：%+v", minSeenRounds, l.Current())
	}
	tick(2, mysql, worker)
	names := map[string]bool{}
	for _, s := range l.Current() {
		names[s.Name] = true
	}
	if !names["MySQL"] || !names["etl-worker"] {
		t.Fatalf("连续看见 %d 轮后应进表：%+v", minSeenRounds, l.Current())
	}
	if names["apt-get"] {
		t.Fatalf("只被看见一轮的短命命令不该进表")
	}

	// 崩溃循环：每 2 分钟重启一次，自身 uptime 永远很短，但身份不变、每轮都看得见。
	// 用"它自己活了多久"做判据会把这类永远挡在表外——而这恰恰最该被看见。
	crashy := collector.Service{Name: "flaky", Exe: "flaky", PID: 100, Ports: []int{9000},
		StartTS: now.Add(3 * time.Minute).Unix()}
	for i := 3; i <= 5; i++ {
		crashy.PID += 1
		crashy.StartTS = now.Add(time.Duration(i) * time.Minute).Unix() // 每轮都是新进程
		tick(i, mysql, worker, crashy)
	}
	found := false
	for _, s := range l.Current() {
		found = found || s.Name == "flaky"
	}
	if !found {
		t.Fatalf("崩溃循环的服务必须进表：%+v", l.Current())
	}
	// 而且重启要被记为事件
	ev := tick(6, mysql, worker, collector.Service{Name: "flaky", Exe: "flaky", PID: 999,
		Ports: []int{9000}, StartTS: now.Add(6 * time.Minute).Unix()})
	restart := false
	for _, e := range ev {
		restart = restart || (e.Kind == evRestart && e.Name == "flaky")
	}
	if !restart {
		t.Fatalf("崩溃循环应持续产生重启事件：%+v", ev)
	}
}
