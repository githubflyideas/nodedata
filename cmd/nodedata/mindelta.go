// mindelta.go — "值得一提的最小变化量"。
//
// z 是尺度无关的：一台安静的机器 σ 趋近于 0，于是每秒 202 字节的流量、
// 每秒 2.8 个包、0.1 次重传全都是 6σ，整屏红。**统计上显著不等于实际上要紧。**
//
// 这些门槛不是拍脑袋，取的是"在单机排查里低于这个量就不会有人多看一眼"的水平：
// 网卡流量按 1 MB/s、包速率按 100 包/秒、内存按 128 MB、CPU 按 3 个百分点。
// 错误类（丢包、重传、UDP 溢出）门槛低得多——1 次/秒 就值得看，因为它们本该是 0。
package main

import "github.com/githubflyideas/nodedata/internal/metrics"

// minDeltaFor 见 internal/metrics：各单位的默认门槛和逐指标的例外都登记在那里。
func minDeltaFor(id string) float64 { return metrics.Lookup(id).MinDelta }
