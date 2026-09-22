package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/tkcrm/mx/logger"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/scheduler"
)

// Config is everything the queue runtime needs from the application config.
// It is a plain struct rather than a pointer to config.Config so this package
// keeps no opinion about where the values came from.
type Config struct {
	// Teams is the configured team set, already mapped to the domain type. The
	// scan dispatcher and the digest schedule are both derived from it.
	Teams []domain.Team

	ScanInterval    time.Duration
	CleanupInterval time.Duration
	// DrainTimeout is River's SoftStopTimeout: how long in-flight jobs get to
	// finish on shutdown before the rest are cancelled (and left durable).
	DrainTimeout time.Duration

	// ReviewWorkers sizes the `review` queue. It is review.max_parallel — one
	// knob for review concurrency, never two that can drift apart.
	ReviewWorkers  int
	DefaultWorkers int
	PublishWorkers int
	SlackWorkers   int

	// PublishEnabled / SlackSendEnabled are the two global dry-run switches.
	// Both default to off so a fresh deployment observes without writing.
	PublishEnabled   bool
	SlackSendEnabled bool

	// WorkDir is review.workdir — the ephemeral root the cleanup job sweeps.
	WorkDir string
}

func (c Config) normalized() Config {
	if c.ScanInterval <= 0 {
		c.ScanInterval = 5 * time.Minute
	}
	if c.CleanupInterval <= 0 {
		c.CleanupInterval = time.Hour
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = time.Minute
	}
	if c.ReviewWorkers <= 0 {
		c.ReviewWorkers = 2
	}
	if c.DefaultWorkers <= 0 {
		c.DefaultWorkers = 2
	}
	if c.PublishWorkers <= 0 {
		c.PublishWorkers = 1
	}
	if c.SlackWorkers <= 0 {
		c.SlackWorkers = 1
	}
	return c
}

// Deps are the service-layer collaborators. Declared as interfaces here (see
// deps.go) so this package can be tested with fakes and internal/service never
// has to import River.
type Deps struct {
	Reviewer Reviewer
	Scanner  Scanner
	Digester Digester
	// Commander is optional: it is only reachable through the Slack socket, and a
	// deployment without an app-level token never opens one. Nil leaves the
	// slack_command worker unregistered, so a job of that kind — one queued
	// before the token was removed — stays in the queue rather than failing
	// repeatedly against a worker that cannot answer it.
	Commander Commander
}

