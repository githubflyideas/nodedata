#!/usr/bin/env bash
# T-SAFE-01 外部验证。
#
# 代码内的 OpenedPaths() 审计可以被绕过（直接 syscall、或审计层本身有 bug），
# 因此禁读清单必须用 strace 从进程外部验证。这一项不可用代码内测试替代。
#
#     sudo ./run_strace_test.sh ./bin/snapshot-collector
#
# 退出码 0 = 通过。非 0 = 验收不通过。

set -uo pipefail

BIN="${1:?用法: $0 <采集器二进制路径>}"
DURATION="${2:-120}"      # 秒。至少覆盖 100 轮全局采集 + 12 轮进程采集
TRACE=$(mktemp /tmp/snapshot-strace.XXXXXX)

FORBIDDEN=(
  "/proc/net/nf_conntrack"
  "/proc/net/tcp"
  "/proc/net/tcp6"
  "/proc/net/udp"
  "/proc/net/udp6"
  "/proc/net/unix"
  "/proc/slabinfo"
  "/proc/kpageflags"
  "/proc/kpagecount"
)

command -v strace >/dev/null || { echo "需要 strace"; exit 2; }

echo "追踪 $BIN，持续 ${DURATION}s ..."

# -f 跟踪子线程；Go runtime 是多线程的，漏掉 -f 会漏报
strace -f -e trace=openat,open,openat2 -o "$TRACE" \
  timeout "$DURATION" "$BIN" --once-per-second >/dev/null 2>&1

echo "分析 $(wc -l < "$TRACE") 条 syscall 记录 ..."

FAIL=0
for path in "${FORBIDDEN[@]}"; do
  # 匹配 open 系列调用中出现的完整路径（带引号，避免前缀误匹配）
  hits=$(grep -cE "open(at|at2)?\(.*\"${path}\"" "$TRACE" || true)
  if [ "$hits" -gt 0 ]; then
    echo "  ✗ [硬] $path — 被打开 $hits 次"
    grep -E "open(at|at2)?\(.*\"${path}\"" "$TRACE" | head -3 | sed 's/^/      /'
    FAIL=1
  else
    echo "  ✓ $path"
  fi
done

# conntrack 必须走 O(1) 的 sysctl 而非全表
if [ -e /proc/sys/net/netfilter/nf_conntrack_count ]; then
  if ! grep -qE "open(at|at2)?\(.*nf_conntrack_count" "$TRACE"; then
    echo "  ✗ conntrack 数量未从 nf_conntrack_count 取得"
    FAIL=1
  else
    echo "  ✓ conntrack 走 sysctl"
  fi
fi

# 顺带检查：不得 fork/exec 外部命令
if strace -f -e trace=execve -o "${TRACE}.exec" \
     timeout 20 "$BIN" --once-per-second >/dev/null 2>&1; then
  n=$(grep -c "execve(" "${TRACE}.exec" || true)
  if [ "$n" -gt 1 ]; then   # 1 = 自身启动
    echo "  ✗ [硬] 调用了外部命令（execve × $((n-1))）"
    grep "execve(" "${TRACE}.exec" | tail -n +2 | head -5 | sed 's/^/      /'
    FAIL=1
  else
    echo "  ✓ 无外部命令调用"
  fi
fi

rm -f "$TRACE" "${TRACE}.exec"

if [ "$FAIL" -ne 0 ]; then
  echo
  echo "T-SAFE-01 未通过。这些文件的读取代价是 O(表长)，"
  echo "在大表时会使采集器自身成为故障源。"
  exit 1
fi

echo
echo "T-SAFE-01 通过。"
