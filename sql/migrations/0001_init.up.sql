-- Initial schema: the operational state of the service.
--
-- Postgres holds what *this service did*, not a copy of GitLab: which head SHA
-- of which MR has already been reviewed and with what outcome, which findings
-- reached GitLab, and the journal of digest runs. GitLab stays the source of
-- truth about the merge requests themselves. River owns its own river_* tables
-- and versions them separately (rivermigrate), so they are not defined here.
--
-- This is deliberately one migration rather than several: nothing has ever been
-- deployed from this schema, so splitting the initial state across files would
-- record a history that never happened. Everything after this point gets its own
-- numbered pair.

-- ---------------------------------------------------------------------------
-- uuidv7(): use the server's own, or install a shim where there is none
-- ---------------------------------------------------------------------------
-- Every table below uses `id uuid PRIMARY KEY DEFAULT uuidv7()`, but uuidv7()
-- became a built-in only in PostgreSQL 18. Production runs PG17 and the
-- supported floor is PG16, where the CREATE TABLE statements would otherwise
-- fail with `function uuidv7() does not exist`.
--
-- The guard is a *capability* check against pg_proc, not a server-version
-- comparison: it asks the only question that matters, and it keeps working if a
-- future build ever backports the function.
--
-- The point of the guard is that exactly ONE uuidv7() exists on any server. An
-- unconditional `CREATE OR REPLACE FUNCTION public.uuidv7()` would leave PG18
-- carrying two: it cannot replace the built-in (different schema, and pg_catalog
-- is not writable anyway), so it would only add a pl/pgSQL twin that nothing
-- calls — pg_catalog is searched *before* the schemas in search_path, so the
-- unqualified `uuidv7()` in the DEFAULTs below binds to the built-in. Verified
-- on PG 18.4 by replacing the shim with a constant-returning function: inserted
-- rows still got real time-ordered ids. Dead weight that looks load-bearing is
-- how the next reader ends up "fixing" the wrong function.
--
-- The DEFAULT expressions are therefore deliberately left unqualified: each
-- server binds to whichever implementation it has. Note the binding is resolved
-- once, at CREATE TABLE time, and stored in pg_attrdef as a parse tree with a
-- fixed OID — it is not re-resolved per INSERT, so a later search_path change
-- cannot move an existing column onto a different implementation.
DO $do$
BEGIN
    IF EXISTS (
        SELECT 1
          FROM pg_proc p
          JOIN pg_namespace n ON n.oid = p.pronamespace
         WHERE n.nspname = 'pg_catalog'
           AND p.proname = 'uuidv7'
           AND p.pronargs = 0
    ) THEN
        RETURN; -- PG18+: the server has its own, in C. Install nothing.
    END IF;

    -- PG16/17: pure pl/pgSQL, no extensions — gen_random_uuid() has been built
    -- in since PG13. CREATE OR REPLACE so re-applying the file by hand is safe.
    EXECUTE $fn$
        CREATE OR REPLACE FUNCTION public.uuidv7() RETURNS uuid
        LANGUAGE plpgsql VOLATILE
        AS $body$
        DECLARE
          v      bytea;
          ms_num numeric;  -- Unix time in milliseconds, with the fraction kept
          ms     bigint;   -- whole milliseconds -> the 48-bit timestamp field
          rand_a int;      -- 12 bits of sub-millisecond precision
        BEGIN
          ms_num := extract(epoch FROM clock_timestamp()) * 1000;
          ms     := floor(ms_num);
          -- RFC 9562 method 3: spend the 12 rand_a bits on extra clock
          -- precision instead of randomness. Without this, ids generated inside
          -- one millisecond order arbitrarily, and PostgreSQL 18's built-in
          -- uuidv7() does exactly this (verified: consecutive calls in one
          -- millisecond return a11, a2d, a33, a36 … in rand_a). Dev runs 18 and
          -- production 17, so leaving it out would mean B-tree insert locality
          -- and `ORDER BY created_at, id` tiebreaks behave differently in the
          -- two environments — the class of difference this shim exists to
          -- avoid. The remaining 62 bits stay random, which is ample.
          rand_a := floor((ms_num - ms) * 4096);

          -- Start from a random v4 UUID; its byte 8 already carries the correct
          -- RFC 9562 variant bits (10xx), which v7 reuses unchanged.
          v := uuid_send(gen_random_uuid());
          -- Overlay the first 48 bits (6 bytes) with the big-endian Unix-ms
          -- timestamp (int8send is 8 bytes; the top 2 are zero for any realistic
          -- date, so they are dropped). This is what makes the ids time-ordered.
          v := overlay(v PLACING substring(int8send(ms) FROM 3) FROM 1 FOR 6);
          -- Bytes 6-7 = version nibble (0111 = 7) followed by rand_a.
          -- 112 = 0x70 sets the version; the low nibble of byte 6 takes rand_a's
          -- high 4 bits and byte 7 its low 8.
          v := set_byte(v, 6, 112 | (rand_a >> 8));
          v := set_byte(v, 7, rand_a & 255);
          RETURN encode(v, 'hex')::uuid;
        END;
        $body$;
    $fn$;
END
$do$;

