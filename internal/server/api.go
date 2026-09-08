package server

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type HeatmapJSON struct {
	Data interface{} `json:"data"`
}

// CheckRunner 后台定时执行 L0 检查
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

	// 首次立即运行
	cr.runCheck()

	for {
		select {
		case <-cr.done:
			return
		case <-ticker.C:
			cr.runCheck()
		}
	}
}

func (cr *CheckRunner) runCheck() {
	// 简单的 L0 检查（演示）
	result := map[string]interface{}{
		"timestamp": time.Now(),
		"status":    "ok",
		"checks": map[string]string{
			"cpu":     "pass",
			"memory":  "pass",
			"disk":    "pass",
			"network": "pass",
		},
	}

	data, _ := json.MarshalIndent(result, "", "  ")
	cr.mu.Lock()
	cr.latest = data
	cr.mu.Unlock()

	// 写到文件
	checkPath := filepath.Join(cr.dataDir, "check.json")
	tmpPath := checkPath + ".tmp"
	io.WriteFile(tmpPath, data, 0644)
	os.Rename(tmpPath, checkPath)
}

func (cr *CheckRunner) GetLatest() []byte {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	return cr.latest
}

func CheckHandler(w http.ResponseWriter, r *http.Request, dataDir string) {
	checkPath := filepath.Join(dataDir, "check.json")
	data, err := os.ReadFile(checkPath)
	if err != nil {
		data = []byte(`{"timestamp":"","status":"no data yet"}`)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func NewMuxWithCheck(webRoot, dataDir string) *http.ServeMux {
	mux := http.NewServeMux()

	// 静态文件服务
	fs := http.FileServer(http.Dir(webRoot))
	mux.Handle("/", fs)

	// /api/check — L0 状态
	mux.HandleFunc("/api/check", func(w http.ResponseWriter, r *http.Request) {
		CheckHandler(w, r, dataDir)
	})

	return mux
}

func ListenAndServe(addr string, mux *http.ServeMux) error {
	return http.ListenAndServe(addr, mux)
}

func respondJSON(w http.ResponseWriter, r *http.Request, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func parseFromTo(r *http.Request) (time.Time, time.Time, error) {
	from := time.Now().Add(-1 * time.Hour)
	to := time.Now()
	return from, to, nil
}
