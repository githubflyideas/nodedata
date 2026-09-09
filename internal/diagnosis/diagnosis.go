// diagnosis.go — L4 诊断链。
// 基于 L0-L3 的结果，输出可操作的诊断建议。
package diagnosis

import (
	"fmt"
	"time"
)

// DiagnosisLevel 诊断级别：0=信息，1=警告，2=严重
type DiagnosisLevel int

const (
	Info     DiagnosisLevel = 0
	Warning  DiagnosisLevel = 1
	Critical DiagnosisLevel = 2
)

// DiagnosisItem 单条诊断
type DiagnosisItem struct {
	Level       DiagnosisLevel `json:"level"`
	Category    string         `json:"category"`       // 来源类别（Time/CPU/Memory等）
	Title       string         `json:"title"`          // 诊断标题
	Description string         `json:"description"`    // 详细描述
	Impact      string         `json:"impact"`         // 影响评估
	Action      string         `json:"action"`         // 建议动作
	Evidence    []string       `json:"evidence"`       // 证据（来自L0-L3）
	Timestamp   time.Time      `json:"timestamp"`
}

// DiagnosisChain 诊断链（多条诊断按优先级排序）
type DiagnosisChain struct {
	Items     []DiagnosisItem `json:"items"`
	Priority  DiagnosisLevel  `json:"priority"`  // 最高级别
	Count     int             `json:"count"`
	Timestamp time.Time       `json:"timestamp"`
}

// Diagnose 输入 L0-L3 数据，返回诊断链
func Diagnose(l0Result interface{}, l1Raw interface{}, l2Dump interface{}, l3Deviations interface{}) *DiagnosisChain {
	chain := &DiagnosisChain{
		Items:     []DiagnosisItem{},
		Timestamp: time.Now(),
	}

	// L0 状态诊断
	diagnoseL0(l0Result, chain)

	// L1-L3 趋势诊断
	diagnoseL1L3(l1Raw, l2Dump, l3Deviations, chain)

	// 计算优先级
	maxLevel := Info
	for _, item := range chain.Items {
		if item.Level > maxLevel {
			maxLevel = item.Level
		}
	}
	chain.Priority = maxLevel
	chain.Count = len(chain.Items)

	return chain
}

// diagnoseL0 基于 L0 检查结果诊断
func diagnoseL0(l0Result interface{}, chain *DiagnosisChain) {
	// 如果 L0 有失败项，立即生成关键诊断
	// 这里简化：实际应检查 l0Result 的具体字段

	// 示例：时间同步失败
	chain.Items = append(chain.Items, DiagnosisItem{
		Level:    Warning,
		Category: "Time/Sync",
		Title:    "System clock drift detected",
		Description: "NTP offset exceeds 100ms or NTP daemon not syncing",
		Impact:    "Affects distributed tracing, metrics correlation, and log ordering",
		Action:    "sudo systemctl restart ntp && ntpstat",
		Evidence:  []string{"L0: NTP sync check failed", "offset: 150ms"},
		Timestamp: time.Now(),
	})
}

// diagnoseL1L3 基于 L1 原始数据和 L3 偏离度诊断
func diagnoseL1L3(l1Raw, l2Dump, l3Deviations interface{}, chain *DiagnosisChain) {
	// 高偏离度诊断
	// 实际应从 l3Deviations 中提取异常的 lag/metric 组合

	// 示例：内存压力
	chain.Items = append(chain.Items, DiagnosisItem{
		Level:    Warning,
		Category: "Memory",
		Title:    "Memory pressure detected",
		Description: "Memory usage at 78% of total, swapiness > 30",
		Impact:    "Risk of OOM kill, potential latency spikes",
		Action:    "Check top -o %MEM | head -20; consider vertical scaling",
		Evidence:  []string{"L1: mem_used = 78%", "L3: deviation +2.5σ from 7d baseline"},
		Timestamp: time.Now(),
	})

	// 示例：IO异常
	chain.Items = append(chain.Items, DiagnosisItem{
		Level:    Critical,
		Category: "Disk/IO",
		Title:    "Disk IO saturation",
		Description: "Disk utilization at 95%, await > 50ms",
		Impact:    "Application latency degradation, potential data loss if no redundancy",
		Action:    "iostat -x 1; check for slow queries or backups; consider RAID upgrade",
		Evidence:  []string{"L1: disk_util = 95%", "L1: io_await = 52ms", "L3: deviation +4.2σ"},
		Timestamp: time.Now(),
	})
}

// String 返回诊断链的可读摘要
func (dc *DiagnosisChain) String() string {
	var s string
	for _, item := range dc.Items {
		level := "INFO"
		if item.Level == Warning {
			level = "WARN"
		} else if item.Level == Critical {
			level = "CRITICAL"
		}
		s += fmt.Sprintf("[%s] %s (%s)\n", level, item.Title, item.Category)
		s += fmt.Sprintf("  → %s\n", item.Action)
	}
	return s
}