-- ---------------------------------------------------------------------------
-- Reviews
-- ---------------------------------------------------------------------------
-- One row = one review attempt of one MR head SHA.
CREATE TABLE mr_reviews (
    id             uuid PRIMARY KEY DEFAULT uuidv7(),
    project_id     bigint      NOT NULL,
    project_path   text        NOT NULL,
    team           text        NOT NULL,
    mr_iid         bigint      NOT NULL,
    head_sha       text        NOT NULL,
    base_sha       text        NOT NULL DEFAULT '',
    start_sha      text        NOT NULL DEFAULT '',
    status         text        NOT NULL,
    findings_count int         NOT NULL DEFAULT 0,
    risk_level     text        NOT NULL DEFAULT '',
    summary        text        NOT NULL DEFAULT '',
    pipeline_json  jsonb       NOT NULL DEFAULT '{}',
    risk_json      jsonb       NOT NULL DEFAULT '{}',
    cost_usd       numeric(12,6) NOT NULL DEFAULT 0,
    duration_ms    bigint      NOT NULL DEFAULT 0,
    error          text        NOT NULL DEFAULT '',
    attempt        int         NOT NULL DEFAULT 1,  -- attempt number for this SHA
    -- How many times the section 6.3 sweep has re-enqueued publication for this
    -- row. It bounds the sweep: a review that cannot be published is retried a
    -- fixed number of times and then goes to 'abandoned', instead of consuming a
    -- publish slot on every scan until the end of time.
    publish_attempts int       NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- "this SHA has already been reviewed" — the single hot-path query.
-- Rows with status='failed' are excluded from the index: there may be several
-- of them (one per attempt), and they are exactly what feeds the failure
-- counter behind the backoff ladder (plan section 6.5).
CREATE UNIQUE INDEX mr_reviews_success_uniq
    ON mr_reviews (project_id, mr_iid, head_sha)
    WHERE status <> 'failed';

-- Failed-attempt counter per SHA: a cheap COUNT for the backoff (section 6.5).
CREATE INDEX mr_reviews_failed_idx
    ON mr_reviews (project_id, mr_iid, head_sha, created_at)
    WHERE status = 'failed';

-- One row = one finding produced by a review. Findings that reached GitLab
-- carry note_id/published_at; everything else (dry-run, publication that never
-- made it) stays NULL, which is what the dedupe query keys on.
CREATE TABLE mr_findings (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    review_id    uuid NOT NULL REFERENCES mr_reviews(id) ON DELETE CASCADE,
    project_id   bigint NOT NULL,
    mr_iid       bigint NOT NULL,
    fingerprint  text   NOT NULL,
    severity     text   NOT NULL,
    category     text   NOT NULL,
    file_path    text   NOT NULL,
    title        text   NOT NULL,
    body         text   NOT NULL,
    position_json jsonb NOT NULL DEFAULT '{}',
    pass         text   NOT NULL DEFAULT '',
    verification text   NOT NULL DEFAULT '',
    note_id      bigint,                 -- id of the published GitLab note (NULL in dry-run)
    published_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Dedupe per MR: the same finding is not published again on a new SHA.
-- Inserts ALWAYS carry `ON CONFLICT (project_id, mr_iid, fingerprint)`: a
-- duplicate is an expected outcome (the engine may fail to filter it when
-- reading discussions degrades), not a reason to fail persistence of the whole
-- review. See sql/queries/finding/findings.sql for why the conflict action is
-- DO UPDATE, and for the two conditions that keep it from stealing a finding
-- out from under a publication that is already in flight.
CREATE UNIQUE INDEX mr_findings_fp_uniq ON mr_findings (project_id, mr_iid, fingerprint);
-- Publication's working set. PublishReview reads the still-unpublished findings
-- of one review twice per run (once to post, once to prove nothing was left
-- behind before marking the review succeeded), and a foreign key does not create
-- an index on its own — so without this both reads, and every ON DELETE CASCADE,
-- scan a table that grows with every finding ever published. Partial: a row is
-- only interesting here until it has a note_id.
CREATE INDEX mr_findings_unpublished_idx ON mr_findings (review_id) WHERE note_id IS NULL;

-- ---------------------------------------------------------------------------
-- Digest
-- ---------------------------------------------------------------------------
-- One row = one digest run (a scheduled slot or an explicit manual repeat).
CREATE TABLE digest_runs (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    team        text        NOT NULL,
    slot        text        NOT NULL,       -- '09:00' | '16:30' | 'manual'
    run_date    date        NOT NULL,       -- the date in Europe/Moscow
    -- attempt: 0 is the scheduled run of the slot; 1,2,... are explicit manual
    -- repeats. This is what makes `ai-reviewer digest --force` possible without
    -- dropping the uniqueness that protects against duplicates across N replicas.
    attempt     int         NOT NULL DEFAULT 0,
    -- Values: see dbtypes.DigestRunStatus. Enforced in the application.
    status      text        NOT NULL,
    parts       int         NOT NULL DEFAULT 0,
    mr_count    int         NOT NULL DEFAULT 0,
    error       text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX digest_runs_slot_uniq ON digest_runs (team, run_date, slot, attempt);

-- One row = one Slack message (a digest may be split into several parts).
-- The payload lives here rather than in the job arguments: Block Kit is bulky
-- and river_job.args is no place for kilobytes of JSON.
CREATE TABLE digest_messages (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    digest_run_id uuid NOT NULL REFERENCES digest_runs(id) ON DELETE CASCADE,
    part_no       int  NOT NULL,
    parts_total   int  NOT NULL,
    channel       text NOT NULL,
    payload       jsonb NOT NULL,           -- ready-made Block Kit blocks
    status        text NOT NULL,
    slack_ts      text NOT NULL DEFAULT '', -- ts of the sent message
    error         text NOT NULL DEFAULT '',
    sent_at       timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX digest_messages_part_uniq ON digest_messages (digest_run_id, part_no);
