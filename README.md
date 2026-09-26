# nodedata — 单机性能展示与百台巡视

把一台 Linux 机器的现状、跟平时比的变化、谁在用资源摆在一屏里，让人自己看出问题。
**给出错的推理不如不给推理**：它只摆事实和"先看哪儿"，不替你下结论。

三个程序，都是单个静态二进制，零依赖，只读 `/proc` 和 `/sys`：

| 程序 | 跑在哪 | 默认端口 | 干什么 |
|---|---|---|---|
| `nodedata` | 每台被看的机器 | 8888 | 采集、判定、单机页面 |
| `nodedata-fleet` | 一台巡视台机器 | 8888 | 拉各台的状态，方块轮播大屏 |
| `nodedata-pelican` | 一台巡视台机器 | 8776 | 同上，天空视图：机群是一座城，鹈鹕飞过去照住出问题的机器 |

更细的设计和取舍见 [DESIGN.md](DESIGN.md)，每版改了什么见 [CHANGELOG.md](CHANGELOG.md)。

---

## 1. 装

从 GitHub Releases 下载 `nodedata-vX.Y.Z-linux.tar.gz`，解压出来是一个同名目录（**不会覆盖当前目录里的文件**）：

```bash
tar xzf nodedata-v5.25.1-linux.tar.gz
cd nodedata-v5.25.1
sha256sum -c SHA256SUMS                          # 校验
sudo install -m 755 nodedata-linux-amd64 /usr/local/bin/nodedata          # ARM 用 -arm64
sudo install -m 755 nodedata-fleet-linux-amd64 /usr/local/bin/nodedata-fleet
sudo install -m 755 nodedata-pelican-linux-amd64 /usr/local/bin/nodedata-pelican
nodedata version
```

## 2. 单机：nodedata

```bash
# 先试：只本机能看，前台跑，Ctrl-C 停
nodedata serve --data-dir ./data
# 浏览器开 http://127.0.0.1:8888/

# 给别人/巡视台看：对内网开放（记得用防火墙只放行该放行的来源）
nodedata serve --listen 0.0.0.0 --port 8888 --data-dir /var/lib/nodedata

# 带资源硬上限跑（CPU 最多 0.2 核、内存 300M、IO 让路；重启机器后消失）
sudo systemd-run --unit=nodedata -p CPUQuota=20% -p MemoryMax=300M -p Nice=10 \
  -p IOSchedulingClass=idle -p Restart=always --setenv=GOMAXPROCS=1 --setenv=GOMEMLIMIT=200MiB \
  /usr/local/bin/nodedata serve --listen 0.0.0.0 --data-dir /var/lib/nodedata

systemctl status nodedata            # 状态
journalctl -u nodedata -f            # 日志
sudo systemctl stop nodedata         # 停
```

开机自启：存成 `/etc/systemd/system/nodedata.service`，再 `sudo systemctl daemon-reload && sudo systemctl enable --now nodedata`。

```ini
[Unit]
Description=nodedata
After=network.target

[Service]
ExecStart=/usr/local/bin/nodedata serve --listen 0.0.0.0 --port 8888 --data-dir /var/lib/nodedata
Restart=always
RestartSec=5
CPUQuota=20%
MemoryMax=300M
Nice=10
IOSchedulingClass=idle
Environment=GOMAXPROCS=1
Environment=GOMEMLIMIT=200MiB

[Install]
WantedBy=multi-user.target
```

CentOS 7/8（cgroup v1）上 `MemoryMax` 不生效，换成 `MemoryLimit=300M`。

**参数**

| 参数 | 默认 | 说明 |
|---|---|---|
| `--listen` | `127.0.0.1` | 监听地址。页面里有进程名和 PID，开放前先想好谁能访问 |
| `--port` | `8888` | |
| `--data-dir` | `./data` | 历史（`history/`）、事故留证（`incidents/`）、人工基线都在这里；升级、重启不会丢 |
| `--history-dir` | `<data-dir>/history` | 长期历史：5 分钟一点，保留 14 天 |
| `--interval` | `10s` | 采集周期 |
| `--rootfs` | `/` | 要看容量的挂载点 |
| `--dump-interval` | `0` | 把页面数据转储成文件；0 = 不转储 |

**不开浏览器也能看**

