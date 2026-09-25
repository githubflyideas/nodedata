# nodedata — 单机性能展示与排查工具

**nodedata 首先是一个性能展示工具。**它把一台机器的现状、历史对比、服务变化摆在一屏里，
让人自己看出问题。它也会给出"线索"（哪个指标偏了、谁在用资源），但那部分标着**测试中**，
是"先看哪儿"的提示，不是结论——**给出错的推理不如不给推理**。

一个静态二进制.
## 运行：一行命令、发现问题

```bash
GOMAXPROCS=1 ./nodedata serve --listen 0.0.0.0 --data-dir ./data
```

（带资源硬上限，停掉或重启机器即消失）：

```bash
sudo systemd-run --unit=nodedata -p CPUQuota=20% -p MemoryMax=300M -p Nice=10 -p IOSchedulingClass=idle -p Restart=always --setenv=GOMAXPROCS=1 --setenv=GOMEMLIMIT=200MiB /usr/local/bin/nodedata serve --port 8888 --data-dir /var/lib/nodedata
```

看状态 / 日志 / 停止：

```bash
systemctl status nodedata ; journalctl -u nodedata -f ; sudo systemctl stop nodedata
```

开机自启：把下面这段存成 `/etc/systemd/system/nodedata.service`，然后
`systemctl daemon-reload && systemctl enable --now nodedata`。
（仓库里只有 Go 与 HTML，不提供安装脚本；这段与上面那一行命令的限制完全相同。）

```ini
[Unit]
Description=nodedata — single-host deviation monitor
After=network.target

[Service]
ExecStart=/usr/local/bin/nodedata serve --port 8888 --data-dir /var/lib/nodedata
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



### 为什么是这些限制

| 设置 | 理由 |
|---|---|
| `CPUQuota=20%` | 正常开销约 2% 核（按实测外推：85 条序列、1000 个进程）；20% 是给页面访问留的余量，同时是硬上限 —— v3.0.8 那种 bug 再出现也只能用到 0.2 核而不是 1.5 核 |
| `GOMAXPROCS=1` | Go 1.24 及以前不感知 cgroup CPU 配额，多核机上会开满 P，突发后被 CFS 节流卡住 |
| `MemoryMax=300M` + `GOMEMLIMIT=200MiB` | 满 24h 原始层 + 56 天长期层、85 条序列约 72MiB；GOMEMLIMIT 让 GC 在内核 OOM 之前先收紧 |
| `Nice=10`、`IOSchedulingClass=idle` | 和业务抢资源时让路 |

cgroup v1 的机器（CentOS 7/8 默认）上 `MemoryMax` 不生效，改用 `-p MemoryLimit=300M`。

## 参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `--listen` | `127.0.0.1` | 监听地址。设为 `0.0.0.0` 前请确认有防火墙或反代鉴权 |
| `--port` | `8888` | 监听端口 |
| `--data-dir` | `./data` | 转储 `*.json`、`baseline.json`、`history/` |
| `--history-dir` | `<data-dir>/history` | 长期层落盘目录 |
| `--rootfs` | `/` | 要监控容量的挂载点 |
| `--interval` | `5s` | 采集与 L0 巡检周期 |
| `--dump-interval` | `30s` | 页面数据转储周期 |
| `--proc` / `--sys` | `/proc` / `/sys` | 测试用的替代根 |
| `--web-root` | 空 | 从磁盘读 `index.html` 覆盖内置页面（前端开发用） |

## 数据怎么存

- **原始层**：每个采集周期一点，内存里保留 24 小时。5 分钟到 1 小时这几档用它。
- **采集维度**：CPU / 内存 / 磁盘（含容量）/ 网络（含 UDP 缓冲区溢出）/ 套接字（TCP 状态分布、conntrack 水位）
  + 每块整盘（`disk.util@nvme0n1` 等）+ 每个网卡（`net.rx_drop@eth1` 等）
  + 每个进程（CPU、块设备读写、主缺页、RSS 与 1 小时增长、状态）。整机磁盘汇总不重复计 dm/md，
  整机网络只汇总物理/virtio 网卡（不重复计 bond 与 VLAN）。
- **长期层**：每 5 分钟从原始点里抽一个（不求平均，保证 Δ 的分布不变），保留 14 天，
  追加写到 `history/YYYY-MM-DD.jsonl`（每天约 0.7MB，14 天约 9MB），重启时读回。
  6 小时到 7 天这几档、以及对比表和服务表的"14天前"一列用它。
- **一套时间点，全页面共用**：5分钟 · 10分钟 · 30分钟 · 1小时 · 6小时 · 12小时 · 1天 · 3天 · 7天 · 14天。
  对比表（原始数值）和服务表（在不在）用全部 10 个；变化幅度用前 9 个——它要判断"这个变化平时有多大"，
  一档需要 2 倍的历史（往回够 H，再估计这一档平时的波动），14 天保留期最多撑到 7 天。
  首次部署后，"和 1 天前比"约 2 天后可用，"和 7 天前比"约 14 天后可用。

## 它会告诉你什么

L4 把偏离按症状归成 CPU / IO / 内存 / 网络四类，每类一条结论，写明**责任方**和**下一步命令**：

```
CPU 劣化：cpu.user 上升 6.0σ — 责任方 burner（PID 4242）
  责任方 burner PID 4242  占 150% 核，占整机忙碌的 41%；在这次变化开始之后才出现，是首要嫌疑
  下一步 top -H -p 4242 · pidstat -u -t -p 4242 1 5 · cat /proc/4242/status · perf top -p 4242