func (d Deps) validate() error {
	var missing []string
	if d.Reviewer == nil {
		missing = append(missing, "Reviewer")
	}
	if d.Scanner == nil {
		missing = append(missing, "Scanner")
	}
	if d.Digester == nil {
		missing = append(missing, "Digester")
	}
	if len(missing) > 0 {
		return fmt.Errorf("jobs: missing dependencies: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Service owns the River client and its lifecycle. It is an mx service
// (Name/Start/Stop) and an mx health checker (Name/Interval/Healthy), so the
// launcher picks both up from one registration.
//
// The embedded *Queue makes the producer API available on the service itself:
// the workers enqueue their successors through the same client that works them.
type Service struct {
	*Queue

	log  logger.Logger
	cfg  Config
	pool *pgxpool.Pool
	deps Deps

	// schedules is one Daily per team, keyed by lower-cased name. Team names are
	// validated unique case-insensitively, and the CLI resolves --team the same
	// way, so the key is what makes "the schedule for this team" answerable from a
	// job's args alone.
	schedules map[string]scheduler.Daily

	// stopping is closed at the top of Stop. It is what withShutdown watches:
	// River's soft stop deliberately leaves in-flight jobs running, and one of the
	// four queues must not be left running. See withShutdown.
	stopping chan struct{}
	stopOnce sync.Once
}

// NewService wires the workers, the periodic schedule and the River client.
// The pool must already be connected — Postgres starts first.
func NewService(log logger.Logger, cfg Config, pool *pgxpool.Pool, deps Deps) (*Service, error) {
	if err := deps.validate(); err != nil {
		return nil, err
	}
	cfg = cfg.normalized()

	// Built here rather than at first use so a malformed slot or an unknown zone
	// fails at startup instead of at the first digest slot. Every team is built,
	// not just the first, because a schedule nobody validated until its first slot
	// fires is the failure this is placed here to prevent.
	schedules, err := teamSchedules(cfg.Teams)
	if err != nil {
		return nil, err
	}

	s := &Service{
		log: log, cfg: cfg, pool: pool, deps: deps, schedules: schedules,
		stopping: make(chan struct{}),
	}

	workers := river.NewWorkers()
	river.AddWorker(workers, &ScanWorker{log: log, svc: s})
	river.AddWorker(workers, &ScanRepoWorker{log: log, svc: s, scanner: deps.Scanner})
	river.AddWorker(workers, &ReviewWorker{log: log, svc: s, reviewer: deps.Reviewer})
	river.AddWorker(workers, &PublishReviewWorker{log: log, reviewer: deps.Reviewer})
	river.AddWorker(workers, &DigestWorker{log: log, svc: s, digester: deps.Digester})
	river.AddWorker(workers, &SlackSendWorker{log: log, digester: deps.Digester})
	if deps.Commander != nil {
		river.AddWorker(workers, &SlackCommandWorker{log: log, svc: s, commander: deps.Commander})
	}
	river.AddWorker(workers, &CleanupWorker{log: log, workdir: cfg.WorkDir})

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			QueueDefault: {MaxWorkers: cfg.DefaultWorkers},
			QueueReview:  {MaxWorkers: cfg.ReviewWorkers},
			QueuePublish: {MaxWorkers: cfg.PublishWorkers},
			QueueSlack:   {MaxWorkers: cfg.SlackWorkers},
		},
		Workers:      workers,
		PeriodicJobs: periodicJobs(cfg, schedules),
		// Without this, Stop hard-cancels every in-flight job immediately. The
		// cancelled ones are durable and would be re-run, but a review cancelled
		// at minute 25 of 30 has burned its tokens for nothing.
		SoftStopTimeout: cfg.DrainTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("create river client: %w", err)
	}
	s.Queue = &Queue{client: client}
	return s, nil
}

// teamSchedules resolves every configured team's digest schedule.
//
// A team whose slots or zone cannot be parsed is a startup failure naming the
// team: the alternative is a service that runs for every other team and silently
// never sends this one's digest.
func teamSchedules(teams []domain.Team) (map[string]scheduler.Daily, error) {
	out := make(map[string]scheduler.Daily, len(teams))
	for _, t := range teams {
		sched, err := scheduler.NewDigest(digestSpec(t))
		if err != nil {
			return nil, fmt.Errorf("digest schedule for team %q: %w", t.Name, err)
		}
		out[strings.ToLower(t.Name)] = sched
	}
	return out, nil
}

// digestSpec is one team's schedule as the scheduler wants it. It exists so the
// four resolved values travel together: two of them are []string, and a caller
// assembling them by hand can swap the pair without anything failing to compile.
func digestSpec(t domain.Team) scheduler.DigestSpec {
	return scheduler.DigestSpec{
		Slots:        t.DigestSlots,
		Timezone:     t.DigestTimezone,
		SkipWeekdays: t.DigestSkipWeekdays,
		SkipDates:    t.DigestSkipDates,
	}
}

// periodicJobs builds the leader-only schedule (§6.6). River elects one leader
// across replicas, so these insert exactly once however many pods are running.
func periodicJobs(cfg Config, schedules map[string]scheduler.Daily) []*river.PeriodicJob {
	jobs := []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(cfg.ScanInterval),
			func() (river.JobArgs, *river.InsertOpts) { return ScanArgs{}, nil },
			// RunOnStart so a freshly deployed replica does not idle for a whole
			// scan interval before noticing the merge requests already waiting.
			&river.PeriodicJobOpts{RunOnStart: true},
		),
		river.NewPeriodicJob(
			river.PeriodicInterval(cfg.CleanupInterval),
			func() (river.JobArgs, *river.InsertOpts) { return CleanupArgs{}, nil },
			// RunOnStart: a SIGKILL is exactly what leaves worktrees behind, and
			// a restart usually follows one.
			&river.PeriodicJobOpts{RunOnStart: true},
		),
	}

	// One periodic job per team rather than one fan-out job: River's constructor
	// returns a single JobArgs, and a fan-out kind would be an eighth job kind
	// that exists only to work around that. Each team's DigestArgs hash to a
	// different unique key, so the slot cannot be double-sent either way.
	//
	// Per-team schedules make this the only shape that works at all: one River
	// periodic job carries one PeriodicSchedule, so two teams on different slots
	// need two jobs regardless.
	for _, team := range cfg.Teams {
		schedule, ok := schedules[strings.ToLower(team.Name)]
		if !ok {
			// Unreachable: teamSchedules is built from the same slice. Skipping is
			// the honest response to a map that has drifted from cfg.Teams —
			// scheduling a team in the wrong zone would be worse than not
			// scheduling it, and NewService already refused every real failure.
			continue
		}
		jobs = append(jobs, river.NewPeriodicJob(
			schedule,
			func() (river.JobArgs, *river.InsertOpts) {
				// Attempt 0 for every scheduled run: that is what makes two
				// replicas firing the same slot collapse into one job.
				args, ok := digestSlotForNow(team.Name, schedule, time.Now())
				if !ok {
					// A nil JobArgs is River's "nothing to insert" — see
					// digestSlotForNow for the only case that produces it.
					return nil, nil
				}
				return args, nil
			},
			// RunOnStart, with the day check inside the constructor doing the
			// work the flag's name suggests.
			//
			// River's periodic enqueuer is leader-only and keeps its schedule in
			// memory: a newly elected leader starts from Next(now) with no idea
			// what the previous one did. Every rolling deploy is a leadership
			// turnover, and a slot falling into that gap was simply never
			// inserted — no digest_runs row, no job, nothing looking for it.
			// RunOnStart is documented as the hedge for exactly that.
			//
			// The reason it was off — "a restart between slots must not re-fire
			// the earlier one" — is handled twice over: the constructor refuses a
			// slot from an earlier day, and BuildDigest is idempotent per
			// (team, run_date, slot, attempt), so a slot that WAS delivered is
			// recognised and reused rather than sent again. What is left is the
			// intended reconciliation: a slot of today whose row is missing gets
			// built, late, instead of never.
			&river.PeriodicJobOpts{RunOnStart: true, ID: "digest:" + team.Name},
		))
	}
	return jobs
}

