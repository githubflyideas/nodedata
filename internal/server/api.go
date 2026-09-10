package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type QueryFns struct {
	Heatmap func(from, to time.Time) (*HeatmapJSON, error)
	Detail  func(ts time.Time) (interface{}, error)
	Raw     func(metricID string, from, to time.Time) (interface{}, error)

	// Window 按窗口名（1h/6h/24h/7d/30d）实时构造转储内容，
	// 用于 /data/*.json 在磁盘文件缺失/损坏时兜底，避免页面拿到 404。
	Window func(name string) (interface{}, error)
	// Health 实时构造 health.json，同样用于兜底。
	Health func() (interface{}, error)
	// Diagnosis 返回 L4 诊断链。
	Diagnosis func(zThreshold float64) (interface{}, error)
}

// MuxConfig 显式指定 web 根目录与 data 目录，避免依赖进程 cwd。
type MuxConfig struct {
	WebRoot string // 非空时从磁盘读 index.html（前端开发用），否则用 Index
	Index   []byte // 嵌入的首页
	DataDir string
	Version string

	// 基线钩子由 main 注入：server 不需要知道 check 包的存在。
	// 基线 = 用户把某一刻的累计计数器值认定为"正常状态"，
	// 之后 L0 只对超出基线的新增量告警。
	BaselineGet   func() interface{}
	BaselineSet   func(note string) (interface{}, error)
	BaselineClear func() error
}

func NewMux(webRoot string, qfns QueryFns) *http.ServeMux {
	return NewMuxWithConfig(MuxConfig{WebRoot: webRoot}, qfns)
}

