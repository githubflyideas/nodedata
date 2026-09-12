// dump.go — 静态转储：每30秒把常用视图写为 data/*.json（原子 rename）。
package server

import (
	"encoding/json"
	"fmt"
	"github.com/githubflyideas/nodedata/internal/deviation"
	"os"
	"path/filepath"
	"time"
)

// HeatmapJSON 是 §8.3 的响应格式。
type HeatmapJSON struct {
	Host        string          `json:"host"`
	From        int64           `json:"from"`
	To          int64           `json:"to"`
	Resolution  int             `json:"resolution"`
	GeneratedAt int64           `json:"generated_at"`
	Degraded    map[string]bool `json:"degraded"`
	Lags        []string        `json:"lags"`
	LagSeconds  []int           `json:"lag_seconds"`
	LagReady    []bool          `json:"lag_ready"`
	LowConf     []string        `json:"low_confidence"`
	PSIAlerts   []PSIAlert      `json:"psi_alerts"`
	Metrics     []MetricPoints  `json:"metrics"`
	Rules       []RuleHit       `json:"rules"`
	// Procs 是最近一轮的进程 CPU 快照（前 N + nodedata 自身），把整机偏离落到 PID。
	Procs   []ProcTop `json:"procs"`
	ProcsTS int64     `json:"procs_ts,omitempty"`
}

// ProcTop 与 collector.ProcTop 同构（server 包不依赖 collector）。
type ProcTop struct {
	PID  int     `json:"pid"`
	Comm string  `json:"comm"`
	Key  string  `json:"key"`
	CPU  float64 `json:"cpu"`
	RSS  uint64  `json:"rss"`
	Self bool    `json:"self,omitempty"`

	ReadBps    float64 `json:"read_bps"`
	WriteBps   float64 `json:"write_bps"`
	MajFlt     float64 `json:"majflt"`
	RSSGrowth  int64   `json:"rss_growth"`
	GrowthSpan int     `json:"growth_span"`
	State      string  `json:"state"`
}

type PSIAlert struct {
	TS        int64   `json:"ts"`
	Metric    string  `json:"metric"`
	Value     float64 `json:"value"`
	Threshold float64 `json:"threshold"`
}

type MetricPoints struct {
	MetricID  string  `json:"metric_id"`
	Domain    string  `json:"domain"`
	Unit      string  `json:"unit"`
	IsPrimary int     `json:"is_primary"`
	Points    []Point `json:"points"`
}

type Point struct {
	TS int64   `json:"ts"`
	V  float64 `json:"v"`
	// Z 每档一个值，nil = 该档未就绪。长度是 deviation.NLag。
	Z        [deviation.NLag]*int8 `json:"z"`
	OnsetLag *string               `json:"onset_lag"`
	Breadth  int                   `json:"breadth"`
}

type RuleHit struct {
	TS         int64          `json:"ts"`
	Domains    []string       `json:"domains"`
	Cascade    bool           `json:"cascade"`
	MaxBreadth int            `json:"max_breadth"`
	Hits       map[string]int `json:"hits"`
}

// HealthJSON 是 data/health.json 格式。
type HealthJSON struct {
	Host        string             `json:"host"`
	GeneratedAt int64              `json:"generated_at"`
	Health      map[string]float64 `json:"health"`
	Degraded    map[string]bool    `json:"degraded"`
}

// Dumper 负责周期性写静态 JSON 文件。
type Dumper struct {
	webRoot  string
	dataDir  string
	queryFn  func(from, to time.Time) (*HeatmapJSON, error)
	healthFn func() HealthJSON
}

// NewDumper 创建 Dumper。
func NewDumper(webRoot string,
	queryFn func(from, to time.Time) (*HeatmapJSON, error),
	healthFn func() HealthJSON) *Dumper {
	return &Dumper{webRoot: webRoot, queryFn: queryFn, healthFn: healthFn}
}

// SetDataDir 显式指定转储目录，覆盖默认的 webRoot/data。
func (d *Dumper) SetDataDir(dir string) { d.dataDir = dir }

// dir 返回实际转储目录。
func (d *Dumper) dir() string {
	if d.dataDir != "" {
		return d.dataDir
	}
	return filepath.Join(d.webRoot, "data")
}

// DumpAll 写出全部视图文件。
func (d *Dumper) DumpAll() error {
	now := time.Now()
	var firstErr error
	windows := []struct {
		name string
		dur  time.Duration
	}{
		{"1h", time.Hour},
		{"6h", 6 * time.Hour},
		{"24h", 24 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"14d", 14 * 24 * time.Hour},
	}

	if err := os.MkdirAll(d.dir(), 0755); err != nil {
		return err
	}

	for _, w := range windows {
		from := now.Add(-w.dur)
		data, err := d.queryFn(from, now)
		if err != nil {
			continue
		}
		if err := writeAtomicJSON(filepath.Join(d.dir(), w.name+".json"), data); err != nil {
			return fmt.Errorf("dump %s: %w", w.name, err)
		}
	}

	if firstErr != nil {
		return firstErr
	}

	// health.json
	h := d.healthFn()
	if err := writeAtomicJSON(filepath.Join(d.dir(), "health.json"), h); err != nil {
		return err
	}

	return nil
}

// RunLoop 周期性调用 DumpAll，间隔 interval。
func (d *Dumper) RunLoop(interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			d.DumpAll()
		}
	}
}

// writeAtomicJSON 以原子方式写纯 JSON 到 path（tmp + rename）。
// Caddy 的 encode gzip 指令会在传输时动态压缩，无需磁盘预压缩。
func writeAtomicJSON(path string, v interface{}) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(v); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
