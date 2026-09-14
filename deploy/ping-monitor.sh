#!/bin/bash
# ══════════════════════════════════════════════════════════════
#  Ping Monitor — keeps mesh nodes alive + auto-restart
#  Run via cron: */5 * * * * /opt/mesh/ping-monitor.sh
# ══════════════════════════════════════════════════════════════

COORD_URL="${COORDINATOR_URL:-http://localhost:7700}"
LOG="/var/log/mesh-ping.log"
DRIVE_MOUNT="/mnt/gdrive"

log() { echo "$(date '+%Y-%m-%d %H:%M:%S') $1" >> $LOG; }

# ── 1. Check coordinator ──
if curl -sf "$COORD_URL/api/ping" > /dev/null 2>&1; then
    STATUS=$(curl -s "$COORD_URL/api/status")
    NODES=$(echo $STATUS | jq -r '.active_nodes // 0')
    TASKS=$(echo $STATUS | jq -r '.total_tasks // 0')
    DONE=$(echo $STATUS | jq -r '.done_tasks // 0')
    log "OK — coordinator alive: $NODES nodes, $DONE/$TASKS tasks"
else
    log "FAIL — coordinator down, restarting..."
    systemctl restart mesh-coordinator 2>/dev/null || true
    sleep 3
    if curl -sf "$COORD_URL/api/ping" > /dev/null 2>&1; then
        log "RECOVERED — coordinator restarted successfully"
    else
        log "CRITICAL — coordinator still down after restart"
    fi
fi

# ── 2. Check Google Drive mount ──
if mountpoint -q $DRIVE_MOUNT 2>/dev/null; then
    log "OK — gdrive mounted at $DRIVE_MOUNT"
else
    log "WARN — gdrive not mounted, attempting mount..."
    rclone mount gdrive: $DRIVE_MOUNT --daemon --allow-other --vfs-cache-mode full 2>/dev/null
    sleep 2
    if mountpoint -q $DRIVE_MOUNT 2>/dev/null; then
        log "OK — gdrive mounted successfully"
    else
        log "FAIL — gdrive mount failed"
    fi
fi

# ── 3. Check disk space ──
DISK_USAGE=$(df -h / | awk 'NR==2{print $5}' | tr -d '%')
if [ "$DISK_USAGE" -gt 90 ]; then
    log "WARN — disk usage at ${DISK_USAGE}%"
fi

# ── 4. Check worker processes ──
WORKERS=$(pgrep -c "mesh.exe.*worker" 2>/dev/null || echo 0)
if [ "$WORKERS" -eq 0 ]; then
    log "WARN — no worker processes running"
fi
