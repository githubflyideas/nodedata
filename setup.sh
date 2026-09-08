#!/usr/bin/env bash
# nodedata 一键部署脚本
# 用法：bash setup.sh [--port=PORT]
# 默认 ClickHouse TCP 端口 9000；用 --port=19000 改用其他端口

set -e
DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR"

CH_PORT=9000
for arg in "$@"; do
  case $arg in
    --port=*) CH_PORT="${arg#*=}" ;;
  esac
done

DSN="clickhouse://localhost:${CH_PORT}/nodedata?dial_timeout=5s"

sep="────────────────────────────────────────────────────────"
echo "$sep"
echo "  nodedata 一键部署"
echo "$sep"

# ── 1. ClickHouse ─────────────────────────────────────────────
echo ""
echo "▶ 检查 ClickHouse..."

CH_BIN=""
if command -v clickhouse-server &>/dev/null; then
  CH_BIN="clickhouse-server"
elif [ -x "$DIR/clickhouse" ]; then
  CH_BIN="$DIR/clickhouse"
fi

if [ -z "$CH_BIN" ]; then
  echo "  ClickHouse 未找到，正在下载官方单文件版..."
  curl -sL https://clickhouse.com/ | sh
  CH_BIN="$DIR/clickhouse"
  echo "  ✓ 下载完成"
fi

# 检查是否已在运行
if "$DIR/clickhouse" client --port "$CH_PORT" --query "SELECT 1" &>/dev/null 2>&1 || \
   clickhouse-client --port "$CH_PORT" --query "SELECT 1" &>/dev/null 2>&1; then
  echo "  ✓ ClickHouse 已运行（端口 $CH_PORT）"
else
  echo "  启动 ClickHouse（端口 $CH_PORT）..."
  if [ -x "$DIR/clickhouse" ]; then
    "$DIR/clickhouse" server --daemon -- --tcp_port="$CH_PORT"
  else
    clickhouse-server --daemon -- --tcp_port="$CH_PORT"
  fi
  echo -n "  等待 ClickHouse 就绪"
  for i in $(seq 1 15); do
    sleep 1
    echo -n "."
    if "$DIR/clickhouse" client --port "$CH_PORT" --query "SELECT 1" &>/dev/null 2>&1 || \
       clickhouse-client --port "$CH_PORT" --query "SELECT 1" &>/dev/null 2>&1; then
      echo ""
      echo "  ✓ ClickHouse 启动成功（端口 $CH_PORT）"
      break
    fi
    if [ "$i" -eq 15 ]; then
      echo ""
      echo "  ✗ ClickHouse 启动超时，请手动检查"
      exit 1
    fi
  done
fi

# ── 2. 建库建表 ───────────────────────────────────────────────
echo ""
echo "▶ 初始化数据库..."
CLIENT_CMD=""
if [ -x "$DIR/clickhouse" ]; then
  CLIENT_CMD="$DIR/clickhouse client --port $CH_PORT"
else
  CLIENT_CMD="clickhouse-client --port $CH_PORT"
fi

$CLIENT_CMD --query "CREATE DATABASE IF NOT EXISTS nodedata"
$CLIENT_CMD --database nodedata < "$DIR/schema.sql"
echo "  ✓ 数据库 nodedata 就绪"

# ── 3. 启动 nodedata ──────────────────────────────────────────
echo ""
echo "▶ 启动 nodedata..."

if pgrep -x nodedata &>/dev/null; then
  echo "  ℹ nodedata 已在运行，跳过"
else
  nohup "$DIR/nodedata" \
    --web="$DIR" \
    --dsn="$DSN" \
    --wal-dir="$DIR" \
    >> "$DIR/nodedata.log" 2>&1 &
  sleep 1
  if pgrep -x nodedata &>/dev/null; then
    echo "  ✓ nodedata 已启动（日志：$DIR/nodedata.log）"
  else
    echo "  ✗ nodedata 启动失败，查看日志："
    tail -20 "$DIR/nodedata.log"
    exit 1
  fi
fi

# ── 4. 状态汇总 ───────────────────────────────────────────────
echo ""
echo "$sep"
echo "  部署完成"
echo "$sep"
echo "  ClickHouse    : localhost:$CH_PORT  ✓"
echo "  nodedata API  : http://127.0.0.1:9701"
echo "  前端页面      : http://127.0.0.1:9701/"
echo ""
echo "  修改端口示例（ClickHouse 改用 19000）："
echo "    bash setup.sh --port=19000"
echo ""
echo "  可选：用 Caddy 对外暴露到 :8888"
echo "    NODEDATA_WEB=$(pwd) caddy run --config $DIR/Caddyfile &"
echo "$sep"
