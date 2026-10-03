#!/bin/sh
# Creates the engine's three database roles and its database (ADR-0008) on
# PostgreSQL's first start, with passwords from the environment. The SQL is
# the same as deploy/postgres/init-roles.sql; passwords are passed as psql
# variables, so they are quoted by psql and never pasted into the SQL.
#
# monitor_owner   owns the schema; used only to run migrations
# monitor_app     subject to row-level security; used by the api role
# monitor_system  BYPASSRLS; used by background roles and admin commands
set -eu

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres \
  -v owner_pw="$MONITOR_OWNER_PASSWORD" \
  -v app_pw="$MONITOR_APP_PASSWORD" \
  -v system_pw="$MONITOR_SYSTEM_PASSWORD" <<'SQL'
CREATE ROLE monitor_owner  LOGIN PASSWORD :'owner_pw';
CREATE ROLE monitor_app    LOGIN PASSWORD :'app_pw';
CREATE ROLE monitor_system LOGIN BYPASSRLS PASSWORD :'system_pw';

CREATE DATABASE monitor OWNER monitor_owner;

\connect monitor

REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO monitor_app, monitor_system;
SQL