```

- 责任方排序：自身序列也偏离的进程 > 变化开始后才出现的进程 > 此刻用得最多的进程（会注明"未必是原因"）。
  常年 200% 的数据库不会因为一直最大就被指认。
- IO 同时给出设备（按盘的 util/延迟）和进程（/proc/PID/io 块层读写），并列出 D 状态进程。
- 内存看 1 小时 RSS 增长、主缺页；slab 领头时指认内核而不是进程。
- 虚机上 `cpu.steal` 领头时指认宿主机，不冤枉本机进程。
- 网络只到接口（按进程拆流量需要 eBPF，未做）。
- 责任方是 nodedata 自己时会明说。
- 结论要求偏离在最近 3 个采样点（15 秒）持续，单点噪声不下结论。

## 事故留证

后台每 15 秒跑一次 L4（不需要打开页面）。出现带责任方的结论时，把现场存到
`<data-dir>/incidents/<时间>-<类别>.json`：结论与完整诊断链、|z|≥2 的指标、进程快照、
责任进程的线程级 CPU（1 秒采样）与 wchan/内核栈、全部 D 状态进程及其栈、最近 100 条内核日志、
原始 pressure/meminfo/vmstat/diskstats/net 文件。同一责任方 30 分钟内只留一次，保留最近 100 份。
不采集进程命令行（参数里常有密码）。页面上可以"立即留证"。

## 故障语料库

- **回放**（每次 CI）：`go test -run TestFaultCorpus -v ./cmd/nodedata`。8 个物理机/虚机场景
  （新失控进程、老进程变异、nodedata 自身、单盘写入、3 小时内存泄漏、网卡丢包、虚机 steal、安静的一天），
  断言类别与责任方，报告出结论耗时和误报率。
- **真实注入**（实验机）：`./faultlab --url http://127.0.0.1:8888 --scenarios cpu,io,mem`，
  nodedata 需先运行约 10 分钟。网卡丢包：`sudo ./faultlab --scenarios net --iface eth0`（需要 tc 和该口上的 TCP 流量）。

## 页面

绝对判定（L0）固定在最上面，其余分四个标签页：

| 标签页 | 内容 |
|---|---|
| 关键指标 | 25 条曲线（1h/6h/24h/7d/14d 可选）+ 诊断链 + 事故留证 |
| OS 指标 | 全部采集项：CPU / 内存 / 磁盘 / 网络 / 套接字 / 进程 |
| 整体偏离度和占用 | z 热力图 + 此刻谁在用资源（带下一步命令） |
| 服务对比 | 服务清单与历史在否 |

## 服务

页面上单独一个"服务"区块，回答"这台机器上跑着什么、各自什么时候起来的、昨天那个还在不在"。

