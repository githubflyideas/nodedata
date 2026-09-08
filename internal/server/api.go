// api.go — HTTP 服务：静态文件 + live API 端点。
package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// QueryFns 是 live 端点所需的查询函数集合，由外部注入。
type QueryFns struct {
	Heatmap func(from, to time.Time) (*HeatmapJSON, error)
	Detail  func(ts time.Time) (interface{}, error)
	Raw     func(metricID string, from, to time.Time) (interface{}, error)
}

// NewMux 构建 HTTP 路由。webRoot 是静态文件根目录。
func NewMux(webRoot string, qfns QueryFns) *http.ServeMux {
	mux := http.NewServeMux()

	// 静态文件（含 gzip 自动支持）
	fs := http.FileServer(http.Dir(webRoot))
	mux.Handle("/", fs)

	// live 端点
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

	return mux
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
	// 若客户端支持 gzip，可在此压缩——此处简化为不压缩（静态文件已 gzip）
	enc := json.NewEncoder(w)
	enc.Encode(v)
}

// CheckHandler 返回最新的 L0 检查结果（从 data/check.json）
func CheckHandler(webRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Try to read check.json
		import (
			"os"
			"path/filepath"
		)
		checkFile := filepath.Join(webRoot, "data", "check.json")
		data, err := os.ReadFile(checkFile)
		if err != nil {
			// Return empty result if file doesn't exist yet
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"Categories":null,"Timestamp":"2026-01-01T00:00:00Z"}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	}
}

// NewMuxWithCheck 构建 HTTP 路由（含 /api/check 端点）
func NewMuxWithCheck(webRoot string, qfns QueryFns) *http.ServeMux {
	mux := NewMux(webRoot, qfns)
	mux.HandleFunc("/api/check", CheckHandler(webRoot))
	return mux
}
