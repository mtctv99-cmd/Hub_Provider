#!/bin/bash
# ==========================================================
# Sever Provider - Unified AI Orchestrator Daemon Launcher
# ==========================================================

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT_DIR"

export PATH=/home/reup/.local/go/bin:/usr/local/bin:/usr/bin:/bin:/home/reup/.local/bin:$PATH
export SERVER_PORT=${SERVER_PORT:-8080}
if [ -z "$MASTER_KEY" ]; then
  echo "Notice: MASTER_KEY is not set. An ephemeral key will be generated (or set MASTER_KEY for persistent encryption)."
fi
export PIPER_BIN=${PIPER_BIN:-"/home/reup/.local/bin/piper"}
export PIPER_MODELS_DIR=${PIPER_MODELS_DIR:-"/media/reup/Data_sv4/ttsoioer/models"}

PIDFILE="$ROOT_DIR/data/sever_provider.pid"
LOGFILE="$ROOT_DIR/data/sever_provider.log"
mkdir -p "$ROOT_DIR/data"

case "${1:-start}" in
  start)
    if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
      echo "Sever Provider is already running (PID $(cat "$PIDFILE"))"
      exit 0
    fi
    
    echo "Starting Sever Provider on port ${SERVER_PORT}..."
    BINARY="$ROOT_DIR/sever-provider"
    if [ ! -f "$BINARY" ] && [ -f "$ROOT_DIR/sever_provider" ]; then
      BINARY="$ROOT_DIR/sever_provider"
    fi
    setsid "$BINARY" -config "$ROOT_DIR/config" > "$LOGFILE" 2>&1 &
    PID=$!
    echo $PID > "$PIDFILE"
    
    # Wait for ready
    for i in $(seq 1 15); do
      if curl -sf "http://127.0.0.1:${SERVER_PORT}/health" > /dev/null 2>&1; then
        echo "Sever Provider is ready (PID $PID, Port $SERVER_PORT)"
        echo "Web Dashboard: http://localhost:${SERVER_PORT}/web/"
        exit 0
      fi
      sleep 0.3
    done
    echo "Warning: Sever Provider started with PID $PID but health check not responding yet. Check $LOGFILE"
    ;;

  stop)
    if [ -f "$PIDFILE" ]; then
      PID=$(cat "$PIDFILE")
      echo "Stopping Sever Provider (PID $PID)..."
      kill -SIGTERM "$PID" 2>/dev/null || true
      for i in $(seq 1 10); do
        if ! kill -0 "$PID" 2>/dev/null; then
          break
        fi
        sleep 0.5
      done
      rm -f "$PIDFILE"
      echo "Stopped successfully."
    else
      echo "Sever Provider not running."
    fi
    ;;

  restart)
    $0 stop
    sleep 1
    $0 start
    ;;

  status)
    if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
      echo "Sever Provider is RUNNING (PID $(cat "$PIDFILE"), Port $SERVER_PORT)"
      curl -s "http://127.0.0.1:${SERVER_PORT}/health"
      echo ""
    else
      echo "Sever Provider is STOPPED"
      exit 1
    fi
    ;;

  log)
    tail -f "$LOGFILE"
    ;;

  *)
    echo "Usage: $0 {start|stop|restart|status|log}"
    exit 1
    ;;
esac