// slotSnapMargin is how far before a slot an instant is still treated as that
// slot's firing.
//
// It exists to cancel River's own margin, not to be lenient. River's periodic
// enqueuer runs every job whose next run is before `now + 100ms`, deliberately,
// so that a timer firing a hair early does not postpone a slot by a whole cycle
// — and it records the intended instant in the job's scheduled_at while handing
// the constructor nothing. A constructor that asks time.Now() can therefore be
// microseconds *before* the slot it was fired for.
//
// Observed, not theorised: on 2026-08-24 the 14:00 job was inserted at
// 13:59:59.999745 with scheduled_at 14:00:00, so SlotAt answered "09:00" — the
// previous slot, already delivered that morning by the RunOnStart path. The
// args matched the existing run exactly, BuildDigest handed back the delivered
// one, slack_send found its message already sent, and the team's 14:00 digest
// simply never arrived, with nothing anywhere saying why.
//
// Two seconds rather than River's own 100ms: the gap between the enqueuer
// reading the clock and this function reading it again is unbounded in
// principle, and the cost of being generous is nil — slots are hours apart, so
// the last two seconds before one are not a meaningful part of the previous
// slot's territory.
const slotSnapMargin = 2 * time.Second

// digestSlotForNow names the digest run the instant `at` belongs to, and
// reports whether it should be inserted at all.
//
// A scheduled firing always passes: the enqueuer calls this at (or fractionally
// before) a slot, so the slot it snaps to is that same slot, today. The day
// check bites only on the RunOnStart path, where `at` can be any time a replica
// happened to win the leader election — 03:00, when the slot the instant belongs
// to is yesterday's last one. Yesterday's digest is not worth sending; today's,
// missing, is.
func digestSlotForNow(team string, schedule scheduler.Daily, at time.Time) (DigestArgs, bool) {
	// Snap forward onto a slot we are within a whisker of, so the name matches the
	// firing rather than the clock — see slotSnapMargin.
	//
	// Compared with After rather than by subtracting: a schedule that skips every
	// day answers neverTime, and while time.Sub clamps rather than overflowing,
	// not relying on that is cheaper than remembering it does.
	if next := schedule.Next(at); !next.After(at.Add(slotSnapMargin)) {
		at = next
	}
	// A skipped day is skipped on this path too. Next steps over it, so the timer
	// never fires there — but RunOnStart does not go through Next, and a replica
	// starting on a Saturday morning would otherwise build and send the very
	// digest the skip list exists to suppress.
	//
	// Checked after the snap, which cannot move `at` onto a skipped day: Next only
	// ever returns one the skip list allows.
	if schedule.Skipped(at) {
		return DigestArgs{}, false
	}
	slot, ok := schedule.SlotAt(at)
	if !ok {
		// A schedule with no slots cannot name a run. Config validation rejects
		// one, and inventing a name here would file the digest under a slot the
		// unique index cannot recognise on the next pass.
		return DigestArgs{}, false
	}
	if slot.At.Format(runDateLayout) != at.In(schedule.Location()).Format(runDateLayout) {
		return DigestArgs{}, false
	}
	return NewDigestArgs(team, schedule, at, 0), true
}

