#!/bin/sh
set -eu

root=${GOALGO_TEST_ROOT:?GOALGO_TEST_ROOT is required}
compose=${COMPOSE_CMD:-docker compose}
project=${COMPOSE_PROJECT_NAME:-goalgo-phase2a-test}
port=${GOALGO_HTTP_PORT:-18988}

cleanup() {
    $compose -p "$project" -f deploy/compose.yaml down --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

wait_http() {
    url=$1
    attempts=90
    while [ "$attempts" -gt 0 ]; do
        if curl -fsS "$url" >/dev/null; then
            return 0
        fi
        attempts=$((attempts - 1))
        sleep 2
    done
    return 1
}

$compose -p "$project" -f deploy/compose.yaml up -d --wait --wait-timeout 240
wait_http "http://127.0.0.1:$port/healthz"
curl -fsS "http://127.0.0.1:$port/" | grep -q '<div id="root">'
curl -fsS "http://127.0.0.1:$port/api/user/site/config" | grep -q '"title"'

for service in frontend gateway user core-data agent postgres redis rabbitmq consul nginx; do
    test "$($compose -p "$project" -f deploy/compose.yaml ps --status running -q "$service" | wc -l | tr -d ' ')" = 1
done

for database in algo_user algo_core_data sanenchen support; do
    $compose -p "$project" -f deploy/compose.yaml exec -T postgres \
        psql -U goalgo -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname='$database'" | grep -q 1
done

$compose -p "$project" -f deploy/compose.yaml exec -T consul consul members | grep -q alive
$compose -p "$project" -f deploy/compose.yaml exec -T agent wget -qO- http://127.0.0.1:8002/healthz | grep -q '"status":"ok"'
$compose -p "$project" -f deploy/compose.yaml exec -T agent wget -qO- http://127.0.0.1:8002/readyz | grep -q '"status":"ok"'
$compose -p "$project" -f deploy/compose.yaml exec -T core-data sh -c \
    'test -r /run/secrets/backup_encryption_key && test -w /var/lib/goalgo/backups && test "$(stat -c %a /run/goalgo/config.yaml)" = 600'
