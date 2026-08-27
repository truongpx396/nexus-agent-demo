-- The role every migration up to here runs as (POSTGRES_USER=nexus, from
-- the official postgres image) is a SUPERUSER. Row-level security is
-- unconditionally bypassed for a superuser — no ENABLE, no FORCE, and no
-- policy can change that (this is documented Postgres behavior, not a bug
-- to work around at the policy level). ENABLE + FORCE only matters for a
-- normal role; it never applies to a superuser or a role with BYPASSRLS.
--
-- So the actual enforcement point is: the application connects as a
-- SEPARATE, ordinary role that owns nothing and cannot bypass anything.
-- Migrations continue to run as the superuser (DDL needs owner privileges
-- anyway); every tenant-scoped runtime query — everything that goes
-- through store.Store.InTenantTx — runs as nexus_app.
CREATE ROLE nexus_app LOGIN PASSWORD 'nexus_app' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;

GRANT USAGE ON SCHEMA public TO nexus_app;

-- Applies to every table THIS migration role (nexus) creates in `public`
-- from this point forward — every later migration's CREATE TABLE
-- automatically grants nexus_app these privileges with no per-migration
-- boilerplate. (It has no retroactive effect on tables that already
-- exist when this runs, which is why this file is 0000: it must be the
-- first migration ever applied.)
ALTER DEFAULT PRIVILEGES FOR ROLE nexus IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO nexus_app;

ALTER DEFAULT PRIVILEGES FOR ROLE nexus IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO nexus_app;
