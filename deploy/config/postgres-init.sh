#!/bin/sh
set -eu

databases="algo_user algo_core_data sanenchen support"

for database in $databases; do
    if ! psql --username "$POSTGRES_USER" --dbname postgres --tuples-only --command \
        "SELECT 1 FROM pg_database WHERE datname = '$database'" | grep -q 1; then
        createdb --username "$POSTGRES_USER" "$database"
    fi
done

for database in $databases; do
    psql --username "$POSTGRES_USER" --dbname "$database" --command \
        "CREATE EXTENSION IF NOT EXISTS vector"
done
