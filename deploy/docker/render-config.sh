#!/bin/sh
set -eu

if [ -n "${JWT_PRIVATE_KEY_FILE:-}" ]; then
    JWT_PRIVATE_KEY=$(awk '{printf "%s\\n", $0}' "$JWT_PRIVATE_KEY_FILE")
    export JWT_PRIVATE_KEY
fi
if [ -n "${JWT_PUBLIC_KEY_FILE:-}" ]; then
    JWT_PUBLIC_KEY=$(awk '{printf "%s\\n", $0}' "$JWT_PUBLIC_KEY_FILE")
    export JWT_PUBLIC_KEY
fi

template=$1
shift
install -d /run/goalgo
envsubst < "$template" > /run/goalgo/config.yaml
chmod 0600 /run/goalgo/config.yaml
exec "$@" -conf /run/goalgo/config.yaml
