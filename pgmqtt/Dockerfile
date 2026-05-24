# pgmqtt image built from a published GitHub release tarball.
# Defaults to the 0.4.0-rc1 release; override with --build-arg PGMQTT_VERSION=...
ARG PG_MAJOR=16
FROM postgres:${PG_MAJOR}-bookworm

ARG PG_MAJOR=16
ARG PGMQTT_VERSION=0.4.0-rc1
ARG TARGETARCH

ADD https://github.com/RayElg/pgmqtt/releases/download/${PGMQTT_VERSION}/pgmqtt-pg${PG_MAJOR}-${TARGETARCH}.tar.gz /tmp/pgmqtt.tar.gz

RUN set -eux; \
    tar -xzf /tmp/pgmqtt.tar.gz -C /tmp; \
    src="/tmp/pgmqtt-pg${PG_MAJOR}-${TARGETARCH}"; \
    cp "${src}/lib/pgmqtt.so" "/usr/lib/postgresql/${PG_MAJOR}/lib/"; \
    cp "${src}/extension/"*   "/usr/share/postgresql/${PG_MAJOR}/extension/"; \
    rm -rf /tmp/pgmqtt.tar.gz "${src}"

RUN { \
      echo "wal_level = logical"; \
      echo "max_replication_slots = 10"; \
      echo "max_wal_senders = 10"; \
      echo "shared_preload_libraries = 'pgmqtt'"; \
      echo "log_min_messages = warning"; \
    } >> /usr/share/postgresql/postgresql.conf.sample; \
    { \
      echo "local   all             all                                     trust"; \
      echo "host    all             all             all                     trust"; \
    } >> /usr/share/postgresql/pg_hba.conf.sample

RUN printf '#!/bin/bash\npsql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "CREATE EXTENSION IF NOT EXISTS pgmqtt;"\n' \
      > /docker-entrypoint-initdb.d/01-create-extension.sh \
    && chmod +x /docker-entrypoint-initdb.d/01-create-extension.sh

EXPOSE 5432 8080 1883 9001
