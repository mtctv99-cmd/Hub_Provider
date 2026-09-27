#!/bin/bash
# ==========================================================
# Copilot Auto-Refresh Daemon / Runner
# Runs scripts/auto_copilot_login.py every 12 hours (43200s)
# ==========================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT_DIR"

INTERVAL_SEC=${REFRESH_INTERVAL_SEC:-43200} # 12 hours
PYTHON_BIN=${PYTHON_BIN:-""}

if [ -z "$PYTHON_BIN" ]; then
  if [ -f "$ROOT_DIR/venv/bin/python3" ]; then
    PYTHON_BIN="$ROOT_DIR/venv/bin/python3"
  elif command -v python3 >/dev/null 2>&1; then
    PYTHON_BIN="python3"
  elif command -v python >/dev/null 2>&1; then
    PYTHON_BIN="python"
  else
    echo "[ERROR] python3 or python executable not found" >&2
    exit 1
  fi
fi

TARGET_SCRIPT="$SCRIPT_DIR/auto_copilot_login.py"
if [ ! -f "$TARGET_SCRIPT" ]; then
  echo "[ERROR] Target script not found: $TARGET_SCRIPT" >&2
  exit 1
fi

run_refresh() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] Starting Copilot auto-login refresh..."
  if "$PYTHON_BIN" "$TARGET_SCRIPT"; then
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] Copilot refresh completed successfully."
  else
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [WARNING] Copilot refresh exited with status $?." >&2
  fi
}

if [ "${1:-}" = "--once" ]; then
  run_refresh
  exit 0
fi

trap 'echo "[$(date "+%Y-%m-%d %H:%M:%S")] Stopping Copilot auto-refresh loop..."; exit 0' SIGINT SIGTERM

echo "[$(date '+%Y-%m-%d %H:%M:%S')] Copilot auto-refresh daemon started (interval: ${INTERVAL_SEC}s / 12h)."

while true; do
  run_refresh
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] Sleeping for ${INTERVAL_SEC} seconds (12h)..."
  sleep "$INTERVAL_SEC" &
  wait $!
done
