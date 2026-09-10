#!/bin/sh
# install.sh — 把 nodedata 装成常驻服务（开机自启），资源上限与 README 里的一行命令完全相同。
#
#   sudo ./install.sh [./nodedata]           # 默认端口 8888
#   sudo PORT=9000 ./install.sh ./nodedata
#
# 只想临时跑一下、不留痕迹：直接用 README 里的 systemd-run 那一行。
set -eu

BIN_SRC=${1:-./nodedata}
PORT=${PORT:-8888}
BIN=/usr/local/bin/nodedata
DATA=/var/lib/nodedata
UNIT=/etc/systemd/system/nodedata.service

[ "$(id -u)" = 0 ] || { echo "需要 root" >&2; exit 1; }
[ -x "$BIN_SRC" ] || { echo "找不到可执行文件 $BIN_SRC" >&2; exit 1; }

install -m 0755 "$BIN_SRC" "$BIN"
mkdir -p "$DATA"

cat > "$UNIT" <<UNIT
[Unit]
Description=nodedata — single-host deviation monitor
After=network.target

[Service]
ExecStart=$BIN serve --port $PORT --data-dir $DATA
Restart=always
RestartSec=5
# sidecar 的硬上限：再出 v3.0.8 那样的 bug，也只能用到 0.2 个核、300MB。
CPUQuota=20%
MemoryMax=300M
Nice=10
IOSchedulingClass=idle
# Go 1.24 及以前不读 cgroup CPU 配额，多核机上会开满 P 然后被 CFS 节流卡顿。
Environment=GOMAXPROCS=1
# 先让 GC 使劲回收，别等内核 OOM。
Environment=GOMEMLIMIT=200MiB

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now nodedata.service
sleep 2
systemctl --no-pager --lines=0 status nodedata.service || true
echo "页面：http://$(hostname -I 2>/dev/null | awk '{print $1}'):$PORT/   数据：$DATA   日志：journalctl -u nodedata -f"
