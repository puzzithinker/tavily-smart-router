#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

echo ""
echo "  _______          _       __  __           _   "
echo " |__   __|        | |     |  \\/  |         | |  "
echo "    | | ___  ___  | |__   | \\  / | ___  ___| |_ "
echo "    | |/ _ \\/ _ \\ | '_ \\  | |\\/| |/ _ \\/ __| __|"
echo "    | |  __/  __/ | |_) | | |  | |  __/\\__ \\ |_ "
echo "    |_|\\___|\\___| |_.__/  |_|  |_|\\___||___/\\__|"
echo ""
echo "============================================="
echo " One-time setup for Raspberry Pi 4 / Linux"
echo "============================================="
echo ""

# --- Checks ---
echo "[1/5] Checking prerequisites..."

for cmd in docker git curl; do
    if ! command -v "$cmd" &> /dev/null; then
        echo "  ERROR: $cmd is not installed."
        echo "  Install it with: sudo apt-get install -y $cmd"
        exit 1
    fi
done

if ! docker compose version &> /dev/null; then
    echo "  ERROR: docker compose v2 is not available."
    echo "  Install it with: sudo apt-get install -y docker-compose-plugin"
    exit 1
fi

if groups | grep -q docker; then
    echo "  Docker:     OK"
else
    echo "  WARNING: User is not in the 'docker' group."
    echo "  Run: sudo usermod -aG docker $USER && newgrp docker"
    echo "  Then re-run this script."
    exit 1
fi

echo "  Git:        OK"
echo "  Docker:     OK"
echo "  Compose:    OK"
echo "  curl:       OK"

# --- Config ---
echo ""
echo "[2/5] Setting up configuration..."

CONFIG_FILE="${SCRIPT_DIR}/config.json"

if [ ! -f "$CONFIG_FILE" ]; then
    if [ -z "${TAVILY_KEYS:-}" ]; then
        echo ""
        echo "  You need at least one Tavily API key."
        echo "  Get one at: https://tavily.com/"
        echo "  Enter your key(s), comma-separated (or set TAVILY_KEYS env var):"
        echo -n "  Keys: "
        read -r KEYS_INPUT
        if [ -z "$KEYS_INPUT" ]; then
            echo "  ERROR: No keys provided. Set TAVILY_KEYS and re-run."
            exit 1
        fi
    else
        KEYS_INPUT="$TAVILY_KEYS"
    fi

    JSON_KEYS="["
    FIRST=true
    IFS=','
    for KEY in $KEYS_INPUT; do
        KEY=$(echo "$KEY" | xargs)
        if [ -z "$KEY" ]; then
            continue
        fi
        if [ "$FIRST" = true ]; then
            FIRST=false
        else
            JSON_KEYS="$JSON_KEYS, "
        fi
        JSON_KEYS="$JSON_KEYS\"$KEY\""
    done
    unset IFS
    JSON_KEYS="$JSON_KEYS]"

    cat > "$CONFIG_FILE" <<EOF
{
  "listen_addr": "0.0.0.0:8082",
  "upstream_base": "https://api.tavily.com",
  "keys": $JSON_KEYS,
  "strategy": "least_used",
  "cooldown_sec": 300,
  "max_fails_before_cooldown": 3,
  "health_check_timeout_seconds": 10,
  "admin_user": "admin",
  "admin_pass": "",
  "enable_prometheus": true,
  "enable_request_log": false,
  "log_file": ""
}
EOF

    echo "  Created config.json with $(echo "$KEYS_INPUT" | tr ',' '\n' | grep -c .) key(s)."
else
    echo "  config.json already exists."
    if grep -q '"enable_prometheus": false' "$CONFIG_FILE"; then
        sed -i 's/"enable_prometheus": false/"enable_prometheus": true/' "$CONFIG_FILE"
        echo "  Prometheus enabled."
    fi
fi

# --- Shared network ---
echo ""
echo "[3/5] Creating shared Docker network..."

docker network create smart-routers 2>/dev/null || true
echo "  Network 'smart-routers' ready."

# --- Build ---
echo ""
echo "[4/5] Building Docker image (this takes a few minutes on first run)..."

docker compose build 2>&1 | tail -5

# --- Start ---
echo ""
echo "[5/5] Starting services..."

docker compose down 2>/dev/null || true
docker compose up -d

# --- Wait for services ---
echo ""
echo "  Waiting for router to start..."

MAX_WAIT=30
WAITED=0

while [ $WAITED -lt $MAX_WAIT ]; do
    if curl -sf http://127.0.0.1:8082/health > /dev/null 2>&1; then
        break
    fi
    sleep 2
    WAITED=$((WAITED + 2))
    echo "  Waiting... ($WAITED/${MAX_WAIT}s)"
done

# --- Verify ---
echo ""
IP_ADDR=$(hostname -I 2>/dev/null | awk '{print $1}')

if curl -sf http://127.0.0.1:8082/health > /dev/null 2>&1; then
    echo "  ✅ Router:    http://${IP_ADDR}:8082/health"
else
    echo "  ❌ Router:    Not responding"
fi

# --- Systemd service ---
SERVICE_FILE="${SCRIPT_DIR}/deploy/systemd/tavily-router.service"

if [ -f "$SERVICE_FILE" ]; then
    sed "s|__WORKINGDIR__|${SCRIPT_DIR}|" "$SERVICE_FILE" | sudo tee /etc/systemd/system/tavily-router.service > /dev/null
    sudo systemctl daemon-reload
    sudo systemctl enable tavily-router
    echo "  ✅ systemd service installed (auto-start on boot)"
else
    echo "  ⚠️  systemd unit file not found, skipping"
fi

echo ""
echo "============================================="
echo " Setup Complete!"
echo "============================================="
echo ""
echo " Endpoints:"
echo ""
echo "   Router:        http://${IP_ADDR}:8082"
echo "   Health:        http://${IP_ADDR}:8082/health"
echo "   Admin Stats:   http://${IP_ADDR}:8082/admin/stats"
echo "   Metrics:       http://${IP_ADDR}:8082/metrics"
echo ""
echo " Point your Tavily client at:"
echo "   http://${IP_ADDR}:8082"
echo ""
echo " Monitoring (shared with opencode-smart-router):"
echo "   Prometheus:    http://${IP_ADDR}:9090"
echo "   Grafana:       http://${IP_ADDR}:3000"
echo ""
echo " The Tavily Router dashboard is auto-provisioned in Grafana."
echo " Look for 'Tavily Smart Router' in Grafana → Dashboards."
echo ""
echo " Useful commands:"
echo "   docker compose logs -f                    Follow all logs"
echo "   docker compose logs tavily-router         Router logs only"
echo "   docker compose restart tavily-router      Restart router"
echo "   docker compose down                        Stop router"
echo "   docker compose up -d                       Start router"
echo "   sudo systemctl status tavily-router        Service status"
echo ""