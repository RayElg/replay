#!/bin/bash
# 02-create-replay-db.sh
# This runs after the built-in 01-create-extension.sh in the pgmqtt image.
# It creates the pgmqtt extension in the replay database (the built-in script
# only creates it in the default POSTGRES_DB which may differ).

set -e

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
    CREATE EXTENSION IF NOT EXISTS pgmqtt;
EOSQL
