#!/bin/sh
set -eu

root=${1:-${GOALGO_ROOT:-/opt/goalgo}}
deploy=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)

command -v openssl >/dev/null 2>&1 || { printf '%s\n' 'openssl is required' >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { printf '%s\n' 'python3 is required' >&2; exit 1; }

install -d "$root/config" "$root/scripts" "$root/state" "$root/logs" "$root/restore"
install -d -m 0700 "$root/secrets"
install -d "$root/data/postgres" "$root/data/redis" "$root/data/rabbitmq" "$root/data/consul"
cp "$deploy/compose.yaml" "$root/compose.yaml"
cp "$deploy"/config/*.yaml "$root/config/"
cp "$deploy/config/postgres-init.sh" "$root/config/"
cp "$deploy/docker/nginx.conf" "$root/config/"
cp "$deploy/docker/rabbitmq-entrypoint.sh" "$root/config/"
cp "$deploy"/scripts/*.sh "$root/scripts/"
chmod 0755 "$root/config/postgres-init.sh" "$root/config/rabbitmq-entrypoint.sh"
chmod 0755 "$root"/scripts/*.sh

if [ ! -f "$root/.env" ]; then
    sed "s|^GOALGO_ROOT=.*|GOALGO_ROOT=$root|" "$deploy/env.example" >"$root/.env.tmp.$$"
    chmod 0600 "$root/.env.tmp.$$"
    mv "$root/.env.tmp.$$" "$root/.env"
fi

generate_secret() {
    secret_file=$1
    bytes=$2
    if [ ! -s "$secret_file" ]; then
        umask 077
        openssl rand -hex "$bytes" >"$secret_file.tmp.$$"
        mv "$secret_file.tmp.$$" "$secret_file"
    fi
    chmod 0600 "$secret_file"
}

generate_secret "$root/secrets/postgres_password" 32
generate_secret "$root/secrets/redis_password" 32
generate_secret "$root/secrets/rabbitmq_password" 32
generate_secret "$root/secrets/backup_encryption_key" 32
generate_secret "$root/secrets/jwt_secret" 32

if [ ! -s "$root/secrets/jwt_private_key.pem" ]; then
    umask 077
    openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "$root/secrets/jwt_private_key.pem.tmp.$$" >/dev/null 2>&1
    mv "$root/secrets/jwt_private_key.pem.tmp.$$" "$root/secrets/jwt_private_key.pem"
fi
if [ ! -s "$root/secrets/jwt_public_key.pem" ]; then
    umask 077
    openssl pkey -pubout -in "$root/secrets/jwt_private_key.pem" -out "$root/secrets/jwt_public_key.pem.tmp.$$" >/dev/null 2>&1
    mv "$root/secrets/jwt_public_key.pem.tmp.$$" "$root/secrets/jwt_public_key.pem"
fi
chmod 0600 "$root/secrets/jwt_private_key.pem" "$root/secrets/jwt_public_key.pem"

postgres_password=$(tr -d '\r\n' <"$root/secrets/postgres_password")
postgres_user=$(sed -n 's/^POSTGRES_USER=//p' "$root/.env" | tail -n 1)
[ -n "$postgres_user" ] || postgres_user=goalgo
uri_escape() { python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$1"; }
postgres_user_uri=$(uri_escape "$postgres_user")
postgres_password_uri=$(uri_escape "$postgres_password")
redis_password=$(tr -d '\r\n' <"$root/secrets/redis_password")
rabbitmq_password=$(tr -d '\r\n' <"$root/secrets/rabbitmq_password")
jwt_secret=$(tr -d '\r\n' <"$root/secrets/jwt_secret")
umask 077
{
    printf 'POSTGRES_USER=%s\n' "$postgres_user"
    printf 'USER_DATABASE_DSN=host=postgres user=%s password=%s dbname=algo_user port=5432 sslmode=disable TimeZone=Asia/Shanghai\n' "$postgres_user" "$postgres_password"
    printf 'CORE_DATABASE_DSN=host=postgres user=%s password=%s dbname=algo_core_data port=5432 sslmode=disable TimeZone=Asia/Shanghai\n' "$postgres_user" "$postgres_password"
	printf 'CWXU_BACKUP_PG_DSN=postgres://%s:%s@postgres:5432/postgres?sslmode=disable\n' "$postgres_user_uri" "$postgres_password_uri"
    printf 'REDIS_ADDR=redis:6379\nREDIS_PASSWORD=%s\n' "$redis_password"
    printf 'AMQP_DSN=amqp://goalgo:%s@rabbitmq:5672/goalgo\n' "$rabbitmq_password"
    printf 'CWXU_JWT_SECRET=%s\n' "$jwt_secret"
    printf 'JWT_PRIVATE_KEY_FILE=/run/secrets/jwt_private_key.pem\n'
    printf 'JWT_PUBLIC_KEY_FILE=/run/secrets/jwt_public_key.pem\n'
} >"$root/secrets/app.env.tmp.$$"
chmod 0600 "$root/secrets/app.env.tmp.$$"
mv "$root/secrets/app.env.tmp.$$" "$root/secrets/app.env"
unset postgres_password postgres_user postgres_user_uri postgres_password_uri redis_password rabbitmq_password jwt_secret

if [ "$(id -u)" -eq 0 ]; then
    chown 10001:10001 "$root/secrets/backup_encryption_key" "$root/secrets/jwt_private_key.pem" "$root/secrets/jwt_public_key.pem"
    chown -R 999:999 "$root/data/postgres"
    chown -R 999:1000 "$root/data/redis"
    chown -R 100:101 "$root/data/rabbitmq"
    chown -R 100:1000 "$root/data/consul"
elif [ "${GOALGO_ALLOW_INSECURE_TEST_PERMISSIONS:-}" = 1 ]; then
    chmod 0777 "$root/data/postgres" "$root/data/redis" "$root/data/rabbitmq" "$root/data/consul"
else
    printf '%s\n' 'provision-root.sh must run as root to assign container data ownership' >&2
    exit 1
fi
