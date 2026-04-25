#!/bin/bash
set -e

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" -c "SELECT 'init'"

for db in netbird_auth netbird_events; do
    EXISTS=$(psql -t --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" -c "SELECT 1 FROM pg_database WHERE datname = '$db'")
    if [ -z "$EXISTS" ]; then
        psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" -c "CREATE DATABASE $db"
    fi
done