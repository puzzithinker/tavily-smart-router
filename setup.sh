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
echo "[1/9] Checking prerequisites..."

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
echo "[2/9] Setting up configuration..."

CONFIG_FILE="${SCRIPT_DIR}/config.json"

if [ ! -f "$CONFIG_FILE" ]; then
    echo "  Creating config.json from config.example.json..."
    cp config.example.json config.json

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
        # Convert comma-separated to JSON array
        JSON_KEYS=$(echo "$KEYS_INPUT" | sed 's/,/","/g' | sed 's/^/"/;s/$/"/' | sed 's/ //g')
        sed -i "s/\"tvly-YOUR-API-KEY-HERE\"/[$JSON_KEYS]/" "$CONFIG_FILE"
    else
        echo "  Using TAVILY_KEYS from environment (${#TAVILY_KEYS} chars)"
    fi

    # Enable Prometheus for monitoring
    sed -i 's/"enable_prometheus": false/"enable_prometheus": true/' "$CONFIG_FILE"
    echo "  Prometheus enabled."
else
    echo "  config.json already exists."
    # Ensure Prometheus is enabled
    if grep -q '"enable_prometheus": false' "$CONFIG_FILE"; then
        sed -i 's/"enable_prometheus": false/"enable_prometheus": true/' "$CONFIG_FILE"
        echo "  Prometheus enabled."
    fi
fi

# --- Prometheus config ---
echo ""
echo "[3/9] Creating Prometheus configuration..."

mkdir -p "${SCRIPT_DIR}/prometheus"

cat > "${SCRIPT_DIR}/prometheus/prometheus.yml" << 'EOF'
global:
  scrape_interval: 15s
  evaluation_interval: 15s

scrape_configs:
  - job_name: 'tavily-router'
    static_configs:
      - targets: ['tavily-router:8082']
        labels:
          instance: 'rpi4'
EOF

echo "  Written: prometheus/prometheus.yml"

# --- Grafana provisioning ---
echo ""
echo "[4/9] Creating Grafana provisioning..."

mkdir -p "${SCRIPT_DIR}/grafana/provisioning/datasources"
mkdir -p "${SCRIPT_DIR}/grafana/provisioning/dashboards/json"

GRAFANA_PASSWORD="${GRAFANA_ADMIN_PASSWORD:-admin}"

cat > "${SCRIPT_DIR}/grafana/provisioning/datasources/datasource.yml" << 'EOF'
apiVersion: 1
datasources:
  - name: Prometheus
    type: prometheus
    access: proxy
    url: http://prometheus:9090
    isDefault: true
    editable: true
EOF

cat > "${SCRIPT_DIR}/grafana/provisioning/dashboards/dashboard.yml" << 'EOF'
apiVersion: 1
providers:
  - name: 'Tavily Router'
    orgId: 1
    folder: ''
    type: file
    disableDeletion: false
    editable: true
    options:
      path: /etc/grafana/provisioning/dashboards/json
      foldersFromFilesStructure: false
EOF

cat > "${SCRIPT_DIR}/grafana/provisioning/dashboards/json/tavily-router.json" << 'DASHBOARD'
{
  "annotations": {"list": []},
  "editable": true,
  "fiscalYearStartMonth": 0,
  "graphTooltip": 1,
  "id": null,
  "links": [],
  "panels": [
    {
      "title": "Request Rate",
      "type": "timeseries",
      "gridPos": {"h": 8, "w": 12, "x": 0, "y": 0},
      "targets": [{"expr": "sum(rate(tavily_router_requests_total[5m]))", "legendFormat": "req/s"}],
      "fieldConfig": {"defaults": {"unit": "reqps"}}
    },
    {
      "title": "Success Rate",
      "type": "stat",
      "gridPos": {"h": 8, "w": 6, "x": 12, "y": 0},
      "targets": [{"expr": "sum(rate(tavily_router_requests_total{status_group=\"2xx\"}[5m])) / sum(rate(tavily_router_requests_total[5m]))", "legendFormat": "success rate"}],
      "fieldConfig": {"defaults": {"unit": "percentunit", "thresholds": {"steps": [{"value": null, "color": "red"}, {"value": 0.9, "color": "yellow"}, {"value": 0.95, "color": "green"}]}}}
    },
    {
      "title": "Key Health",
      "type": "stat",
      "gridPos": {"h": 8, "w": 6, "x": 18, "y": 0},
      "targets": [{"expr": "tavily_router_key_healthy", "legendFormat": "{{key}}"}]
    },
    {
      "title": "Latency P50 / P95 / P99",
      "type": "timeseries",
      "gridPos": {"h": 8, "w": 12, "x": 0, "y": 8},
      "targets": [
        {"expr": "histogram_quantile(0.5, sum(rate(tavily_router_request_duration_seconds_bucket[5m])) by (le))", "legendFormat": "p50"},
        {"expr": "histogram_quantile(0.95, sum(rate(tavily_router_request_duration_seconds_bucket[5m])) by (le))", "legendFormat": "p95"},
        {"expr": "histogram_quantile(0.99, sum(rate(tavily_router_request_duration_seconds_bucket[5m])) by (le))", "legendFormat": "p99"}
      ],
      "fieldConfig": {"defaults": {"unit": "s"}}
    },
    {
      "title": "Requests by Status",
      "type": "timeseries",
      "gridPos": {"h": 8, "w": 12, "x": 12, "y": 8},
      "targets": [{"expr": "sum by (status_group) (rate(tavily_router_requests_total[5m]))", "legendFormat": "{{status_group}}"}]
    },
    {
      "title": "Key Usage",
      "type": "timeseries",
      "gridPos": {"h": 8, "w": 12, "x": 0, "y": 16},
      "targets": [{"expr": "sum by (key) (rate(tavily_router_key_usage_total[5m]))", "legendFormat": "{{key}}"}]
    },
    {
      "title": "Cooldown Events",
      "type": "timeseries",
      "gridPos": {"h": 8, "w": 12, "x": 12, "y": 16},
      "targets": [{"expr": "sum by (key) (rate(tavily_router_key_cooldown_total[5m]))", "legendFormat": "{{key}}"}]
    },
    {
      "title": "Upstream Errors",
      "type": "timeseries",
      "gridPos": {"h": 8, "w": 12, "x": 0, "y": 24},
      "targets": [{"expr": "sum by (error_type) (rate(tavily_router_upstream_errors_total[5m]))", "legendFormat": "{{error_type}}"}]
    },
    {
      "title": "Key Errors",
      "type": "timeseries",
      "gridPos": {"h": 8, "w": 12, "x": 12, "y": 24},
      "targets": [{"expr": "sum by (key) (rate(tavily_router_requests_total{status_group=~\"4xx|5xx\"}[5m]))", "legendFormat": "{{key}}"}]
    }
  ],
  "refresh": "10s",
  "schemaVersion": 39,
  "tags": ["tavily", "proxy"],
  "templating": {"list": []},
  "time": {"from": "now-1h", "to": "now"},
  "title": "Tavily Smart Router",
  "uid": "tavily-router"
}
DASHBOARD

