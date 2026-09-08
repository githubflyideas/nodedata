package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/githubflyideas/nodedata/cmd/nodedata/check"
)

type BackgroundCheckRunner struct {
	interval time.Duration
	dataDir  string
	mu       sync.Mutex
	latest   check.FullResult
	done     chan struct{}
}

func NewBackgroundCheckRunner(interval time.Duration, dataDir string) *BackgroundCheckRunner {
	return &BackgroundCheckRunner{
		interval: interval,
		dataDir:  dataDir,
		done:     make(chan struct{}),
	}
}

func (bcr *BackgroundCheckRunner) Start() {
	go bcr.run()
}

func (bcr *BackgroundCheckRunner) Stop() {
	close(bcr.done)
}

func (bcr *BackgroundCheckRunner) run() {
	ticker := time.NewTicker(bcr.interval)
	defer ticker.Stop()
	bcr.runCheck()
	for {
		select {
		case <-bcr.done:
			return
		case <-ticker.C:
			bcr.runCheck()
		}
	}
}

func (bcr *BackgroundCheckRunner) runCheck() {
	results, _ := check.Run(fmt.Sprintf("%v", bcr.interval))
	bcr.mu.Lock()
	bcr.latest = results
	bcr.mu.Unlock()
	checkPath := filepath.Join(bcr.dataDir, "check.json")
	tmpPath := checkPath + ".tmp"
	data, _ := json.MarshalIndent(results, "", "  ")
	os.WriteFile(tmpPath, data, 0644)
	os.Rename(tmpPath, checkPath)
}

func (bcr *BackgroundCheckRunner) GetLatest() check.FullResult {
	bcr.mu.Lock()
	defer bcr.mu.Unlock()
	return bcr.latest
}
