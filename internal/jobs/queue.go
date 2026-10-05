package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

// Queue is the producer side of River: inserting jobs and inspecting them.
//
// It is split from Service because the CLI needs exactly this and nothing else.
// `ai-reviewer review <ref>` inserts a job and exits; it must not start worker
// pools, elect a leader or run periodic jobs — a River client with no Queues
// and no Workers configured is insert-only and never needs Start.
type Queue struct {
	client *river.Client[pgx.Tx]
}

// NewQueue builds an insert-only client over an existing pool.
func NewQueue(pool *pgxpool.Pool) (*Queue, error) {
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		return nil, fmt.Errorf("create river client: %w", err)
	}
	return &Queue{client: client}, nil
}

// InsertResult reports what an enqueue actually did. River returns the existing
// job row when an insert is deduped, which is what lets a caller explain the
// collapse instead of reporting a phantom success.
type InsertResult struct {
	Job *rivertype.JobRow
	// Deduplicated is true when an equivalent job was already in flight and this
	// insert was skipped. Job then describes that pre-existing job.
	Deduplicated bool
}

// ID is the job id — the pre-existing one when the insert was deduped.
func (r InsertResult) ID() int64 {
	if r.Job == nil {
		return 0
	}
	return r.Job.ID
}

func result(res *rivertype.JobInsertResult) InsertResult {
	if res == nil {
		return InsertResult{}
	}
	return InsertResult{Job: res.Job, Deduplicated: res.UniqueSkippedAsDuplicate}
}

func (q *Queue) insert(ctx context.Context, args river.JobArgs) (InsertResult, error) {
	res, err := q.client.Insert(ctx, args, nil)
	if err != nil {
		return InsertResult{}, fmt.Errorf("enqueue %s: %w", args.Kind(), err)
	}
	return result(res), nil
}

// EnqueueScan queues a scan pass.
func (q *Queue) EnqueueScan(ctx context.Context, args ScanArgs) (InsertResult, error) {
	return q.insert(ctx, args)
}

// EnqueueScanRepo queues one repository's pass.
func (q *Queue) EnqueueScanRepo(ctx context.Context, args ScanRepoArgs) (InsertResult, error) {
	return q.insert(ctx, args)
}

// EnqueueReview queues one merge request review.
func (q *Queue) EnqueueReview(ctx context.Context, args ReviewArgs) (InsertResult, error) {
	return q.insert(ctx, args)
}

// EnqueuePublishReview queues publication of an already-persisted review. Used
// by the scan_repo sweep; the review job itself inserts it transactionally.
func (q *Queue) EnqueuePublishReview(ctx context.Context, args PublishReviewArgs) (InsertResult, error) {
	return q.insert(ctx, args)
}

// EnqueueDigest queues one team's digest for one slot.
func (q *Queue) EnqueueDigest(ctx context.Context, args DigestArgs) (InsertResult, error) {
	return q.insert(ctx, args)
}

// EnqueueSlackSend queues delivery of one digest part.
func (q *Queue) EnqueueSlackSend(ctx context.Context, args SlackSendArgs) (InsertResult, error) {
	return q.insert(ctx, args)
}

// EnqueueSlackCommand queues the answer to one in-chat command.
func (q *Queue) EnqueueSlackCommand(ctx context.Context, args SlackCommandArgs) (InsertResult, error) {
	return q.insert(ctx, args)
}

// EnqueuePublishReviewTx inserts the publication job inside the caller's
// transaction — the §10.4 atomic hand-off. See OnPersist.
func (q *Queue) EnqueuePublishReviewTx(ctx context.Context, tx pgx.Tx, reviewID uuid.UUID) (InsertResult, error) {
	res, err := q.client.InsertTx(ctx, tx, PublishReviewArgs{ReviewID: reviewID}, nil)
	if err != nil {
		return InsertResult{}, fmt.Errorf("enqueue %s for review %s: %w", KindPublishReview, reviewID, err)
	}
	return result(res), nil
}

// JobGet loads one job row.
func (q *Queue) JobGet(ctx context.Context, id int64) (*rivertype.JobRow, error) {
	row, err := q.client.JobGet(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get job %d: %w", id, err)
	}
	return row, nil
}

// IsTerminal reports whether a job has reached a state it will not leave on its
// own. Cancelled and discarded are included: `--wait` must not hang on a job
// that will never run again.
func IsTerminal(state rivertype.JobState) bool {
	switch state {
	case rivertype.JobStateCompleted, rivertype.JobStateCancelled, rivertype.JobStateDiscarded:
		return true
	default:
		return false
	}
}

// ErrWaitTimeout is returned by Wait when the job is still not terminal.
var ErrWaitTimeout = errors.New("timed out waiting for the job to finish")

// Wait polls a job until it reaches a terminal state, the context ends, or the
// budget runs out. Polling (rather than River's event subscription) is what a
// separate process can do: `--wait` runs in the CLI, while the worker that will
// finish the job lives in another replica entirely.
func (q *Queue) Wait(ctx context.Context, id int64, poll, budget time.Duration) (*rivertype.JobRow, error) {
	if poll <= 0 {
		poll = time.Second
	}
	deadline := time.Now().Add(budget)

	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		row, err := q.JobGet(ctx, id)
		if err != nil {
			return nil, err
		}
		if IsTerminal(row.State) {
			return row, nil
		}
		if budget > 0 && time.Now().After(deadline) {
			return row, ErrWaitTimeout
		}
		select {
		case <-ctx.Done():
			return row, ctx.Err()
		case <-ticker.C:
		}
	}
}

// DecodeArgs unmarshals a job row's args. It is how the CLI inspects the job
// its own insert collapsed into — River hands back the pre-existing row, and
// its args say whether that run is going to publish.
func DecodeArgs[T river.JobArgs](row *rivertype.JobRow) (T, error) {
	var args T
	if row == nil {
		return args, errors.New("no job row")
	}
	if err := json.Unmarshal(row.EncodedArgs, &args); err != nil {
		return args, fmt.Errorf("decode %s args: %w", row.Kind, err)
	}
	return args, nil
}
