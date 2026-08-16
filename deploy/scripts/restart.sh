#!/bin/sh
set -eu
. "$(CDPATH= cd -- "$(dirname "$0")" && pwd)/lib.sh"
lock_or_reexec "$@"
validate_release
compose restart
compose up -d --wait --wait-timeout "${GOALGO_WAIT_TIMEOUT:-300}"
health_check
smoke_test