// runDateLayout is the digest_runs.run_date encoding shared by the scheduler,
// the job args and the CLI.
const runDateLayout = "2006-01-02"

// Teams returns the configured teams.
func (s *Service) Teams() []domain.Team { return s.cfg.Teams }

// ScheduleFor is one team's digest schedule, and reports whether that team is
// configured. The CLI needs it to name the slot and run date of a manual digest
// exactly as the scheduled run would have — which, with per-team schedules, is
// only true if it asks for the same team's.
func (s *Service) ScheduleFor(team string) (scheduler.Daily, bool) {
	sched, ok := s.schedules[strings.ToLower(team)]
	return sched, ok
}

// NewDigestArgs names the digest run that the instant `at` belongs to: the most
// recent scheduled slot at or before it.
//
// Both the periodic schedule and the CLI go through it, so a manual `digest`
// lands on exactly the slot and calendar day the scheduled run would have used
// — which is what makes the digest_runs unique index able to tell a repeat from
// a fresh slot.
//
// The snapping itself lives in scheduler.Daily.SlotAt — Daily.Times is the list
// of valid slots, so it is the only place that can answer which one an instant
// belongs to. See that method for why formatting the clock instead would defeat
// the digest_runs unique index.
//
// Everything resolves in the schedule's own zone: at 23:30 UTC the Moscow
// calendar day is already tomorrow, and before the first slot of the day the
// run belongs to yesterday's last one.
func NewDigestArgs(team string, schedule scheduler.Daily, at time.Time, attempt int) DigestArgs {
	slot, ok := schedule.SlotAt(at)
	if !ok {
		// Only a schedule with no slots gets here, and config validation rejects
		// one. Describe the instant itself so the args stay well-formed rather
		// than carrying an empty run date.
		local := at.In(schedule.Location())
		return DigestArgs{
			Team:    team,
			Slot:    local.Format(scheduler.SlotLayout),
			RunDate: local.Format(runDateLayout),
			Attempt: attempt,
		}
	}

	return DigestArgs{
		Team:    team,
		Slot:    slot.Name,
		RunDate: slot.At.Format(runDateLayout),
		Attempt: attempt,
	}
}

// PublishEnabled is the configured default for review publication, used when a
// review job's args do not override it.
func (s *Service) PublishEnabled() bool { return s.cfg.PublishEnabled }

func (s *Service) Name() string { return "river-jobs" }

// Start runs the River client and blocks until ctx is cancelled.
//
// River runs on a context detached from ctx on purpose: mx cancels the service
// context at the top of shutdown, and if River's work context inherited it,
// every in-flight job would be hard-cancelled before Stop had a chance to drain
// them. The drain is driven solely by Stop.
func (s *Service) Start(ctx context.Context) error {
	if err := s.client.Start(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("start river client: %w", err)
	}
	<-ctx.Done()
	return nil
}

// Stop drains in-flight jobs for up to SoftStopTimeout (or ctx's deadline,
// whichever comes first) and then cancels the rest, which stay durable. The
// service must therefore be registered with a ShutdownTimeout greater than
// DrainTimeout, or mx's default 10s would cut the drain short.
//
// Reviews are exempt from the drain — see withShutdown — so the wait here is the
// publish and Slack queues finishing their seconds-long work, not a review that
// could never have finished anyway.
func (s *Service) Stop(ctx context.Context) error {
	s.beginStop()
	return s.client.Stop(ctx)
}

// beginStop announces the shutdown to every context withShutdown handed out. It
// runs before River is asked to drain, so a review is cancelled while the drain
// is still ahead of it rather than at the end of it.
func (s *Service) beginStop() { s.stopOnce.Do(func() { close(s.stopping) }) }

