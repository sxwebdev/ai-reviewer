package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrReviewLocked means another review of the same head SHA holds the advisory
// lock — a scanner-queued job, or a second `--local` run.
var ErrReviewLocked = errors.New("this head SHA is already being reviewed")

// LocalLockKey is the advisory-lock name for one merge request at one head SHA
// (§15). It mirrors the River uniqueness key exactly, which is the point: the
// lock exists to give `--local` the mutual exclusion River gives the queue.
func LocalLockKey(args ReviewArgs) string {
	return fmt.Sprintf("review:%d:%d:%s", args.ProjectID, args.MRIID, args.HeadSHA)
}

// TryLock takes a session-level advisory lock keyed by hashtext(name), without
// blocking. The returned func releases the lock and the connection; it is safe
// to call it even when ok is false (it is then a no-op).
//
// The lock is session-scoped, so it must be held on one dedicated connection
// for the whole run — hence the checked-out *pgxpool.Conn rather than a pooled
// Exec. hashtext returns int4 and pg_try_advisory_lock takes int8; the implicit
// widening is why a single-argument call works.
//
// Note the collision property this inherits: hashtext is a 32-bit hash, so two
// unrelated keys can in principle share a lock. The cost of a collision here is
// one `--local` run declining to start with a clear message, which is the same
// failure mode as a genuine conflict.
func TryLock(ctx context.Context, pool *pgxpool.Pool, name string) (release func(), ok bool, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return func() {}, false, fmt.Errorf("acquire connection for advisory lock: %w", err)
	}

	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtext($1))", name).Scan(&locked); err != nil {
		// The statement may have taken the lock and failed on the way back —
		// a cancelled context is the ordinary way to get here. Release through
		// the same path a successful lock takes, because returning the
		// connection to the pool while it still holds a session lock leaks that
		// lock for the life of the connection, and every later acquirer then
		// waits on something no code believes it holds.
		releaser(ctx, conn, name)()
		return func() {}, false, fmt.Errorf("take advisory lock %q: %w", name, err)
	}
	if !locked {
		// Nothing to unlock: pg_try_advisory_lock returning false means the
		// lock was not granted.
		conn.Release()
		return func() {}, false, nil
	}

	return releaser(ctx, conn, name), true, nil
}

// Locker takes the same advisory lock as TryLock, but blocks until it is
// granted. It exists for the git mirror (internal/git.Locker, which this
// satisfies structurally — that package must not depend on a database, and this
// one must not depend on git).
//
// Blocking rather than try-and-fail is the right shape there: two reviews of
// one repository both need the mirror, and the second one waiting a few seconds
// for the first one's fetch is the whole point. Failing instead would put it
// back on the silently-degraded diff-only path the lock exists to remove.
//
// Ordering, since this lock is taken while the review lock is already held:
// review → mirror, always, and nothing ever acquires a review lock while
// holding a mirror lock. One global order means no cycle, so the blocking wait
// cannot deadlock against TryLock.
type Locker struct{ pool *pgxpool.Pool }

// NewLocker builds a Locker over the shared pool.
func NewLocker(pool *pgxpool.Pool) *Locker { return &Locker{pool: pool} }

// Lock blocks until the named lock is held or ctx is done.
//
// It holds one checked-out connection for as long as the caller holds the lock,
// which for a mirror fetch is the length of that fetch. That connection is
// budgeted for in the composition root's pool sizing.
func (l *Locker) Lock(ctx context.Context, name string) (func(), error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection for advisory lock: %w", err)
	}

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", name); err != nil {
		// A cancelled wait is the expected error here, and it is also the one
		// case where Postgres may have granted the lock just as the cancel
		// arrived. Release through the same path a successful lock takes, so
		// the session cannot keep a lock this call reports as not held.
		releaser(ctx, conn, name)()
		return nil, fmt.Errorf("take advisory lock %q: %w", name, err)
	}
	return releaser(ctx, conn, name), nil
}

// releaser builds the release func both lock flavours hand back.
//
// It is idempotent, because callers legitimately release on more than one path:
// a deferred release plus an explicit one on an early return. Releasing a
// pooled connection twice panics, which would turn tidy-up into a crash.
func releaser(ctx context.Context, conn *pgxpool.Conn, name string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			// Unlock on a context detached from the caller's: a cancelled run
			// must still release the lock, or the next attempt fails for the
			// wrong reason. (Releasing the connection alone would not do it —
			// a pooled connection is reused, not closed, and a session-level
			// advisory lock outlives the checkout.) The error is unactionable
			// here; the release below bounds the damage either way.
			_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(hashtext($1))", name)
			conn.Release()
		})
	}
}
