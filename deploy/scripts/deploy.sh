#!/bin/sh
set -eu
. "$(CDPATH= cd -- "$(dirname "$0")" && pwd)/lib.sh"
lock_or_reexec "$@"

candidate=${1:-$GOALGO_ROOT/release.env}
validate_release "$candidate"
require_root_files
atomic_copy "$GOALGO_ROOT/release.env" "$GOALGO_ROOT/release.previous.env"
if [ "$candidate" != "$GOALGO_ROOT/release.env" ]; then
    atomic_copy "$candidate" "$GOALGO_ROOT/release.env"
fi

if compose pull && \
   compose up -d --wait --wait-timeout "${GOALGO_WAIT_TIMEOUT:-300}" && \
   health_check && smoke_test; then
    printf '%s\n' 'goalgo: deployment succeeded'
    exit 0
fi

printf '%s\n' 'goalgo: deployment failed; restoring previous release' >&2
if rollback_release; then
    die 'deployment failed; previous release restored'
fi
die 'deployment failed and automatic rollback was unavailable or failed'
