package main

import (
	"strings"
	"testing"
)

// 跟 nodedata-fleet 的页面同样的底线：不从外网加载东西、断开要说、失联不当正常。
// 另外鹈鹕必须能关掉，关掉后信息一样不少。
func TestPelicanPage(t *testing.T) {
	s := string(pageHTML)
	for _, bad := range []string{"https://", "http://", "sampleInventory", "tickSim"} {
		if strings.Contains(s, bad) {
			t.Errorf("页面里不应出现 %q", bad)
		}
	}
	for _, need := range []string{"api/state", "巡视台断开", "不代表正常", `id="pel"`, "pel-off", "巡检日记", "巡视台来之前就这样"} {
		if !strings.Contains(s, need) {
			t.Errorf("页面里应有 %q", need)
		}
	}
}
