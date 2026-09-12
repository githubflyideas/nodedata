package check

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// TestStatfsMatchesDF：根分区使用率必须和运维手里的 df 对得上。
// v3.2.0 之前用 (Blocks-Bavail)/Blocks，在本机上 df=52% 而 L0 报 96.4%，
// 于是 health.txt 恒为 DOWN —— 这种误报比漏报更致命。
func TestStatfsMatchesDF(t *testing.T) {
	u, ok := statfsUsage("/")
	if !ok {
		t.Skip("statfs 不可用")
	}
	out, err := exec.Command("df", "-P", "/").Output()
	if err != nil {
		t.Skip("df 不可用:", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		t.Skip("df 输出异常")
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 5 {
		t.Skip("df 输出异常")
	}
	want, err := strconv.ParseFloat(strings.TrimSuffix(f[4], "%"), 64)
	if err != nil {
		t.Skip("df 百分比解析失败")
	}
	// df 向上取整，允许 1.5 个百分点的差
	if d := u.spacePct - want; d > 1.5 || d < -1.5 {
		t.Fatalf("根分区使用率 %.1f%%，df 报 %.0f%% —— 口径不一致", u.spacePct, want)
	}
}
