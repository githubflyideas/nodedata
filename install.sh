#!/bin/bash

# install.sh v0.2.0 — nodedata L0 deployment bootstrap
# Features: dependency check, smoke test, systemd integration

set -e

VERSION="0.2.0"
NODEDATA_PORT="${NODEDATA_PORT:-8888}"
NODEDATA_INTERVAL="${NODEDATA_INTERVAL:-30s}"
NODEDATA_CLICKHOUSE="${NODEDATA_CLICKHOUSE:-clickhouse://localhost:9000}"
SYSTEMD_MODE=false

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

print_status() {
    echo -e "${GREEN}[✓]${NC} $1"
}

print_error() {
    echo -e "${RED}[✗]${NC} $1" >&2
}

print_warn() {
    echo -e "${YELLOW}[⚠]${NC} $1"
}

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --systemd)
            SYSTEMD_MODE=true
            shift
            ;;
        --port)
            NODEDATA_PORT="$2"
            shift 2
            ;;
        --interval)
            NODEDATA_INTERVAL="$2"
            shift 2
            ;;
        --ch-port)
            CH_PORT="$2"
            shift 2
            ;;
        *)
            echo "Unknown option: $1"
            exit 1
            ;;
    esac
done

# === 1. Dependency Check ===

echo "=== Dependency Check ==="

# Check Go version
if ! command -v go &> /dev/null; then
    print_error "Go not installed. Install Go 1.21+:"
    echo "  curl -sSL https://dl.google.com/go/go1.23.1.linux-amd64.tar.gz | tar -C /usr/local -xz"
    exit 1
fi

GO_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
GO_MAJOR=$(echo $GO_VERSION | cut -d. -f1)
GO_MINOR=$(echo $GO_VERSION | cut -d. -f2)

if [[ $GO_MAJOR -lt 1 ]] || [[ $GO_MAJOR -eq 1 && $GO_MINOR -lt 21 ]]; then
    print_error "Go 1.21+ required (found $GO_VERSION)"
    exit 1
fi
print_status "Go $GO_VERSION"

# Check ClickHouse (optional)
if command -v clickhouse &> /dev/null; then
    print_status "ClickHouse found"
else
    print_warn "ClickHouse not found. Download: https://github.com/ClickHouse/ClickHouse/releases"
    print_warn "  Or: curl -fsSL https://repo.clickhouse.com/clickhouse-common-static.tar.gz | tar xz"
fi

# Check Caddy (optional)
if command -v caddy &> /dev/null; then
    print_status "Caddy found"
else
    print_warn "Caddy not found. Download: https://caddyserver.com/download"
fi

# === 2. Build ===

echo ""
echo "=== Building nodedata ==="

if [[ ! -f "go.mod" ]]; then
    print_error "go.mod not found. Run from nodedata root directory."
    exit 1
fi

if go build -o ./nodedata ./cmd/nodedata 2>&1 | head -5; then
    print_status "Build successful"
else
    print_error "Build failed"
    exit 1
fi

if [[ ! -f "./nodedata" ]]; then
    print_error "Binary not found after build"
    exit 1
fi

# === 3. Smoke Test ===

echo ""
echo "=== Smoke Test (20s) ==="

./nodedata check --timeout 5s > /tmp/nodedata_check.log 2>&1

if grep -q "✓ All checks passed" /tmp/nodedata_check.log; then
    print_status "L0 checks: ALL PASS"
elif grep -q "⚠ System has warnings" /tmp/nodedata_check.log; then
    print_warn "L0 checks: WARNINGS (see below)"
    cat /tmp/nodedata_check.log | grep "⚠"
else
    print_error "L0 checks: FAILURES (see below)"
    cat /tmp/nodedata_check.log | grep "✗"
    # Don't exit, continue anyway
fi

cat /tmp/nodedata_check.log

# === 4. Systemd Integration (if requested) ===

if [[ "$SYSTEMD_MODE" == "true" ]]; then
    echo ""
    echo "=== Systemd Integration ==="

    INSTALL_DIR="${PWD}"
    SERVICE_FILE="/etc/systemd/system/nodedata.service"

    if [[ ! -w "/etc/systemd/system" ]]; then
        print_error "No permission to write to /etc/systemd/system. Re-run with sudo."
        exit 1
    fi

    cat > $SERVICE_FILE << EOF
[Unit]
Description=nodedata L0 Sanity Check & Metrics
After=network.target

[Service]
Type=simple
User=$(whoami)
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/nodedata check --timeout 10s
Restart=on-failure
RestartSec=60

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable nodedata.service
    print_status "Systemd service installed: $SERVICE_FILE"

    # Try to start
    if systemctl start nodedata.service 2>&1; then
        print_status "Service started"
        sleep 2
        systemctl status nodedata.service --no-pager || true
    else
        print_error "Failed to start service"
    fi
fi

# === 5. Summary ===

echo ""
echo "=== Setup Complete ==="
print_status "Binary: $PWD/nodedata"
print_status "Version: $VERSION"
print_status "Check command: ./nodedata check [--timeout 5s]"

if [[ "$SYSTEMD_MODE" == "true" ]]; then
    print_status "Systemd enabled: systemctl {start,stop,status} nodedata"
fi