识别**只读 /proc**：可执行文件名查表 → java 命令行（区分 ELK / Kafka / Tomcat）→
监听端口反查 → 都不认就显示可执行文件名。不连服务、不读配置、不执行任何命令。
把 mysqld 改名部署也能靠 3306 认出来。内置表覆盖 Nginx / Apache / Caddy / HAProxy /
MySQL / MariaDB / PostgreSQL / MongoDB / Redis / Memcached / ClickHouse / etcd /
Elasticsearch / Kibana / Logstash / Kafka / ZooKeeper / RabbitMQ / Docker / containerd /
BIND / CoreDNS / PHP-FPM 等。

每行给出：服务名与监听端口、进程名、PID、实例数、启动时间、已运行时长、CPU、内存，
以及 1h / 6h / 12h / 1d / 3d / 7d / 14d 前在不在（`✓` 在 / `—` 不在 / `?` 当时 nodedata 没在跑 /
`重启` 那时在但之后换过一次）。**消失的服务留在表里标灰**，不会凭空不见。

服务变化只呈现、不告警：我们分不清"挂了"和"运维手动停的"，分不清就不该报警。

## 给监控系统的接口

`GET /health.txt` 一行文本，关键字打头，主机名在同一行：

```
NODEDATA host=jp02-dns-01 status=WARN cpu=182% load=9.14 mem_avail=1.2GiB disk_util=96 \
  culprit=fl-io/236 reason="IO 劣化：disk.await_w 上升 6.0σ" ts=2026-09-12T03:10:51Z
```

```bash
curl -s http://127.0.0.1:8888/health.txt | grep -q '^NODEDATA .*status=NORMAL' || 告警
```

`status` 取 L0 绝对判定与 L4 归因结论中较严重者：`DOWN`（L0 有 fail）、
`WARN`（L0 有 warn，或 L4 给出了带责任方的结论）、`NORMAL`。异常时 `reason=` 说明原因，
`culprit=` 直接给出进程名/PID，派单时不用再登机器。
自由文本里的 `NODEDATA`、`status=`、换行与引号都会被中和 —— 进程名是攻击者可控的，
否则一个叫 `NODEDATA status=NORMAL` 的进程就能让监控的 grep 在真告警时匹配成功。

## 巡视台：nodedata-fleet

几十上百台机器的轮播大屏，给挂墙的 iPad 用。另一个二进制，加一份 `host.list`：

```
nodedata-fleet -hosts host.list -listen 0.0.0.0 -port 8888
```

启动参数：

| 参数 | 默认 | 说明 |
|---|---|---|
| `-hosts` | `host.list` | 主机清单文件，改了自动重读 |
| `-listen` | `127.0.0.1` | 监听地址；给 iPad 看要设 `0.0.0.0`，并用防火墙限制来源 |
| `-port` | `8888` | 页面端口。**巡视台所在机器如果也跑着 nodedata（默认同样是 8888），两者必须错开**，比如 `-port 8890`；撞了启动会直接报"端口已被占用"并提示 |
| `-interval` | `15s` | 多久拉一轮（最小 5s） |
| `-timeout` | `5s` | 单台拉取超时（不超过间隔的一半） |
| `-lost-after` | 3 轮 | 多久没拉到算失联 |
| `-parallel` | `32` | 同时拉几台 |

iPad 打开 `http://巡视台地址:8888/#rack`（`#` 后面是开机默认按什么翻：host / group / product / rack / dc / tags）。

### host.list 格式

纯文本，**一台主机一行**，字段之间用空格或 Tab 隔开（几个都行，对齐好看就行）：

```
名字  地址  键=值  键=值 …
```

| 位置 | 必填 | 说明 |
|---|---|---|
| 第 1 列：名字 | 是 | 大屏上显示的名字。字母、数字、`.` `-` `_`，最长 63 个字符，**不能重复** |
| 第 2 列：地址 | 是 | 这台 nodedata 的地址，三种写法见下 |
| 后面：键=值 | 否 | 用来分组翻页，可以不写、可以只写几个、顺序随意 |

**地址的三种写法**

| 写法 | 例子 | 巡视台实际访问 |
|---|---|---|
| 只写 IP 或主机名 | `10.1.0.11` | `http://10.1.0.11:8888`（补 nodedata 默认端口） |
| IP:端口 | `10.1.0.11:9100` | `http://10.1.0.11:9100` |
| 完整 URL | `https://ops:密码@gw.example.com/nd/web-01` | 原样使用；经反代、走 https、带基本认证时用 |

