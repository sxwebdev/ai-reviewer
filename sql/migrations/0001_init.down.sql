-- Reverse order of the up migration: children before parents, so the drops do
-- not depend on CASCADE.
DROP TABLE IF EXISTS digest_messages CASCADE;

DROP TABLE IF EXISTS digest_runs CASCADE;

DROP TABLE IF EXISTS mr_findings CASCADE;

DROP TABLE IF EXISTS mr_reviews CASCADE;

-- Only PG16/17 ever had a shim installed (see the up migration's capability
-- guard), so on PG18 this is a no-op — hence IF EXISTS rather than a second
-- version check. Dropped last: the tables above carry DEFAULT uuidv7(), so the
-- function must outlive them.
--
-- The schema qualifier is load-bearing on PostgreSQL 18: an unqualified
-- `DROP FUNCTION IF EXISTS uuidv7()` resolves to the pg_catalog built-in and
-- fails with `cannot drop function uuidv7() because it is required by the
-- database system` (verified on PG 18.4).
DROP FUNCTION IF EXISTS public.uuidv7();
