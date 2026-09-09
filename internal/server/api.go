package server

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type QueryFns struct {
	Heatmap func(from, to time.Time) (*HeatmapJSON, error)
	Detail  func(ts time.Time) (interface{}, error)
	Raw     func(metricID string, from, to time.Time) (interface{}, error)
}

func NewMux(webRoot string, qfns QueryFns) *http.ServeMux {
	mux := http.NewServeMux()

	fs := http.FileServer(http.Dir(webRoot))
	mux.Handle("/", fs)

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

	// L0 检查端点
	mux.HandleFunc("/api/check", func(w http.ResponseWriter, r *http.Request) {
		checkPath := filepath.Join(webRoot, "data", "check.json")
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

// 让 mux 支持 ListenAndServe 方法
func (m *http.ServeMux) ListenAndServe(addr string, handler http.Handler) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return http.Serve(listener, handler)
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