func NewMuxWithConfig(cfg MuxConfig, qfns QueryFns) *http.ServeMux {
	webRoot := cfg.WebRoot
	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = "data"
	}

	mux := http.NewServeMux()

	// 只服务首页这一个文件。早期这里是 http.FileServer(http.Dir(webRoot))：
	// 带目录列表、监听 0.0.0.0，而 webRoot 找不到 index.html 时回落到当前目录 ——
	// 在 /root 下启动就能从网络上浏览 /root/.ssh。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/index.html":
		case "/l0.html": // 旧书签
			http.Redirect(w, r, "/", http.StatusMovedPermanently)
			return
		default:
			http.NotFound(w, r)
			return
		}
		page := cfg.Index
		if webRoot != "" {
			if b, err := os.ReadFile(filepath.Join(webRoot, "index.html")); err == nil {
				page = b
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(page)
	})

	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"version": cfg.Version})
	})

	// /api/baseline：GET 看当前基线，POST 把此刻状态定为基线，DELETE 清除。
	mux.HandleFunc("/api/baseline", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		switch r.Method {
		case http.MethodGet:
			if cfg.BaselineGet == nil {
				_ = enc.Encode(map[string]any{"baseline": nil})
				return
			}
			_ = enc.Encode(map[string]any{"baseline": cfg.BaselineGet()})
		case http.MethodPost:
			if cfg.BaselineSet == nil {
				http.Error(w, `{"error":"baseline not supported"}`, http.StatusNotImplemented)
				return
			}
			var body struct {
				Note string `json:"note"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body) // 空 body 合法
			b, err := cfg.BaselineSet(body.Note)
			if err != nil {
				http.Error(w, `{"error":`+strconv.Quote(err.Error())+`}`, http.StatusInternalServerError)
				return
			}
			_ = enc.Encode(map[string]any{"baseline": b})
		case http.MethodDelete:
			if cfg.BaselineClear == nil {
				http.Error(w, `{"error":"baseline not supported"}`, http.StatusNotImplemented)
				return
			}
			if err := cfg.BaselineClear(); err != nil {
				http.Error(w, `{"error":`+strconv.Quote(err.Error())+`}`, http.StatusInternalServerError)
				return
			}
			_ = enc.Encode(map[string]any{"baseline": nil})
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	})

	// /data/*.json：先读磁盘转储；缺失或非法 JSON 时用内存实时构造兜底。
	// 只要采集器在跑，页面就不会再出现 404 或空文件。
	mux.HandleFunc("/data/", func(w http.ResponseWriter, r *http.Request) {
		name := path.Base(r.URL.Path)
		if !strings.HasSuffix(name, ".json") {
			http.NotFound(w, r)
			return
		}
		if b, err := os.ReadFile(filepath.Join(dataDir, name)); err == nil && json.Valid(b) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Nodedata-Source", "dump")
			w.Write(b)
			return
		}
		stem := strings.TrimSuffix(name, ".json")
		var v interface{}
		var err error
		switch stem {
		case "1h", "6h", "24h", "7d", "30d":
			if qfns.Window == nil {
				http.NotFound(w, r)
				return
			}
			v, err = qfns.Window(stem)
		case "health":
			if qfns.Health == nil {
				http.NotFound(w, r)
				return
			}
			v, err = qfns.Health()
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("X-Nodedata-Source", "live")
		respondJSON(w, r, v)
	})

	mux.HandleFunc("/api/heatmap", func(w http.ResponseWriter, r *http.Request) {
		from, to, err := parseFromTo(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		data, err := qfns.Heatmap(from, to)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		respondJSON(w, r, data)
	})

	mux.HandleFunc("/api/detail", func(w http.ResponseWriter, r *http.Request) {
		tsStr := r.URL.Query().Get("ts")
		if tsStr == "" {
			http.Error(w, "missing ts", http.StatusBadRequest)
			return
		}
		tsUnix, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			http.Error(w, "invalid ts", http.StatusBadRequest)
			return
		}
		data, err := qfns.Detail(time.Unix(tsUnix, 0))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		respondJSON(w, r, data)
	})

	mux.HandleFunc("/api/raw", func(w http.ResponseWriter, r *http.Request) {
		metric := r.URL.Query().Get("metric")
		if metric == "" {
			http.Error(w, "missing metric", http.StatusBadRequest)
			return
		}
		from, to, err := parseFromTo(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		data, err := qfns.Raw(metric, from, to)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		respondJSON(w, r, data)
	})

	// L4 诊断链端点
	mux.HandleFunc("/api/diagnosis", func(w http.ResponseWriter, r *http.Request) {
		if qfns.Diagnosis == nil {
			http.Error(w, "diagnosis not configured", http.StatusNotImplemented)
			return
		}
		th := 3.0
		if v := r.URL.Query().Get("z"); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
				th = f
			}
		}
		data, err := qfns.Diagnosis(th)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		respondJSON(w, r, data)
	})

	// L0 检查端点
	mux.HandleFunc("/api/check", func(w http.ResponseWriter, r *http.Request) {
		checkPath := filepath.Join(dataDir, "check.json")
		data, err := os.ReadFile(checkPath)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"timestamp":"","categories":[],"exit_code":-1}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	})

	return mux
}

func ListenAndServe(addr string, handler http.Handler) error {
	return http.ListenAndServe(addr, handler)
}

func parseFromTo(r *http.Request) (time.Time, time.Time, error) {
	q := r.URL.Query()
	fromStr := q.Get("from")
	toStr := q.Get("to")
	var from, to time.Time
	var err error
	if fromStr != "" {
		fu, e := strconv.ParseInt(fromStr, 10, 64)
		if e != nil {
			return from, to, e
		}
		from = time.Unix(fu, 0)
		err = e
	}
	if toStr != "" {
		tu, e := strconv.ParseInt(toStr, 10, 64)
		if e != nil {
			return from, to, e
		}
		to = time.Unix(tu, 0)
	}
	if to.IsZero() {
		to = time.Now()
	}
	if from.IsZero() {
		from = to.Add(-time.Hour)
	}
	return from, to, err
}

func respondJSON(w http.ResponseWriter, r *http.Request, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.Encode(v)
}
