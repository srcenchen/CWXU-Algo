#!/bin/sh
set -eu

root=${GOALGO_TEST_ROOT:?GOALGO_TEST_ROOT is required}
workspace=$(pwd)
exec docker run --rm \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "$workspace:$workspace:ro" \
    -v "$root:$root" \
    --env-file "$root/secrets/compose.env" \
    -w "$workspace" \
    docker:29-cli docker compose "$@"
