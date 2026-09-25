package metrics

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

type golden struct {
	Unit         string  `json:"unit"`
	Domain       string  `json:"domain"`
	MinDelta     float64 `json:"min_delta"`
	Primary      int     `json:"primary"`
	Class        string  `json:"class"`
	Direction    float64 `json:"direction"`
	Family       string  `json:"family"`
	EvidenceOnly bool    `json:"evidence_only"`
}

// syntheticUnknown 是故意放进黄金快照的"没人会用的名字"，用来确认兜底路径。
// 除了它们，快照里的每个 ID 都必须是登记过的。
var syntheticUnknown = map[string]bool{
	"foo.bar": true, "weird": true, "x.util_thing": true, "y_pct": true, "z_ms": true,
	"a.bytes_total": true, "b.iops_x": true, "c_per_s": true, "d@": true, "@e": true,
	"mem.": true, "cpu.": true, "proc.cpu.": true,
}

func loadGolden(t *testing.T) map[string]golden {
	t.Helper()
	b, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g map[string]golden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// 重构不改行为：登记过的指标，8 个属性必须跟重构前 10 个旧函数的输出逐字一致。
func TestRegistryMatchesGolden(t *testing.T) {
	g := loadGolden(t)
	ids := make([]string, 0, len(g))
	for id := range g {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	checked := 0
	for _, id := range ids {
		want := g[id]
		in := Lookup(id)
		if !in.Registered {
			if !syntheticUnknown[id] {
				t.Errorf("%s 是真实指标却没登记，走了兜底", id)
			}
			continue
		}
		checked++
		prim := 0
		if in.Primary {
			prim = 1
		}
		got := golden{in.Unit, Domain(id), in.MinDelta, prim, in.Class, in.Direction(), in.Family, in.EvidenceOnly}
		if got != want {
			t.Errorf("%s\n  重构后 %+v\n  重构前 %+v", id, got, want)
		}
	}
	if checked < 150 {
		t.Errorf("只核对了 %d 个登记指标，快照可能没读全", checked)
	}
	t.Logf("核对 %d 个登记指标，%d 个故意的未知名", checked, len(g)-checked)
}

// 没登记的指标：只猜单位，其余一律中性——不参与归因、不进族、升高算坏。
// 旧代码会按前缀把它们塞进某个诊断类别，那等于替一个没人认识的指标下判断。
func TestUnregisteredIsNeutral(t *testing.T) {
	for id := range syntheticUnknown {
		in := Lookup(id)
		if in.Registered || in.Class != "" || in.Family != "" || in.EvidenceOnly || in.LowerIsBad {
			t.Errorf("%s 未登记，属性应中性：%+v", id, in)
		}
		if in.MinDelta != defaultMinDelta(in.Unit) {
			t.Errorf("%s 的门槛应取单位默认值", id)
		}
	}
	if u := Lookup("x.util_thing").Unit; u != "percent" {
		t.Errorf("兜底仍要猜对单位：x.util_thing = %s", u)
	}
}

func TestBaseAndDomain(t *testing.T) {
	for in, want := range map[string][2]string{
		"disk.util@nvme0n1": {"disk.util", "disk"}, "net.rx@eth0": {"net.rx", "net"},
		"procs_running": {"procs_running", "procs_running"}, "@e": {"@e", "@e"},
	} {
		if Base(in) != want[0] || Domain(in) != want[1] {
			t.Errorf("%s → %s/%s，期望 %v", in, Base(in), Domain(in), want)
		}
	}
}
