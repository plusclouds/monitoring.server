-- One-time database setup (ADR-0008). Run as a PostgreSQL superuser, for
-- example: psql -v ON_ERROR_STOP=1 -f init-roles.sql
-- Replace every password, and store the three DSNs as secret files for
-- database.owner, database.app and database.system.
--
-- monitor_owner   owns the schema; used only to run migrations
-- monitor_app     subject to row-level security; used by the api role
-- monitor_system  BYPASSRLS; used by runner, engine, notifier, ingest,
--                 maintenance and the admin commands
--
-- The role names are fixed: migrations grant privileges to them by name.

CREATE ROLE monitor_owner  LOGIN PASSWORD 'change-me-owner';
CREATE ROLE monitor_app    LOGIN PASSWORD 'change-me-app';
CREATE ROLE monitor_system LOGIN BYPASSRLS PASSWORD 'change-me-system';

CREATE DATABASE monitor OWNER monitor_owner;

\connect monitor

-- The database owner owns the public schema (PostgreSQL 15+). Nobody else
-- may create objects in it.
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO monitor_app, monitor_system;
