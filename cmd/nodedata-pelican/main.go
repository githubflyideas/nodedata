// nodedata-pelican — 鹈鹕巡检员：巡视台的实验版界面。
//
//	nodedata-pelican -hosts host.list -listen 0.0.0.0        # 默认端口 8776
//
// 跟 nodedata-fleet 用同一份 host.list、同一套拉取和判断（internal/fleet），
// 只是页面上多了一只骑车巡逻的鹈鹕：全部正常时它沿着底边巡逻，
// 有机器不对它就骑过去、停下、用嘴指着，气泡里写上是哪台、从什么时候开始。
// 两个可以并排跑，对比哪个在墙上更好用。
package main

import (
	_ "embed"
	"os"

	"github.com/githubflyideas/nodedata/internal/fleet"
)

var version = "dev"

//go:embed pelican.html
var pageHTML []byte

func main() {
	os.Exit(fleet.Main(fleet.Options{
		Name: "nodedata-pelican", Version: version, DefaultPort: "8776",
		Page: pageHTML, PageFile: "pelican.html", Args: os.Args[1:],
	}))
}
