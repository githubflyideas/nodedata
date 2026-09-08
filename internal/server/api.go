// api.go — HTTP 服务：静态文件 + live API 端点
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type QueryFns struct {
	Heatmap func(from, to time.Time) (*HeatmapJSON, error)
	Detail  func(ts time.Time) (interface{}, error)
	Raw     func(metricID string, from, to time.Time) (interface{}, error)
}

type CheckRunner struct {
	interval time.Duration
	dataDir  string
	mu       sync.Mutex
	latest   []byte
	done     chan struct{}
}

func NewCheckRunner(interval time.Duration, dataDir string) *CheckRunner {
	return &CheckRunner{
		interval: interval,
		dataDir:  dataDir,
		done:     make(chan struct{}),
	}
}

func (cr *CheckRunner) Start() {
	go cr.run()
}

func (cr *CheckRunner) Stop() {
	close(cr.done)
}

func (cr *CheckRunner) run() {
	ticker := time.NewTicker(cr.interval)
	defer ticker.Stop()

	for {
		select {
		case <-cr.done:
			return
		case <-ticker.C:
			cr.readCheckFile()
		}
	}
}

func (cr *CheckRunner) readCheckFile() {
	checkPath := filepath.Join(cr.dataDir, "check.json")
	data, _ := os.ReadFile(checkPath)
	
	cr.mu.Lock()
	cr.latest = data
	cr.mu.Unlock()
}

func (cr *CheckRunner) GetLatest() []byte {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	return cr.latest
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

	return mux
}

func NewMuxWithCheck(webRoot, dataDir string) *http.ServeMux {
	mux := http.NewServeMux()

	fs := http.FileServer(http.Dir(webRoot))
	mux.Handle("/", fs)

	mux.HandleFunc("/api/check", func(w http.ResponseWriter, r *http.Request) {
		checkPath := filepath.Join(dataDir, "check.json")
		data, err := os.ReadFile(checkPath)
		if err != nil {
			data = []byte(`{"timestamp":"","status":"no data yet"}`)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	})

	return mux
}

func ListenAndServe(addr string, mux *http.ServeMux) error {
	return http.ListenAndServe(addr, mux)
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
