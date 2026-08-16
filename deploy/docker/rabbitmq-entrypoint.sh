#!/bin/sh
set -eu

RABBITMQ_DEFAULT_PASS=$(cat /run/secrets/rabbitmq_password)
export RABBITMQ_DEFAULT_PASS
exec docker-entrypoint.sh rabbitmq-server
