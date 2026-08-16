#!/bin/sh
set -eu
. "$(CDPATH= cd -- "$(dirname "$0")" && pwd)/lib.sh"
lock_or_reexec "$@"
require_command docker
require_command curl
require_root_files
validate_release
docker compose version >/dev/null
for path in config secrets data state logs; do
    [ -d "$GOALGO_ROOT/$path" ] || die "missing directory: $GOALGO_ROOT/$path"
done
for file in secrets/app.env secrets/jwt_private_key.pem secrets/jwt_public_key.pem secrets/postgres_password secrets/redis_password secrets/rabbitmq_password secrets/backup_encryption_key; do
    [ -r "$GOALGO_ROOT/$file" ] || die "missing or unreadable: $GOALGO_ROOT/$file"
done
compose config --quiet
printf '%s\n' 'goalgo: configuration is healthy'
