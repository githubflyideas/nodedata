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
	interval  time.Duration
	dataDir   string
	mu        sync.Mutex
	latest    check.FullResult
	writeFile bool // 是否把结果落盘（默认否，见 runCheck）
	done      chan struct{}
}

// NewBackgroundCheckRunner：writeFile 为真时才把每轮结果写成 data/check.json。
func NewBackgroundCheckRunner(interval time.Duration, dataDir string, writeFile bool) *BackgroundCheckRunner {
	return &BackgroundCheckRunner{
		interval:  interval,
		dataDir:   dataDir,
		writeFile: writeFile,
		done:      make(chan struct{}),
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
	// 默认不落盘：L0 每个采集周期跑一次，7.4KB 的 check.json 无条件重写就是每天上百 MB
	// 的写入（判定文案里嵌着"已用 52.15%""刚启动 324 秒"这类活数字，每次都变，
	// 按内容去重也拦不住）。结果已经在 bcr.latest 里，页面与 L4 都从内存读。
	// 只有显式开启转储时才写文件，供离线查看。
	if !bcr.writeFile {
		return
	}
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return
	}
	checkPath := filepath.Join(bcr.dataDir, "check.json")
	tmpPath := checkPath + ".tmp"
	if os.WriteFile(tmpPath, data, 0644) == nil {
		os.Rename(tmpPath, checkPath)
	}
}

func (bcr *BackgroundCheckRunner) GetLatest() check.FullResult {
	bcr.mu.Lock()
	defer bcr.mu.Unlock()
	return bcr.latest
}
