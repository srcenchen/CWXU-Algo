#!/bin/sh
set -eu
. "$(CDPATH= cd -- "$(dirname "$0")" && pwd)/lib.sh"
lock_or_reexec "$@"

current="$GOALGO_ROOT/release.env"
previous="$GOALGO_ROOT/release.previous.env"
require_root_files
validate_release "$previous"
temporary="$GOALGO_ROOT/release.swap.$$"
atomic_copy "$current" "$temporary"
atomic_copy "$previous" "$current"
atomic_copy "$temporary" "$previous"
rm -f "$temporary"

if compose pull && compose up -d --wait --wait-timeout "${GOALGO_WAIT_TIMEOUT:-300}" && health_check && smoke_test; then
    printf '%s\n' 'goalgo: rollback succeeded'
    exit 0
fi

atomic_copy "$previous" "$current"
die 'rollback failed; release.env restored, services require operator inspection'