echo "  Written: datasource, dashboard provider, and dashboard JSON"

# Update docker-compose.yml with Grafana password
sed -i "s/GF_SECURITY_ADMIN_PASSWORD:.*/GF_SECURITY_ADMIN_PASSWORD: ${GRAFANA_PASSWORD}/" "${SCRIPT_DIR}/docker-compose.yml" 2>/dev/null || true

# --- Build ---
echo ""
echo "[5/9] Building Docker image (this takes a few minutes on first run)..."

docker compose build 2>&1 | tail -5

# --- Start ---
echo ""
echo "[6/9] Starting services..."

docker compose down 2>/dev/null || true
docker compose up -d

# --- Wait for services ---
echo ""
echo "[7/9] Waiting for services to start..."

MAX_WAIT=30
WAITED=0
ROUTER_UP=false

while [ $WAITED -lt $MAX_WAIT ]; do
    if curl -sf http://127.0.0.1:8082/health > /dev/null 2>&1; then
        ROUTER_UP=true
        break
    fi
    sleep 2
    WAITED=$((WAITED + 2))
    echo "  Waiting... ($WAITED/${MAX_WAIT}s)"
done

# --- Verify ---
echo ""
echo "[8/9] Verifying services..."
echo ""

print_status() {
    local name="$1"
    local url="$2"
    local label="$3"
    if curl -sf "$url" > /dev/null 2>&1; then
        echo "  ✅ $name:    $label"
    else
        echo "  ❌ $name:    Not responding ($label)"
    fi
}

IP_ADDR=$(hostname -I 2>/dev/null | awk '{print $1}')

print_status "Router"     "http://127.0.0.1:8082/health"        "http://${IP_ADDR}:8082/health"
print_status "Metrics"    "http://127.0.0.1:8082/metrics"       "http://${IP_ADDR}:8082/metrics"
print_status "Prometheus" "http://127.0.0.1:9090/-/healthy"     "http://${IP_ADDR}:9090"
print_status "Grafana"     "http://127.0.0.1:3000/api/health"   "http://${IP_ADDR}:3000"

# --- Systemd service ---
echo ""
echo "[9/9] Installing systemd service..."

SERVICE_FILE="${SCRIPT_DIR}/deploy/systemd/tavily-router.service"

if [ ! -f "$SERVICE_FILE" ]; then
    echo "  WARNING: systemd unit file not found at ${SERVICE_FILE}"
    echo "  Skipping systemd setup. You can start services with: docker compose up -d"
else
    sed "s|__WORKINGDIR__|${SCRIPT_DIR}|" "$SERVICE_FILE" | sudo tee /etc/systemd/system/tavily-router.service > /dev/null
    sudo systemctl daemon-reload
    sudo systemctl enable tavily-router
    echo "  systemd service installed and enabled."
    echo "  Services will auto-start on boot."
fi

echo ""
echo "============================================="
echo " Setup Complete!"
echo "============================================="
echo ""
echo " Endpoints (LAN access from other hosts):"
echo ""
echo "   Router:        http://${IP_ADDR}:8082"
echo "   Health:        http://${IP_ADDR}:8082/health"
echo "   Admin Stats:   http://${IP_ADDR}:8082/admin/stats"
echo "   Prometheus:    http://${IP_ADDR}:9090"
echo "   Grafana:       http://${IP_ADDR}:3000"
echo ""
echo " Grafana login:"
echo "   Username: admin"
echo "   Password: ${GRAFANA_PASSWORD}"
echo ""
echo " The 'Tavily Smart Router' dashboard is auto-provisioned."
echo " Find it in Grafana → Dashboards."
echo ""
echo " Point your Tavily client at:"
echo "   http://${IP_ADDR}:8082"
echo ""
echo " Useful commands:"
echo "   docker compose logs -f                    Follow all logs"
echo "   docker compose logs tavily-router         Router logs only"
echo "   docker compose restart tavily-router      Restart router"
echo "   docker compose down                        Stop all services"
echo "   docker compose up -d                       Start all services"
echo "   sudo systemctl status tavily-router        Service status"
echo "   sudo systemctl restart tavily-router      Restart via systemd"
echo ""