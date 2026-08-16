#!/bin/sh
set -eu

if [ -n "${JWT_PUBLIC_KEY_FILE:-}" ]; then
    CWXU_JWT_PUBLIC_KEY=$(cat "$JWT_PUBLIC_KEY_FILE")
    export CWXU_JWT_PUBLIC_KEY
fi
exec "$@"
