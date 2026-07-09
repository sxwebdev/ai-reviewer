-- cancel_requested: set to 1 when a stop is requested for a job that is already
-- running. The worker owning that job observes the flag in its control loop and
-- cancels the job's context (killing the claude subprocess), then writes a
-- terminal 'cancelled' status. Queued jobs are cancelled synchronously without
-- this flag. ClaimJob ignores rows with cancel_requested=1 so a stop-marked job
-- that gets requeued (e.g. by RecoverStuckJobs) is never picked up again.
ALTER TABLE jobs ADD COLUMN cancel_requested INTEGER NOT NULL DEFAULT 0;
