// timepoints.go — 全页面共用的一套时间点。
//
// 原来页面上有三套：变化幅度 10 档（5分 10分 20分 40分 1.5时 3时 6时 12时 1天 7天）、
// 对比表 6 列、服务表 7 列，中间的点各不相同，纯粹是各加各的——对比表和服务表还是
// 两份分别定义的列表，服务表的注释写着"与对比表对齐"，其实没对齐。
//
// 现在只有这一份。三张表的差别只剩一处，而且是有道理的：
//   - 对比表（原始数值）和服务表（在不在）不需要统计，保留期内 14 天都能看；
//   - 变化幅度要判断"这个变化平时有多大"，需要 2 倍的历史，14 天保留期最多撑到 7 天，
//     所以它用前 9 个（deviation.LagSeconds，一致性由测试钉住）。
package main

import "time"

type timePoint struct {
	Name string // 接口里的列编号："5m"、"1h"、"3d"；页面上写成"5分钟前"
	Ago  time.Duration
}

var timePoints = []timePoint{
	{"5m", 5 * time.Minute},    // 刚刚
	{"10m", 10 * time.Minute},  // 刚才
	{"30m", 30 * time.Minute},  // 半小时前
	{"1h", time.Hour},          // 一小时前
	{"6h", 6 * time.Hour},      // 今天早些时候
	{"12h", 12 * time.Hour},    // 半天前
	{"1d", 24 * time.Hour},     // 昨天这个时候
	{"3d", 72 * time.Hour},     // 前几天
	{"7d", 7 * 24 * time.Hour}, // 上周这个时候
	{"14d", 14 * 24 * time.Hour},
}

// timePointNamed 按编号取时间点；找不到返回零值和 false。
func timePointNamed(name string) (timePoint, bool) {
	for _, p := range timePoints {
		if p.Name == name {
			return p, true
		}
	}
	return timePoint{}, false
}
