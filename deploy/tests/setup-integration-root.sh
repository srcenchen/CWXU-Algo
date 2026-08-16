#!/bin/sh
set -eu

root=${1:?usage: setup-integration-root.sh ROOT}
deploy=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
GOALGO_ALLOW_INSECURE_TEST_PERMISSIONS=1 sh "$deploy/scripts/provision-root.sh" "$root"

openssl genrsa -out "$root/secrets/jwt_private.pem" 2048 >/dev/null 2>&1
openssl rsa -in "$root/secrets/jwt_private.pem" -pubout -out "$root/secrets/jwt_public.pem" >/dev/null 2>&1
openssl rand 32 > "$root/secrets/backup_encryption_key"

postgres_password='Pg:@/?#[]!phase2a'
redis_password='Rd:@/?#[]!phase2a'
rabbitmq_password='Mq:@/?#[]!phase2a'

printf '%s' "$postgres_password" > "$root/secrets/postgres_password"
printf '%s' "$redis_password" > "$root/secrets/redis_password"
printf '%s' "$rabbitmq_password" > "$root/secrets/rabbitmq_password"

{
    printf '%s\n' 'POSTGRES_PASSWORD=Pg:@/?#[]!phase2a'
    printf '%s\n' 'USER_DATABASE_DSN=postgres://goalgo:Pg%3A%40%2F%3F%23%5B%5D%21phase2a@postgres:5432/algo_user?sslmode=disable&TimeZone=Asia/Shanghai'
    printf '%s\n' 'CORE_DATABASE_DSN=postgres://goalgo:Pg%3A%40%2F%3F%23%5B%5D%21phase2a@postgres:5432/algo_core_data?sslmode=disable&TimeZone=Asia/Shanghai'
    printf '%s\n' 'CWXU_CORE_DATABASE_SOURCE=postgres://goalgo:Pg%3A%40%2F%3F%23%5B%5D%21phase2a@postgres:5432/algo_core_data?sslmode=disable'
    printf '%s\n' 'CWXU_USER_DATABASE_SOURCE=postgres://goalgo:Pg%3A%40%2F%3F%23%5B%5D%21phase2a@postgres:5432/algo_user?sslmode=disable'
    printf '%s\n' 'REDIS_ADDR=redis:6379'
    printf '%s\n' 'REDIS_PASSWORD=Rd:@/?#[]!phase2a'
    printf '%s\n' 'AMQP_DSN=amqp://goalgo:Mq%3A%40%2F%3F%23%5B%5D%21phase2a@rabbitmq:5672/goalgo'
    printf '%s\n' 'CWXU_JWT_SECRET=phase2a-jwt-secret-at-least-32-characters'
    printf '%s\n' 'CWXU_CONFIG_ENCRYPTION_KEY=phase2a-config-key-at-least-32-chars'
    printf '%s\n' 'CWXU_BACKUP_ENABLED=false'
    printf '%s\n' "CWXU_JWT_PRIVATE_KEY='$(cat "$root/secrets/jwt_private.pem")'"
    printf '%s\n' "CWXU_JWT_PUBLIC_KEY='$(cat "$root/secrets/jwt_public.pem")'"
} > "$root/secrets/app.env"

{
    printf 'GOALGO_ROOT=%s\n' "$root"
    printf '%s\n' 'GOALGO_HTTP_BIND=127.0.0.1'
    printf '%s\n' 'GOALGO_HTTP_PORT=18988'
    printf '%s\n' 'POSTGRES_USER=goalgo'
    printf '%s\n' 'RABBITMQ_USER=goalgo'
    printf '%s\n' 'FRONTEND_IMAGE=goalgo-frontend:phase2a-review'
    printf '%s\n' 'GATEWAY_IMAGE=goalgo-gateway:phase2a-review'
    printf '%s\n' 'USER_IMAGE=goalgo-user:phase2a-review'
    printf '%s\n' 'CORE_DATA_IMAGE=goalgo-core-data:phase2a-review'
    printf '%s\n' 'AGENT_IMAGE=goalgo-agent:phase2a-review'
} > "$root/secrets/compose.env"

chmod 0600 "$root"/secrets/*