```bash
curl -s localhost:8888/health.txt         # 一行，给监控 grep
curl -s localhost:8888/api/report         # 纯文本简报，可以直接贴给大模型
curl -s localhost:8888/api/use            # USE 五行（JSON）
curl -s localhost:8888/api/fleet          # 巡视台拉的就是这个
curl -s localhost:8888/api/cores          # 逐核 CPU
```

**接进现有监控**

```bash
curl -s http://127.0.0.1:8888/health.txt | grep -q '^NODEDATA .*status=NORMAL' || echo 告警
# NODEDATA host=db-01 status=WARN cpu=182% load=9.14 ... culprit=rsync/4411 reason="IO 劣化：..."
```

`status` 是 `NORMAL` / `WARN` / `DOWN`，异常时带 `reason=` 和 `culprit=`（进程名/PID）。

## 3. 状态怎么判

每台机器按 CPU / 内存 / 磁盘 / 网络 / 系统 五行给状态。**大部分时间机器是正常的**，所以第一目标是正常的机器不亮黄。

| 状态 | 什么时候 |
|---|---|
| **异常**（红） | 绝对判定没过：只读挂载、分区满、OOM…… |
| **偏离**（黄） | ① 往**坏的方向**变 + 变化显著 + 读数落在**可能是瓶颈的区间**，并持续 60 秒；或 ② 过了**绝对线**，持续 60 秒（不看历史） |
| 正常 | 其余。变化显著但不是问题的（比如变闲了）只标"高于/低于平时"，不变色 |
| 无数据 | 采不到，绝不当正常 |

| 资源 | 瓶颈区间 | 绝对线 |
|---|---|---|
| CPU | 整机 ≥50%，单核 ≥60%，排队 > 核数，CPU 压力 ≥5%，被偷 ≥5% | 单核 ≥95%，CPU 压力 ≥20%，排队 > 2×核数 |
| 内存 | 已用 ≥80%，换页 ≥1MB/s，内存压力 ≥5%，主缺页 ≥100/s | 内存压力 ≥10%，换出 ≥10MB/s |
| 磁盘 | IO 压力 ≥5%；延迟：机械盘 50ms / SSD 10ms / 虚拟盘 20ms | IO 压力 ≥20% |
| 网络 | 丢包/错包 ≥1/s，TCP 重传 ≥10/s | 持续丢包/错包 |

数字的来历写在 `cmd/nodedata/concern.go`，要调就改那里。

## 4. 巡视台：nodedata-fleet

一台机器跑巡视台，去各台拉状态（只拉不推，各台不用知道巡视台在哪），iPad 挂墙轮播。

```bash
cp host.list.example host.list        # 改成你的机器
nodedata-fleet -hosts host.list -listen 0.0.0.0
# iPad 打开 http://巡视台地址:8888/#rack

# 巡视台这台机器上也跑着 nodedata（同样 8888）时换个端口
nodedata-fleet -hosts host.list -listen 0.0.0.0 -port 8890

# 看拉取情况
curl -s localhost:8888/health.txt     # ok hosts=100 ok=93 dev=4 bad=1 lost=2 last_round=3s
curl -s localhost:8888/api/state | head -c 500
```

各台要做的只有：`nodedata serve --listen 0.0.0.0`，防火墙只放行巡视台的 IP。

| 参数 | 默认 | 说明 |
|---|---|---|
| `-hosts` | `host.list` | 主机清单，改了自动重读 |
| `-listen` | `127.0.0.1` | 给 iPad 看要设 `0.0.0.0` |
| `-port` | `8888` | |
| `-interval` | `15s` | 多久拉一轮 |
| `-timeout` | `5s` | 单台超时 |
| `-lost-after` | 3 轮 | 多久没拉到算失联 |

### host.list 格式

一台一行：`名字  地址  键=值 …`，`#` 后面是注释。

- 地址：`10.1.0.11`（补默认端口 8888）、`10.1.0.11:9100`，或完整 URL（反代、https、基本认证；密码不会显示在屏上）。
- 键：`dc`（机房）、`rack`（机柜）、`product`（产品线）、`group`（主机组）、`tags`（逗号分隔，可多个）。值里不能有空格。
- 改完保存就生效。**写错一处，整份不采用**，继续用上一版，屏幕顶部提示哪一行错了。

**例子**

