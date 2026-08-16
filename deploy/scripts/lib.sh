#!/bin/sh

GOALGO_ROOT=${GOALGO_ROOT:-/opt/goalgo}
GOALGO_LOCK_FILE=${GOALGO_LOCK_FILE:-/run/lock/goalgo-ops.lock}
export GOALGO_ROOT

die() {
    printf 'goalgo: %s\n' "$*" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

lock_or_reexec() {
    [ "${GOALGO_LOCK_HELD:-}" = 1 ] && return 0
    require_command flock
    lock_dir=${GOALGO_LOCK_FILE%/*}
    [ "$lock_dir" = "$GOALGO_LOCK_FILE" ] || mkdir -p "$lock_dir"
    exec flock -n "$GOALGO_LOCK_FILE" env GOALGO_LOCK_HELD=1 GOALGO_ROOT="$GOALGO_ROOT" GOALGO_LOCK_FILE="$GOALGO_LOCK_FILE" "$0" "$@"
}

require_root_files() {
    [ -f "$GOALGO_ROOT/compose.yaml" ] || die "missing $GOALGO_ROOT/compose.yaml"
    [ -f "$GOALGO_ROOT/.env" ] || die "missing $GOALGO_ROOT/.env"
    [ -f "$GOALGO_ROOT/release.env" ] || die "missing $GOALGO_ROOT/release.env"
}

compose() {
    require_root_files
    docker compose \
        --env-file "$GOALGO_ROOT/.env" \
        --env-file "$GOALGO_ROOT/release.env" \
        -f "$GOALGO_ROOT/compose.yaml" "$@"
}

validate_release() {
    release_file=${1:-$GOALGO_ROOT/release.env}
    [ -f "$release_file" ] || die "release file not found: $release_file"
    expected='FRONTEND_IMAGE GATEWAY_IMAGE USER_IMAGE CORE_DATA_IMAGE AGENT_IMAGE'
    prefix='registry.cn-hangzhou.aliyuncs.com/sanenchen/goalgo@sha256:'
    for variable in $expected; do
        count=$(sed -n "/^${variable}=/p" "$release_file" | wc -l | tr -d ' ')
        [ "$count" = 1 ] || die "$release_file must define $variable exactly once"
        value=$(sed -n "s/^${variable}=//p" "$release_file")
        printf '%s\n' "$value" | grep -Eq "^${prefix}[0-9a-f]{64}$" || \
            die "$variable must be $prefix followed by 64 lowercase hex characters"
    done
    assignments=$(sed -n '/^[A-Za-z_][A-Za-z0-9_]*=/p' "$release_file" | wc -l | tr -d ' ')
    [ "$assignments" = 5 ] || die "$release_file must contain only the five application image assignments"
}

atomic_copy() {
    source_file=$1
    destination=$2
    temporary="${destination}.tmp.$$"
    cp "$source_file" "$temporary"
    chmod 0600 "$temporary"
    mv -f "$temporary" "$destination"
}

health_check() {
    compose ps --status running >/dev/null
}

smoke_test() {
    bind=$(sed -n 's/^GOALGO_HTTP_BIND=//p' "$GOALGO_ROOT/.env")
    port=$(sed -n 's/^GOALGO_HTTP_PORT=//p' "$GOALGO_ROOT/.env")
    [ -n "$port" ] || port=8988
    case "${bind:-127.0.0.1}" in
        0.0.0.0|::|'[::]') bind=127.0.0.1 ;;
    esac
    curl -fsS --max-time 10 "http://$bind:$port/healthz" >/dev/null
    curl -fsS --max-time 15 "http://$bind:$port/api/user/site/config" >/dev/null
}

rollback_release() {
    previous="$GOALGO_ROOT/release.previous.env"
    [ -f "$previous" ] || return 1
    atomic_copy "$previous" "$GOALGO_ROOT/release.env"
    validate_release "$GOALGO_ROOT/release.env"
    compose pull
    compose up -d --wait --wait-timeout "${GOALGO_WAIT_TIMEOUT:-300}"
    health_check
    smoke_test
}