// withShutdown derives a context that is cancelled as soon as Stop begins.
//
// The drain window exists so in-flight work can finish, and for publish and Slack
// it does: those jobs are network calls measured in seconds. A review is measured
// in minutes — 4 to 10 of them — against a one-minute default drain, so it can
// never finish. Left running it does exactly two things: it holds the shutdown
// open for the full window, and it keeps paying the model for a result that is
// thrown away when the window closes. Cancelling it immediately costs nothing new,
// because the cancellation is already a first-class path: service.recordFailure
// recognises our own shutdown, discards the attempt row rather than counting it
// against the head SHA, and the next scan enqueues the same SHA again.
//
// The goroutine is a lifecycle watcher, not deferred work — the rule that deferred
// work must be a River job does not apply to it. There is one per in-flight review,
// bounded by the review queue's worker count, and each ends with its job.
func (s *Service) withShutdown(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-s.stopping:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// Interval is how often mx polls Healthy for the /readyz overlay.
func (s *Service) Interval() time.Duration { return 15 * time.Second }

// Healthy is the readiness probe (§14.4): the pool answers and the River client
// is running. Deliberately local — no GitLab, no Slack, no repository walk.
// A probe that talks to a third party turns their outage into ours.
func (s *Service) Healthy(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	select {
	case <-s.client.Stopped():
		return errors.New("river client is not running")
	default:
		return nil
	}
}

// RunReviewLocal executes a review in this process, under the advisory lock —
// the CLI's `--local` path (§15).
//
// It runs the same code as the worker; there is no second pipeline. What it
// does not get is River's uniqueness, so it takes
// pg_try_advisory_lock(hashtext('review:<project>:<iid>:<sha>')) first. Without
// that, a local run racing the scanner would mean two LLM runs and two
// publishers, each seeing note_id IS NULL and posting every finding twice.
func (s *Service) RunReviewLocal(ctx context.Context, args ReviewArgs) (*ReviewOutcome, error) {
	out, ok, err := s.lockedReview(ctx, args, s.deps.Reviewer)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrReviewLocked, LocalLockKey(args))
	}
	return out, nil
}

// lockedReview runs one review holding the (project, iid, head_sha) advisory
// lock, reporting ok=false when somebody else holds it.
//
// BOTH entry points go through here — `--local` and the River worker — and that
// is the whole point. A lock only one side takes excludes a second `--local`
// run and nothing else, which is precisely not the case §15 introduces it for:
// River's uniqueness covers queued jobs, the lock covers the in-process run,
// and only holding it on both sides makes the two exclusive.
//
// The lock is session-scoped, so it holds one pooled connection for the whole
// review — up to the 30-minute timeout. The composition root sizes the pool for
// that (two connections per concurrent review), because a review queue that
// cannot get a connection is worse than the race this prevents.
func (s *Service) lockedReview(ctx context.Context, args ReviewArgs, reviewer Reviewer) (*ReviewOutcome, bool, error) {
	unlock, ok, err := TryLock(ctx, s.pool, LocalLockKey(args))
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	defer unlock()

	out, err := s.runReview(ctx, args, reviewer)
	return out, true, err
}

// runReview is the single review body: resolve the effective publish decision,
// then run the service with (or without) the transactional publish hand-off.
// Both the worker and `--local` go through it.
func (s *Service) runReview(ctx context.Context, args ReviewArgs, reviewer Reviewer) (*ReviewOutcome, error) {
	publish := s.cfg.PublishEnabled
	if args.Publish != nil {
		publish = *args.Publish
	}

	req := ReviewRequest{
		Team:        args.Team,
		ProjectPath: args.ProjectPath,
		ProjectID:   args.ProjectID,
		MRIID:       args.MRIID,
		HeadSHA:     args.HeadSHA,
		Publish:     publish,
	}

	// A nil OnPersist is the dry-run contract: the review is computed, validated
	// and persisted, and no publication job exists to deliver it.
	var onPersist OnPersist
	if publish {
		onPersist = func(ctx context.Context, tx pgx.Tx, reviewID uuid.UUID) error {
			res, err := s.EnqueuePublishReviewTx(ctx, tx, reviewID)
			if err != nil {
				return err
			}
			if res.Deduplicated {
				// Another publication for this review is already in flight; the
				// review row is still committed by this transaction.
				s.log.Debugw("publish_review already queued",
					"review_id", reviewID, "job_id", res.ID())
			}
			return nil
		}
	}

	return reviewer.RunReview(ctx, req, onPersist)
}