```
# 名字        地址                                   分组
pay-db-01     10.1.0.11:8888   dc=tokyo1 rack=A03 product=支付 group=mysql-主 tags=核心,SSD
pay-java-01   10.1.0.21        dc=tokyo1 rack=A03 product=支付 group=java-app tags=核心
ord-redis-01  10.1.0.31        dc=tokyo1 rack=A04 product=订单 group=redis    tags=SSD
log-kafka-01  10.2.0.41        dc=osaka1 rack=B01 product=日志平台 group=kafka  # 没有标签
edge-01       https://ops:secret@gw.example.com/nd/edge-01   dc=osaka1 product=基础设施
test-01       10.9.0.5         # 什么都不写也行：只在"主机"那一层出现，其他层归到"(未填)"
```

### 屏上怎么用

- 顶部选按什么翻页：主机 / 主机组 / 产品线 / 机柜 / 机房 / 标签。
- 左右滑动或点边缘箭头翻页，箭头下的数字 = 那一边还有几页不正常。默认 15 秒一页，手一碰暂停 60 秒。
- 点方块进主机页。右上角"拉取状况"列出没拉到的机器和原因（比如"对方只监听了 127.0.0.1"）。
- 巡视台自己断了，整屏变灰并写明最后更新时间——绝不留着一屏旧的绿。

## 5. 鹈鹕巡检：nodedata-pelican

同一份 host.list、同一套拉取和判断，换成天空视图：**一栋楼 = 一个机柜，一扇窗 = 一台主机**，窗的颜色就是状态。

```bash
nodedata-pelican -hosts host.list -listen 0.0.0.0              # http://巡视台地址:8776/
nodedata-pelican -hosts host.list -listen 0.0.0.0 -port 9000   # 换端口

# 跟方块版并排跑，比一比哪个在墙上好用
nodedata-fleet   -hosts host.list -listen 0.0.0.0 &            # :8888
nodedata-pelican -hosts host.list -listen 0.0.0.0 &            # :8776
```

| 情况 | 鹈鹕 |
|---|---|
| 全部正常 | 高空巡航，拍几下滑一阵，拖着横幅"全部 N 台正常" |
| 异常 | 俯冲下来，贴着那扇窗快速拍翅悬停，探照灯照住它 |
| 偏离 | 在那栋楼上空盘旋，盯着那扇窗 |
| 失联 | 贴窗悬停，头顶问号 |
| 巡视台断开 | 落在楼顶收翅睡觉，城市变灰 |

停下时左上角"巡检报告"显示这台的 USE 五行。它只去已经亮红、亮黄、变黑的窗，不推断原因。
点楼进这一组，点窗进这台主机；顶部"天空 / 列表"随时切换（列表就是 nodedata-fleet 的页面）。

地址 `#` 后面可以组合，用 `-` 连接：

```
http://巡视台:8776/#rack            按机柜分楼（默认）
http://巡视台:8776/#group           按主机组分楼
http://巡视台:8776/#product-list    按产品线、打开就是列表视图
http://巡视台:8776/#rack-night      强制夜景（默认天色跟 iPad 本地时间：白天/霞光/夜）
http://巡视台:8776/#demo            演示模式：不连任何机器，100 台假数据，顶部挂"演示数据"
```

## 6. iPad 挂墙

1. Safari 打开巡视台地址 → 分享 → **添加到主屏幕**（全屏、无地址栏）。
2. 设置 → 显示与亮度 → 自动锁定 → **永不**。
3. 设置 → 辅助功能 → **引导式访问** 打开；进页面后连按三下顶部按钮（有主屏幕按钮的机型按主屏幕按钮）锁定。
4. 页面不从外网加载任何东西，机房 iPad 上不了外网也能用。

## 7. 从源码编译

```bash
git clone https://github.com/githubflyideas/nodedata && cd nodedata
V=$(git describe --tags --always)
for c in nodedata nodedata-fleet nodedata-pelican; do
  CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$V" -o $c ./cmd/$c
done
go test ./...                          # 含 8 天历史回放和故障语料，约 1 分钟
go test -short ./...                   # 跳过回放，几秒
```

## 8. 安全

- 默认只监听 `127.0.0.1`。页面里有进程名、PID、负载，开放给内网前先用防火墙限制来源，或放在带鉴权的反代后面（程序本身不做鉴权）。
- 只读 `/proc`、`/sys`；不连服务、不读配置、不执行命令；不采集进程命令行（参数里常有密码）。
- 页面给的"下一步命令"只填 PID、不拼进程名，只给诊断命令，nodedata 自己从不执行它们。
