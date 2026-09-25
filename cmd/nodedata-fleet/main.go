// nodedata-fleet — 巡视台：把几十上百台 nodedata 的 USE 五行汇到一块屏上轮播。
//
//	nodedata-fleet -hosts host.list -listen 0.0.0.0 -port 8888
//
// 一个二进制加一份 host.list。没有数据库、不存历史：历史在各台自己那里，
// 大屏只回答"现在哪台不对、从什么时候开始"。核心在 internal/fleet。
package main

import (
	_ "embed"
	"os"

	"github.com/githubflyideas/nodedata/internal/fleet"
)

var version = "dev"

//go:embed fleet.html
var pageHTML []byte

func main() {
	os.Exit(fleet.Main(fleet.Options{
		Name: "nodedata-fleet", Version: version, DefaultPort: "8888",
		Page: pageHTML, PageFile: "fleet.html", Args: os.Args[1:],
	}))
}
