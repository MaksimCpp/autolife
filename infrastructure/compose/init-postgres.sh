#!/bin/sh
set -eu

psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --set ON_ERROR_STOP=1 \
  --set "db_name=$POSTGRES_DB" <<'SQL'
\getenv api_password AUTOLIFE_DB_API_PASSWORD
\getenv worker_password AUTOLIFE_DB_WORKER_PASSWORD
\getenv migrate_password AUTOLIFE_DB_MIGRATION_PASSWORD
CREATE ROLE autolife_api LOGIN PASSWORD :'api_password';
CREATE ROLE autolife_worker LOGIN PASSWORD :'worker_password';
CREATE ROLE autolife_migrate LOGIN PASSWORD :'migrate_password';
REVOKE ALL ON DATABASE :"db_name" FROM PUBLIC;
GRANT CONNECT ON DATABASE :"db_name" TO autolife_api, autolife_worker, autolife_migrate;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
CREATE SCHEMA platform AUTHORIZATION autolife_migrate;
CREATE SCHEMA queue AUTHORIZATION autolife_migrate;
GRANT USAGE ON SCHEMA platform, queue TO autolife_api, autolife_worker;
ALTER DEFAULT PRIVILEGES FOR ROLE autolife_migrate IN SCHEMA queue
    GRANT SELECT, INSERT ON TABLES TO autolife_api;
ALTER DEFAULT PRIVILEGES FOR ROLE autolife_migrate IN SCHEMA queue
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO autolife_worker;
ALTER DEFAULT PRIVILEGES FOR ROLE autolife_migrate IN SCHEMA queue
    GRANT USAGE, SELECT ON SEQUENCES TO autolife_api, autolife_worker;
SQL
