#!/usr/bin/env bash
# Run all five Go services natively against localhost Postgres/Redis.
#   scripts/dev.sh         build + start (logs: /tmp/quizbox-<svc>.log)
#   scripts/dev.sh stop    stop everything
# Needs Postgres (schema from db/init.sql) and Redis on localhost, e.g.
#   docker compose -f docker-compose.dev.yml up -d postgres redis
# Ports (CONTRACT.md): gateway 8080, frontend 8081, account 8082, quiz 8083, game 8084.
# Binaries are built to bin/ first (instead of `go run`) so stop can kill them
# reliably: `go run` leaves the compiled child alive when its parent is killed.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
SVCS="account quiz game frontend gateway"
port() { case "$1" in gateway) echo 8080;; frontend) echo 8081;; account) echo 8082;; quiz) echo 8083;; game) echo 8084;; esac; }

stop() {
  for s in $SVCS; do
    pf="/tmp/quizbox-$s.pid"
    if [ -f "$pf" ]; then
      pid="$(cat "$pf")"
      if kill -0 "$pid" 2>/dev/null; then kill "$pid" 2>/dev/null && echo "stopped $s ($pid)"; fi
      rm -f "$pf"
    fi
  done
}

if [ "${1:-}" = stop ]; then stop; exit 0; fi

stop
mkdir -p bin
export DATABASE_URL="${DATABASE_URL:-postgres://quizbox:quizbox@localhost:5432/quizbox?sslmode=disable}"
export REDIS_ADDR="${REDIS_ADDR:-localhost:6379}"
export SEED_DEMO="${SEED_DEMO:-true}"
export NODE_NAME="${NODE_NAME:-$(hostname)}"

for s in $SVCS; do
  go build -o "bin/$s" "./services/$s"
done
for s in $SVCS; do
  PORT="$(port "$s")" POD_NAME="$s-dev" nohup "bin/$s" >"/tmp/quizbox-$s.log" 2>&1 &
  echo $! >"/tmp/quizbox-$s.pid"
  echo "started $s on :$(port "$s") (pid $!, log /tmp/quizbox-$s.log)"
done
sleep 1
for s in $SVCS; do
  kill -0 "$(cat "/tmp/quizbox-$s.pid")" 2>/dev/null || echo "WARNING: $s exited, see /tmp/quizbox-$s.log" >&2
done
echo "open http://localhost:8080   (demo login: demo@quizbox.local / demo1234)"
echo "stop with: scripts/dev.sh stop"
