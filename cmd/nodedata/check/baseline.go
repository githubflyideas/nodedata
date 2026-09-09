// baseline.go — 累计型计数器的"增量判定"与人工基线。
//
// 为什么需要它：conntrack drop、listen 溢出、OOM 击杀、内核 taint 这些量都是
// 自开机起单调累加/置位的。机器三个月前抖过一次，counter 就永远不回零，页面于是
// 永远挂着一条告警——"曾经发生过" 被当成 "现在有问题"，噪音把真问题盖住。
//
// 正确的判定对象是增量，不是绝对值：
//   - 有人工基线时，只对超过基线的新增量告警（基线 = 用户说"此刻这样就算正常"）；
//   - 没有基线时，用本进程第一次观测值当隐式基线，也就是只对启动之后的新增告警；
//   - 累计值仍然照实显示，只是不再单独构成告警。
package check

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Baseline 是"把当时状态当作标准状态"的一次快照。
type Baseline struct {
	At   time.Time          `json:"at"`
	Note string             `json:"note,omitempty"`
	Vals map[string]float64 `json:"vals"`
}

var (
	blMu  sync.RWMutex
	bl    *Baseline
	first = map[string]float64{} // 本进程首次观测值（隐式基线）
)

// SetBaseline 应用一份基线；传 nil 表示清除。
func SetBaseline(b *Baseline) {
	blMu.Lock()
	defer blMu.Unlock()
	bl = b
}

// CurrentBaseline 返回当前基线的副本，没有则返回 nil。
func CurrentBaseline() *Baseline {
	blMu.RLock()
	defer blMu.RUnlock()
	if bl == nil {
		return nil
	}
	c := Baseline{At: bl.At, Note: bl.Note, Vals: make(map[string]float64, len(bl.Vals))}
	for k, v := range bl.Vals {
		c.Vals[k] = v
	}
	return &c
}

// baseOf 返回某计数器的判定起点，以及这个起点的人类可读说明。
// 优先用人工基线；否则用本进程首次观测值。
func baseOf(key string, cur float64) (float64, string) {
	blMu.Lock()
	defer blMu.Unlock()
	if bl != nil {
		if v, ok := bl.Vals[key]; ok {
			return v, "基线（" + bl.At.Local().Format("01-02 15:04") + "）"
		}
	}
	if v, ok := first[key]; ok {
		return v, "本次启动"
	}
	first[key] = cur
	return cur, "本次启动"
}

// counterCheck 判定一个单调累加的计数器：有新增给 lvlOnGrowth，
// 只有历史累计（基线之后没动过）一律判 0。
func counterCheck(id, name string, cur float64, lvlOnGrowth int, total, what string) Check {
	return counterCheckH(id, name, cur, lvlOnGrowth, 0, total, what)
}

// counterCheckH 同上，但可以指定"只有历史累计"时的级别。
// OOM 击杀这种事情即便发生在观察期之前也值得留一条提示（lvlOnHistory=1），
// 而 conntrack 丢弃、listen 溢出这类抖动给 0，免得永久挂告警。
func counterCheckH(id, name string, cur float64, lvlOnGrowth, lvlOnHistory int, total, what string) Check {
	base, src := baseOf(id, cur)
	delta := cur - base
	if delta < 0 { // 计数器回绕或机器重启过，重新以当前值为起点
		blMu.Lock()
		first[id] = cur
		blMu.Unlock()
		delta = 0
		src = "计数器已归零，重新起算"
	}
	switch {
	case delta > 0:
		return Check{ID: id, Name: name, Level: lvlOnGrowth,
			Message: fmt.Sprintf("自%s起新增 %s %.0f 次（累计 %s）", src, what, delta, total)}
	case cur > 0:
		return Check{ID: id, Name: name, Level: lvlOnHistory,
			Message: fmt.Sprintf("累计 %s，自%s起无新增", total, src)}
	default:
		return Check{ID: id, Name: name, Level: 0, Message: "累计 0 次 " + what}
	}
}

// taintCheck 判定内核 taint。taint 是位图不是计数器：只对基线之后"新出现的位"
// 告警，基线里已有的位（比如装了外部编译的网卡驱动）只作说明。
func taintCheck(cur int64, decode func(int64) string) Check {
	base, src := baseOf("kernel.tainted", float64(cur))
	newBits := cur &^ int64(base)
	switch {
	case newBits != 0:
		return Check{ID: "E01", Name: "内核 taint", Level: 1,
			Message: fmt.Sprintf("自%s起新增污染位 %d（%s），当前 tainted=%d",
				src, newBits, decode(newBits), cur)}
	case cur != 0:
		return Check{ID: "E01", Name: "内核 taint", Level: 0,
			Message: fmt.Sprintf("tainted=%d（%s）—— 自%s起无新增污染", cur, decode(cur), src)}
	default:
		return Check{ID: "E01", Name: "内核 taint", Level: 0, Message: "0（干净）"}
	}
}

// CounterSnapshot 读当前所有累计型计数器，用来生成基线。
// 键必须与各 check 里传给 counterCheck / taintCheck 的 id 一致。
func CounterSnapshot() map[string]float64 {
	out := map[string]float64{}
	if d, i, ok := conntrackDrops(); ok {
		out["CT02"] = d + i
	}
	if v, ok := listenOverflows(); ok {
		out["N05"] = v
	}
	if v, ok := kvField(procRoot+"/vmstat", "oom_kill", 1); ok {
		out["M05"] = v
	}
	if v, ok := readInt(procRoot + "/sys/kernel/tainted"); ok {
		out["kernel.tainted"] = float64(v)
	}
	return out
}

// ── 基线的读写：落在 <data-dir>/baseline.json ──────────────────────────

func BaselinePath(dataDir string) string { return filepath.Join(dataDir, "baseline.json") }

// LoadBaseline 从磁盘读基线并应用。文件不存在不是错误。
func LoadBaseline(dataDir string) (*Baseline, error) {
	b, err := os.ReadFile(BaselinePath(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out Baseline
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	if out.Vals == nil {
		out.Vals = map[string]float64{}
	}
	SetBaseline(&out)
	return &out, nil
}

// SaveBaseline 用当前计数器值生成基线，原子写盘并立即生效。
func SaveBaseline(dataDir, note string) (*Baseline, error) {
	b := &Baseline{At: time.Now(), Note: note, Vals: CounterSnapshot()}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	tmp := BaselinePath(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, BaselinePath(dataDir)); err != nil {
		return nil, err
	}
	SetBaseline(b)
	return b, nil
}

// ClearBaseline 删除基线文件并回到"只看本次启动之后的新增"。
func ClearBaseline(dataDir string) error {
	SetBaseline(nil)
	err := os.Remove(BaselinePath(dataDir))
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}