完整 URL 里的用户名密码只用于拉取，**不会**出现在大屏和 `/api/state` 里。

**键**（只认这 5 个，写别的算错误）

| 键 | 大屏上叫 | 例子 | 说明 |
|---|---|---|---|
| `dc` | 机房 | `dc=tokyo1` | |
| `rack` | 机柜 | `rack=A03` | 大屏上显示为"机房 / 机柜"（`tokyo1 / A03`），不同机房的同名机柜不会混在一起 |
| `product` | 产品线 | `product=支付` | |
| `group` | 主机组 | `group=mysql-主` | |
| `tags` | 标签 | `tags=核心,SSD` | 逗号分隔、可以多个；一台机器会出现在它的每个标签页里 |

值里不能有空格（有空格就会被当成下一个字段），中文可以。某个键没写的机器，在按这个键翻页时归到"(未填)"一页，
没有标签的归到"(无标签)"。`#` 后面到行尾是注释，空行忽略。

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

**改了就生效，写错不会黑屏**

- 保存即可，巡视台每轮开始前检查文件，不用重启。
- 新文件里**有任何一处**写错（缺地址、名字重复、不认识的键、`键=值` 写成 `键:值`……），整份不采用，
  继续用上一版，大屏顶部提示"第几行、错在哪"，右上角"拉取状况"里能看到全部错误。一个笔误不会让某台机器悄悄从墙上消失。
- 首次启动时没有上一版可退：能读的先用上，错的行照样提示；一台都读不到就直接退出并打印错误。
- 仓库里的完整样例：`cmd/nodedata-fleet/host.list.example`（有测试保证它和这里的例子都能原样读通）。

- **只拉不推。** 巡视台每 15 秒并发去各台拉一次 `/api/fleet`（USE 五行，判断已经在各台用各自的基线做完了）。
  各台不需要知道巡视台在哪，也不往外发任何东西。各台要做的只有：`nodedata serve --listen 0.0.0.0`，
  防火墙只放行巡视台的 IP。v5.21 及更早的 nodedata 没有 `/api/fleet`，巡视台会退回 `/api/use`，照样能看。
- **没有数据库、不存历史。** 历史在各台自己那里；大屏只回答"现在哪台不对、从什么时候开始"。点方块进主机页。
- **按什么翻页**：主机 / 主机组 / 产品线 / 机柜 / 机房 / 标签，来自 host.list（格式见上）。左右滑动或点边缘箭头翻页；箭头下的数字是那一边还有几页不正常。
  默认每页 15 秒自动轮播，手一碰暂停 60 秒。页面地址加 `#rack`、`#host` 等可以指定开机默认按什么翻。
- **改 host.list 不用重启**，每轮开始前检查。新文件有一处写错就整份不采用、继续用上一版，大屏顶部提示哪一行错了——
  一个笔误不能让一台机器从墙上悄悄消失。
- **宁可说不知道，也不画假绿：**
  - 超过 3 轮（默认 45 秒）没拉到 = 失联，不带上一次的读数；从没拉到过的单独标出来；
  - 对方少报一行或状态认不出 = 无数据，不当正常；
  - 巡视台之前就已经不正常的，起点写"巡视台 hh:mm 开始看时已是这样"，不编时间；
  - iPad 连不上巡视台、或巡视台停止拉取，整屏变灰并写明最后一次更新时间；
  - "同时发生"（同一资源 3 分钟内 ≥2 台变坏）只列事实，并提示时钟偏差超过 5 秒的机器。
- **拉取状况**（右上角）：没拉到的机器和原因，常见原因会翻译成部署时该查什么
  （连接被拒绝多半是对方只监听了 127.0.0.1）。
- `/health.txt` 一行文本给现有监控：`ok hosts=100 ok=93 dev=4 bad=1 lost=2 last_round=3s`，
  刚启动第一轮还没拉完是 `starting`，超过 3 轮没拉完是 `crit`，清单有错是 `warn`。
- iPad：Safari 打开上面的地址后"添加到主屏幕"（全屏、无地址栏），设置里自动锁定设为"永不"，
  再开"引导式访问"锁在这个页面上。页面不从外网加载任何东西，机房 iPad 上不了外网也能用。

