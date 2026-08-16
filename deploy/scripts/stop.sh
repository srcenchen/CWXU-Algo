#!/bin/sh
set -eu
. "$(CDPATH= cd -- "$(dirname "$0")" && pwd)/lib.sh"
lock_or_reexec "$@"
compose stop
