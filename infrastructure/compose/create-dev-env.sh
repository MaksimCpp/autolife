#!/bin/sh
set -eu

if [ -e .env ]; then
  echo ".env already exists" >&2
  exit 1
fi

umask 077
set -C
admin=$(openssl rand -hex 24)
api=$(openssl rand -hex 24)
worker=$(openssl rand -hex 24)
migrate=$(openssl rand -hex 24)
s3_access=$(openssl rand -hex 16)
s3_secret=$(openssl rand -hex 24)
auth_secret=$(openssl rand -hex 32)
cat > .env <<VARS
AUTOLIFE_DB_ADMIN_PASSWORD=$admin
AUTOLIFE_DB_API_PASSWORD=$api
AUTOLIFE_DB_WORKER_PASSWORD=$worker
AUTOLIFE_DB_MIGRATION_PASSWORD=$migrate
AUTOLIFE_API_DATABASE_URL=postgres://autolife_api:$api@postgres:5432/autolife?sslmode=disable
AUTOLIFE_WORKER_DATABASE_URL=postgres://autolife_worker:$worker@postgres:5432/autolife?sslmode=disable
AUTOLIFE_MIGRATION_DATABASE_URL=postgres://autolife_migrate:$migrate@postgres:5432/autolife?sslmode=disable
AUTOLIFE_AUTH_SECRET=$auth_secret
AUTOLIFE_S3_ACCESS_KEY=$s3_access
AUTOLIFE_S3_SECRET_KEY=$s3_secret
VARS
