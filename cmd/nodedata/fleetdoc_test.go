package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// /api/fleet 是跨机器的契约：巡视台和各台不会同时升级。字段名只能加不能改。
func TestFleetDocFieldNames(t *testing.T) {
	b, _ := json.Marshal(fleetDoc{V: fleetAPIVersion, Host: "h", Version: "v", At: 1, Rows: []UseRow{{Resource: "CPU", State: "正常"}}})
	s := string(b)
	for _, k := range []string{`"v":1`, `"host":`, `"version":`, `"at":`, `"rows":`, `"resource":"CPU"`, `"state":"正常"`} {
		if !strings.Contains(s, k) {
			t.Errorf("缺 %s：%s", k, s)
		}
	}
}
