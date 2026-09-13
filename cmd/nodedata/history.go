// history.go — 长期层落盘：<data-dir>/history/YYYY-MM-DD.jsonl（UTC 日期），每 5 分钟追加一行。
//
//	{"t":1789079400,"v":{"cpu.user":218.2,"mem.free":2.07e9,...}}
//
// 为什么是这个格式：
//   - 只追加、不改写：每 5 分钟写一次 ~2KB，崩溃最多丢最后一行，半行在读取时跳过；
//   - 按天分文件：保留期到了直接删文件，不需要压缩/合并；
//   - 纯文本：出了问题 tail/grep/jq 就能看，不引入任何依赖。
//
// 规模：~85 条序列 × 288 行/天 × 14 天 ≈ 9MB 磁盘；启动时全量读回（约 4k 行）约 0.25s。
// 这条 5 分钟流也正是将来汇到中心 ClickHouse 的那一份（1000 节点 ≈ 280 行/秒）。
package main

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/githubflyideas/nodedata/internal/collector"
)

type histLine struct {
	T int64              `json:"t"`
	V map[string]float64 `json:"v"`
}

// History 管理长期层的落盘与回读。
type History struct {
	dir    string
	retain time.Duration

	mu  sync.Mutex
	f   *os.File
	day string
}

func NewHistory(dir string, retain time.Duration) (*History, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &History{dir: dir, retain: retain}, nil
}

func dayOf(t time.Time) string { return t.UTC().Format("2006-01-02") }

// Load 把保留期内的历史装回 s 的长期层，返回读到的行数与点数。
func (h *History) Load(s *Series, now time.Time) (lines, points int, err error) {
	files, err := h.files()
	if err != nil {
		return 0, 0, err
	}
	oldest := dayOf(now.Add(-h.retain))
	for _, name := range files { // 文件名即日期，字典序 = 时间序
		if strings.TrimSuffix(name, ".jsonl") < oldest {
			continue
		}
		f, err := os.Open(filepath.Join(h.dir, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			var hl histLine
			if json.Unmarshal(sc.Bytes(), &hl) != nil || hl.T == 0 {
				continue // 崩溃留下的半行
			}
			ts := time.Unix(hl.T, 0)
			batch := make([]collector.Sample, 0, len(hl.V))
			for id, v := range hl.V {
				batch = append(batch, collector.Sample{MetricID: id, TS: ts, Value: v})
			}
			s.AddCoarse(batch)
			lines++
			points += len(batch)
		}
		f.Close()
	}
	return lines, points, nil
}

// Append 落盘一批长期层样本：按时间戳分组，每个时刻一行，但只做一次写系统调用。
//
// 必须按各自的时间戳分组，不能拿第一条的时间戳给整批打标：
// 一批里的样本可能相差一个长期层周期（5 分钟），而短档位配对的容差只有 30 秒，
// 错标时间戳会让这些点永远配不上对。
func (h *History) Append(samples []collector.Sample) error {
	if len(samples) == 0 {
		return nil
	}
	byTS := make(map[int64]map[string]float64, 4)
	var order []int64
	for _, sm := range samples {
		if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) { // JSON 表示不了
			continue
		}
		t := sm.TS.Unix()
		m, ok := byTS[t]
		if !ok {
			m = make(map[string]float64, len(samples))
			byTS[t] = m
			order = append(order, t)
		}
		m[sm.MetricID] = sm.Value
	}
	if len(order) == 0 {
		return nil
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })

	var buf []byte
	for _, t := range order {
		b, err := json.Marshal(histLine{T: t, V: byTS[t]})
		if err != nil {
			continue
		}
		buf = append(append(buf, b...), '\n')
	}
	if len(buf) == 0 {
		return nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if d := dayOf(time.Unix(order[0], 0)); d != h.day || h.f == nil {
		if h.f != nil {
			h.f.Close()
		}
		f, err := os.OpenFile(filepath.Join(h.dir, d+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			h.f = nil
			return err
		}
		h.f, h.day = f, d
		h.expire(time.Unix(order[0], 0))
	}
	_, err := h.f.Write(buf) // 一次 write 写完整批
	return err
}

// expire 删除保留期外的日文件。
func (h *History) expire(now time.Time) {
	files, _ := h.files()
	oldest := dayOf(now.Add(-h.retain))
	for _, name := range files {
		if strings.TrimSuffix(name, ".jsonl") < oldest {
			os.Remove(filepath.Join(h.dir, name))
		}
	}
}

func (h *History) files() ([]string, error) {
	ents, err := os.ReadDir(h.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if !e.IsDir() && strings.HasSuffix(n, ".jsonl") && len(n) == len("2006-01-02.jsonl") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (h *History) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.f != nil {
		h.f.Close()
		h.f = nil
	}
}
