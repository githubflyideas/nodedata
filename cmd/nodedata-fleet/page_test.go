package main

import (
	"os"
	"strings"
	"testing"

	"github.com/githubflyideas/nodedata/internal/fleet"
)

// 页面里不能留原型的模拟数据，也不能从外网加载任何东西（机房 iPad 多半上不了外网）。
func TestPageIsSelfContained(t *testing.T) {
	s := string(pageHTML)
	for _, bad := range []string{"sampleInventory", "tickSim", "示例数据", "https://", "http://"} {
		if strings.Contains(s, bad) {
			t.Errorf("页面里不应出现 %q", bad)
		}
	}
	for _, need := range []string{"api/state", "巡视台断开", "不代表正常"} {
		if !strings.Contains(s, need) {
			t.Errorf("页面里应有 %q", need)
		}
	}
}

// 仓库里给的样例清单必须能原样读通。
func TestExampleInventoryParses(t *testing.T) {
	f, err := os.Open("host.list.example")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	hs, errs := fleet.ParseInventory(f)
	if len(errs) > 0 || len(hs) != 5 {
		t.Fatalf("hosts=%d errs=%v", len(hs), errs)
	}
}