### 实验版：鹈鹕巡检员 nodedata-pelican

同一套巡视台（同一份 host.list、同一套拉取和判断，代码在 `internal/fleet`），换了一个会动的界面，默认端口 **8776**，
可以跟 `nodedata-fleet` 并排跑着对比：

```
nodedata-pelican -hosts host.list -listen 0.0.0.0              # http://巡视台地址:8776/
```

屏幕底边多了一条巡逻道，一只骑自行车的鹈鹕。它是一个会动的指示器，**去哪、摆什么姿势全部来自屏上已经画出来的东西，自己不推断原因**：

| 情况 | 鹈鹕 |
|---|---|
| 这一页全正常 | 沿巡逻道慢慢来回骑；所有机器都正常时，隔一阵摇一下铃"叮～ 全部 N 台正常" |
| 有机器异常 | 骑到那台下面停住，抬嘴指着它，嘴囊里叼着写有主机名的牌子，虚线连到那个方块/那一行 |
| 有机器偏离 | 停在旁边歪头看着 |
| 有机器失联 | 停在旁边，头顶一个问号 |
| host.list 写错了（且这页没别的问题） | 拿着清单发愁，气泡里写哪一行错了 |
| 巡视台断开 | 趴在车把上睡着，气泡写最后一次更新时间 |

气泡里的起点跟主机页同一口径：巡视台亲眼看到变坏的写"hh:mm 起"，启动前就不正常的写"巡视台来之前就这样"。

另外两样是这个版本独有的：

- **巡检日记**：巡视台亲眼看到的状态变化（某台某资源 正常→异常、失联、重新拉到了），底边显示最近三条，
  点鹈鹕或右上角"巡检日记"看全部（服务端保留最近 200 条，重启清空）。启动前就不正常的不算变化；只记"什么时候变的"，不猜为什么。
- **可以关**：底栏"鹈鹕：开/关"，关掉后跟 nodedata-fleet 的信息完全一样。系统设置了"减少动态效果"时，鹈鹕不做动画、直接出现在目标位置。

## 监听与访问控制

默认只监听 `127.0.0.1`。页面会列出进程名、PID 与主机负载水位，是内网侦察的现成材料，
所以要给别人看必须显式 `--listen 0.0.0.0`，并自行用防火墙限制来源或放在带鉴权的反代之后
（程序本身不做鉴权）。绑非回环地址时启动会打印一行警告。

## API

| 路径 | 说明 |
|---|---|
| `/` | 页面 |
| `/data/{1h,6h,24h,7d,30d}.json` | 窗口数据（含 `procs` 进程快照） |
| `/data/health.json` | 采集器自检，含 `self.cpu_pct`、`self.rss_mb` |
| `/api/check` | L0 绝对判定 |
| `/api/diagnosis?z=3` | L4 诊断链 |
| `/health.txt` | 一行文本，给监控 grep |
| `/api/services` | 服务清单与历史在否 |
| `/api/keyseries?win=6h` | 关键指标曲线数据（`w` 为每步最坏值） |
| `/api/use` | USE 五行：读数、已查项、谁干的、下一步命令 |
| `/api/fleet` | 给巡视台拉的：USE 五行外加版本号、主机名、本机时间（跨机器契约，只加字段不改字段） |
| `/api/report` | 纯文本简报（机器、先后、最近变化、各资源、看不到），可直接贴给大模型 |
| `/api/context` | 机器底子：核数、内存、盘类型、虚拟化、开机时长 |
| `/api/cores` | 逐核 CPU（仅当前） |
| `/api/baseline` | GET / POST / DELETE 人工基线 |
| `/api/incidents` | GET 留证列表；POST 立即留证 |
| `/api/incidents/<id>` | 一份证据 |

## 编译

```bash
CGO_ENABLED=0 go build -ldflags "-X main.version=$(git describe --tags --always)" -o nodedata ./cmd/nodedata
CGO_ENABLED=0 go build -ldflags "-X main.version=$(git describe --tags --always)" -o nodedata-fleet ./cmd/nodedata-fleet
CGO_ENABLED=0 go build -ldflags "-X main.version=$(git describe --tags --always)" -o nodedata-pelican ./cmd/nodedata-pelican
```
