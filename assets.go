// Package nodedata 只承载嵌入的前端页面，让二进制是唯一需要分发的文件。
package nodedata

import _ "embed"

// IndexHTML 是首页。开发时可用 serve --web-root 指向磁盘上的 index.html 覆盖。
//
//go:embed index.html
var IndexHTML []byte
