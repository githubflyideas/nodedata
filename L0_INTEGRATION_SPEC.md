# L0 Web Integration Specification

## 完成状态

✅ **已实现的组件**

### 1. CheckRunner 后台执行 (check_bg.go)
```go
type CheckRunner struct {
    interval   time.Duration    // 5 秒
    outDir     string           // data/ 目录
    ticker     *time.Ticker     // 定时器
    lastCheck  *FullResult      // 最新结果
}

Methods:
- Start()      // 启动后台 goroutine
- Stop()       // 停止运行
- GetLatest()  // 获取最新结果
```

**特点**:
- 原子写入 data/check.json（temp + rename）
- 线程安全：使用 sync.Mutex
- 立即运行第一次，然后按间隔重复

### 2. API 端点 (api.go)
```
GET /api/check → data/check.json content
```

**返回格式** (JSON):
```json
{
  "Categories": [
    {
      "Name": "Time/Sync",
      "Checks": [
        {
          "ID": "T01",
          "Name": "NTP synchronized",
          "Level": 0,
          "Msg": "NTP not detected"
        }
      ],
      "Level": 1,
      "Passed": 2,
      "Total": 3
    },
    ...
  ],
  "Timestamp": "2026-09-08T11:20:30Z"
}
```

**Status codes**:
- 200: Success (JSON returned)
- 200: No data (empty result returned)

### 3. Web UI 面板 (index.html)
**显示内容**:
- 标题：🔍 nodedata L0 Sanity Check
- 检查结果：✓/⚠/✗ 符号 + 类别名 + 状态
- 详情：失败/警告项的说明
- 时间戳：最后更新时间
- 刷新按钮：手动刷新

**实时更新**:
- 初始加载：pageload
- 自动轮询：每 5 秒
- 手动刷新：点击按钮

**样式**:
- 绿色边框：全过 (pass)
- 橙色边框：仅警告 (warn)
- 红色边框：有失败 (fail)

## 整合点

### main.go 需要添加

在启动代码中（假设你有现有的 server/collector 启动代码）：

```go
package main

import (
    "github.com/githubflyideas/nodedata/cmd/nodedata/check"
)

func main() {
    // ... existing flag parsing ...
    
    // 1. Start L0 check runner
    checkRunner := NewCheckRunner(5*time.Second, *webDir)
    checkRunner.Start()
    defer checkRunner.Stop()
    
    // 2. Pass to server (or integrate into dump loop)
    // If using file-based approach:
    //   → checkRunner writes to data/check.json
    //   → API serves from that file
    
    // 3. Start HTTP server with /api/check endpoint
    mux := server.NewMuxWithCheck(*webDir, queryFns)
    listener, _ := net.Listen("tcp", *addr)
    http.Serve(listener, mux)
}
```

### api.go 需要添加

```go
package server

// CheckHandler 服务 /api/check
func CheckHandler(webRoot string) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodGet {
            http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
            return
        }
        
        checkFile := filepath.Join(webRoot, "data", "check.json")
        data, _ := os.ReadFile(checkFile)
        
        w.Header().Set("Content-Type", "application/json")
        if data != nil {
            w.Write(data)
        } else {
            w.Write([]byte(`{"Categories":null,"Timestamp":"2026-01-01T00:00:00Z"}`))
        }
    }
}

// 或使用 NewMuxWithCheck()
func NewMuxWithCheck(webRoot string, qfns QueryFns) *http.ServeMux {
    mux := NewMux(webRoot, qfns)
    mux.HandleFunc("/api/check", CheckHandler(webRoot))
    return mux
}
```

## 文件结构

```
cmd/nodedata/
  main.go          # 启动代码
  check_bg.go      # ✅ CheckRunner (新)
  check/
    check.go       # L0 逻辑

internal/server/
  api.go           # ✅ CheckHandler 已添加
  dump.go          # 保留

index.html         # ✅ Web panel (新)

data/
  check.json       # ✅ 检查结果 (自动生成)
```

## 时间流

```
T=0s:   checkRunner.Start() 
        → 立即运行 RunAllChecks()
        → 写入 data/check.json
        
T=0s+:  前端 /api/check 请求
        → 返回 data/check.json 内容
        → 显示在面板
        
T=5s:   定时器触发
        → 再次运行 RunAllChecks()
        → 更新 data/check.json
        
T=5s+:  前端轮询（无需等待，实时）
        → 读取更新的结果
        → 更新面板显示
        
（循环）
```

## 运行示例

启动：
```bash
./nodedata run -web . -addr 0.0.0.0:8888
```

访问：
```
http://localhost:8888/
```

看到：
```
🔍 nodedata L0 Sanity Check

✓ Time/Sync:         OK (3/3)
✓ CPU:               OK (7/7)
✓ Memory:            OK (8/8)
✓ Disk/IO:           OK (8/8)
✓ Network:           WARN (5/6)
  ⚠ N02: min=1400 (<1500)
✓ Filesystem:        OK (3/3)
✓ Conntrack:         OK (2/2)
✓ Socket:            OK (2/2)
✓ Errors:            OK (2/2)

✓ All checks passed

Last update: 11:20:30 AM [⟲ Refresh]
```

## 接下来的步骤

1. **集成到现有 main.go**
   - 添加 CheckRunner 启动代码
   - 确保 data/ 目录创建

2. **集成到现有 API 路由**
   - 注册 /api/check handler
   - 测试 GET 请求

3. **编译测试**
   ```bash
   go build -o nodedata ./cmd/nodedata
   mkdir -p data
   ./nodedata run -web .
   ```

4. **浏览器测试**
   - 访问 http://localhost:8888
   - 观察面板实时更新

