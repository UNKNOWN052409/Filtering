#!/bin/bash
# ══════════════════════════════════════════════════════════════
#  VPS Setup Script — BugsCloud Mesh Node
#  Run on a fresh Ubuntu/Debian VPS as root
# ══════════════════════════════════════════════════════════════
set -e

echo "═══ BugsCloud Mesh VPS Setup ═══"

# ── 1. System deps ──
echo "[1/7] Installing system dependencies..."
apt-get update -qq
apt-get install -y -qq curl wget git htop tmux jq unzip

# ── 2. Go (latest) ──
echo "[2/7] Installing Go..."
if ! command -v go &>/dev/null; then
    GO_VER="1.22.2"
    wget -q "https://go.dev/dl/go${GO_VER}.linux-amd64.tar.gz" -O /tmp/go.tar.gz
    tar -C /usr/local -xzf /tmp/go.tar.gz
    echo 'export PATH=$PATH:/usr/local/go/bin' >> /etc/profile.d/go.sh
    export PATH=$PATH:/usr/local/go/bin
fi
echo "  Go: $(go version)"

# ── 3. rclone ──
echo "[3/7] Installing rclone..."
if ! command -v rclone &>/dev/null; then
    curl -sL https://rclone.org/install.sh | bash
fi
echo "  rclone: $(rclone version | head -1)"

# ── 4. FUSE (for rclone mount) ──
echo "[4/7] Setting up FUSE..."
apt-get install -y -qq fuse
usermod -aG fuse $USER 2>/dev/null || true
echo "user_allow_other" >> /etc/fuse.conf

# ── 5. Deploy mesh binary ──
# BEFORE running this script, from your LOCAL machine:
#   scp mesh/mesh_linux root@VPS_IP:/opt/mesh/mesh   (create dir first: ssh root@VPS_IP mkdir -p /opt/mesh)
echo "[5/7] Deploying mesh binary..."
MESH_DIR="/opt/mesh"
mkdir -p $MESH_DIR
if [ ! -f $MESH_DIR/mesh ]; then
    echo "  ⚠ /opt/mesh/mesh not found — scp mesh_linux from local machine first (see comment above)"
fi
cp nodes.json $MESH_DIR/
chmod 600 $MESH_DIR/nodes.json
chmod +x $MESH_DIR/mesh 2>/dev/null || true

# ── 6. Systemd service ──
echo "[6/7] Creating systemd services..."

# Coordinator service
cat > /etc/systemd/system/mesh-coordinator.service << 'EOF'
[Unit]
Description=BugsCloud Mesh Coordinator
After=network.target

[Service]
Type=simple
WorkingDirectory=/opt/mesh
ExecStart=/opt/mesh/mesh -mode coordinator -config /opt/mesh/nodes.json
Restart=always
RestartSec=5
Environment=HOME=/root

[Install]
WantedBy=multi-user.target
EOF

# Worker service (uses NODE_ID env)
cat > /etc/systemd/system/mesh-worker.service << 'EOF'
[Unit]
Description=BugsCloud Mesh Worker
After=network.target

[Service]
Type=simple
WorkingDirectory=/opt/mesh
ExecStart=/opt/mesh/mesh -mode worker -config /opt/mesh/nodes.json -coordinator http://127.0.0.1:7700
Restart=always
RestartSec=10
Environment=NODE_ID=1
Environment=HOME=/root

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
echo "  Services created. Start with:"
echo "    systemctl start mesh-coordinator"
echo "    systemctl start mesh-worker"

# ── 7. Google Drive mount ──
echo "[7/7] Setting up Google Drive mount..."
MOUNT_DIR="/mnt/gdrive"
mkdir -p $MOUNT_DIR

# rclone config (user must paste their rclone.conf)
RCLONE_CONF="/root/.config/rclone/rclone.conf"
mkdir -p $(dirname $RCLONE_CONF)
if [ ! -f $RCLONE_CONF ]; then
    echo "  ⚠ rclone config not found at $RCLONE_CONF"
    echo "  Run: rclone config  and set up gdrive: remote"
    echo "  Then mount with: rclone mount gdrive: $MOUNT_DIR --daemon"
else
    echo "  rclone config found. Mounting..."
    rclone mount gdrive: $MOUNT_DIR --daemon --allow-other --vfs-cache-mode full
    echo "  Mounted at $MOUNT_DIR"
fi

echo ""
echo "═══ Setup Complete ═══"
echo ""
echo "Next steps:"
echo "  1. Configure rclone:  rclone config  (set up gdrive: remote)"
echo "  2. Mount drive:       rclone mount gdrive: /mnt/gdrive --daemon"
echo "  3. Start coordinator: systemctl start mesh-coordinator"
echo "  4. Start worker:      systemctl start mesh-worker"
echo "  5. Check status:      curl http://localhost:7700/api/status"
echo ""
echo "Upload combo file:"
echo "  curl -F 'file=@combos.txt' -F 'keywords=netflix.com spotify.com' http://localhost:7700/api/upload"
