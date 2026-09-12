// fs.go — 文件系统使用率（statfs）。
//
// 唯一一处不走 /proc 的采集。磁盘写满是最常见也最致命的单机故障之一，
// 而 /proc/diskstats 只有 IO 速率、没有容量 —— 容量必须 statfs。
//
// 只看挂载点根 "/"：单机排查关心的是"根分区还剩多少"，逐挂载点遍历要读 /proc/mounts，
// 在有大量 bind mount 的机器上噪声远大于价值。
package collector

import (
	"syscall"
	"time"
)

// parseFilesystem 输出根分区使用率与 inode 使用率。
func (c *Collector) parseFilesystem(now time.Time, out *[]Sample) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(c.cfg.RootFS, &st); err != nil {
		return
	}
	// 用 Blocks-Bfree 作已用，Bavail 作可用：两者之差是 root 保留块。
	// df 的百分比口径是 used/(used+avail)，这里保持一致，免得和运维手里的 df 对不上。
	if st.Blocks > 0 {
		used := st.Blocks - st.Bfree
		denom := used + st.Bavail
		if denom > 0 {
			*out = append(*out, Sample{MetricID: "fs.used_pct", TS: now, Value: float64(used) * 100 / float64(denom)})
		}
		*out = append(*out, Sample{MetricID: "fs.avail", TS: now, Value: float64(st.Bavail) * float64(st.Bsize)})
	}
	if st.Files > 0 {
		*out = append(*out, Sample{MetricID: "fs.inode_used_pct", TS: now,
			Value: float64(st.Files-st.Ffree) * 100 / float64(st.Files)})
	}
}
