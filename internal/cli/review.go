package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/riverqueue/river/rivertype"
	"github.com/tkcrm/mx/logger"
	"github.com/urfave/cli/v3"

	"github.com/sxwebdev/ai-reviewer/internal/app"
	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/jobs"
	"github.com/sxwebdev/ai-reviewer/internal/service"
)

// waitPoll / waitBudget bound `--wait`. Polling (rather than River's event
// subscription) is what a separate process can do: the worker that finishes the
// job usually lives in another replica entirely.
const (
	waitPoll   = 2 * time.Second
	waitBudget = 45 * time.Minute // above the review job's own 30m timeout
)

func reviewCommand(boot logger.ExtendedLogger) *cli.Command {
	return &cli.Command{
		Name:      "review",
		Usage:     "Review one merge request",
		ArgsUsage: "<url | group/repo!iid | project-id:iid>",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "publish",
				Usage: "Publish the findings to GitLab, overriding service.ai_review_publish_enabled",
			},
			&cli.BoolFlag{
				Name:  "wait",
				Usage: "Poll the queued job until it finishes and print the result",
			},
			&cli.BoolFlag{
				Name:  "local",
				Usage: "Run the review in this process instead of queueing it (debugging)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref := cmd.Args().First()
			if ref == "" {
				return errors.New("a merge request reference is required")
			}
			if cmd.Bool("local") && cmd.Bool("wait") {
				return errors.New("--wait is meaningless with --local: the review runs here")
			}

			a, err := open(ctx, boot, cmd)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			if cmd.Bool("local") {
				return runLocalReview(ctx, a, ref, cmd.Bool("publish"))
			}
			return queueReview(ctx, a, ref, cmd.Bool("publish"), cmd.Bool("wait"))
		},
	}
}

// resolveRef turns a user-supplied reference into complete job args.
//
// The GitLab round trip is not optional. Uniqueness is keyed on
// (project_id, mr_iid, head_sha), so a job inserted from a path with no head
// SHA would hash differently from the scanner's and produce a second, parallel
// review of the same merge request — exactly what the unique key exists to
// prevent.
func resolveRef(ctx context.Context, a *app.App, gl gitlab.API, ref string) (jobs.ReviewArgs, error) {
	parsed, err := gitlab.ParseRef(ref, a.Config.GitLab.BaseURL)
	if err != nil {
		return jobs.ReviewArgs{}, err
	}

	proj, err := gl.GetProject(ctx, parsed.ProjectKey())
	if err != nil {
		return jobs.ReviewArgs{}, fmt.Errorf("resolve project: %w", err)
	}
	mr, err := gl.GetMR(ctx, parsed.ProjectKey(), parsed.IID)
	if err != nil {
		return jobs.ReviewArgs{}, fmt.Errorf("resolve merge request !%d: %w", parsed.IID, err)
	}
	// service.HeadSHA, not mr.SHA: it is the one derivation every producer of a
	// review job shares. The scanner keys its job on diff_refs.head_sha, and the
	// two fields disagree for a moment right after a push — long enough for this
	// command to insert a second job, under a different unique key, for work
	// already queued.
	head := service.HeadSHA(mr)
	if head == "" {
		return jobs.ReviewArgs{}, fmt.Errorf("merge request !%d has no head SHA", parsed.IID)
	}

	args := jobs.ReviewArgs{
		ProjectPath: proj.PathWithNamespace,
		ProjectID:   proj.ID,
		MRIID:       parsed.IID,
		HeadSHA:     head,
	}
	// The team is advisory here — it labels metrics and picks the review
	// settings — so a repository outside every configured team is a warning,
	// not a refusal: reviewing an unlisted MR on request is legitimate.
	if team, ok := app.TeamForRepository(a.Config, proj.PathWithNamespace); ok {
		args.Team = team.Name
	} else {
		fmt.Printf("note: %s is not in any configured team; the review runs with default settings\n",
			proj.PathWithNamespace)
	}
	return args, nil
}

// queueReview inserts the job and, when asked, waits for it.
func queueReview(ctx context.Context, a *app.App, ref string, publish, wait bool) error {
	gl, err := a.GitLabClient()
	if err != nil {
		return err
	}
	args, err := resolveRef(ctx, a, gl, ref)
	if err != nil {
		return err
	}
	// --publish travels in the job args, never in process config: it is the only
	// way the flag can reach a worker in another replica. It is deliberately
	// outside the uniqueness key, so the insert can still collapse — handled
	// below.
	if publish {
		args.Publish = &publish
	}

	pg, q, err := a.Queue(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = pg.Stop(ctx) }()

	res, err := q.EnqueueReview(ctx, args)
	if err != nil {
		return err
	}

	if res.Deduplicated {
		if err := reportCollapse(res, publish); err != nil {
			return err
		}
	} else {
		fmt.Printf("queued review job %d for %s!%d @%s\n",
			res.ID(), args.ProjectPath, args.MRIID, shortSHA(args.HeadSHA))
	}

	if !wait {
		return nil
	}
	return waitForJob(ctx, q, res.ID())
}

// reportCollapse explains an insert that folded into an existing job.
//
// The case that matters is a `--publish` request colliding with a scanner run
// that is not going to publish: the job the user asked for does not exist, and
// the one that survives will finish as a dry run. Reporting success there would
// mean the command silently did not do what was asked (§15).
func reportCollapse(res jobs.InsertResult, publish bool) error {
	existing, err := jobs.DecodeArgs[jobs.ReviewArgs](res.Job)
	if err != nil {
		// Unreadable args are no reason to hide the collapse itself.
		fmt.Printf("a review of this head SHA is already in flight (job %d)\n", res.ID())
		return nil //nolint:nilerr // the collapse is reported; the decode is only for the detail below
	}

	existingPublishes := existing.Publish != nil && *existing.Publish
	if publish && !existingPublishes {
		return cli.Exit(fmt.Sprintf(
			"a review of this head SHA is already running as job %d and it will NOT publish.\n"+
				"Your --publish was not applied — the job was deduplicated by (project, iid, head_sha).\n"+
				"Either wait for it and re-run with --publish afterwards (`--wait`), or run the review\n"+
				"in this process with `--local --publish`.", res.ID()), 1)
	}

	fmt.Printf("a review of this head SHA is already in flight (job %d); nothing new was queued\n", res.ID())
	return nil
}

// waitForJob polls until the job settles and reports what happened.
func waitForJob(ctx context.Context, q *jobs.Queue, id int64) error {
	fmt.Printf("waiting for job %d…\n", id)

	row, err := q.Wait(ctx, id, waitPoll, waitBudget)
	if errors.Is(err, jobs.ErrWaitTimeout) {
		return cli.Exit(fmt.Sprintf("job %d is still %s after %s", id, row.State, waitBudget), 1)
	}
	if err != nil {
		return err
	}

	switch row.State {
	case rivertype.JobStateCompleted:
		fmt.Printf("job %d completed after %d attempt(s)\n", id, row.Attempt)
		return nil
	default:
		detail := string(row.State)
		if n := len(row.Errors); n > 0 {
			detail += ": " + strings.TrimSpace(row.Errors[n-1].Error)
		}
		return cli.Exit(fmt.Sprintf("job %d finished as %s", id, detail), 1)
	}
}

// runLocalReview executes the review in this process (§15).
//
// It runs the worker's own function — there is no second implementation of the
// pipeline — but River's uniqueness does not cover an in-process execution, so
// the service takes an advisory lock on the same (project, iid, head_sha) key
// first. Without it a local run racing the scanner would mean two LLM runs and
// two publishers, both seeing note_id IS NULL and posting every finding twice.
func runLocalReview(ctx context.Context, a *app.App, ref string, publish bool) error {
	rt, err := a.Runtime(ctx)
	if err != nil {
		return err
	}
	defer rt.Close(ctx)

	args, err := resolveRef(ctx, a, rt.GitLab, ref)
	if err != nil {
		return err
	}
	if publish {
		args.Publish = &publish
	}

	fmt.Printf("reviewing %s!%d @%s in this process…\n", args.ProjectPath, args.MRIID, shortSHA(args.HeadSHA))

	out, err := rt.Jobs.RunReviewLocal(ctx, args)
	if errors.Is(err, jobs.ErrReviewLocked) {
		return cli.Exit(
			"this head SHA is already being reviewed (by a queued job or another --local run).\n"+
				"Wait for it to finish, or follow the queued one with `review <ref> --wait`.", 1)
	}
	if err != nil {
		return err
	}
	if out == nil {
		fmt.Println("nothing to do")
		return nil
	}
	if out.SkipReason != "" {
		fmt.Printf("skipped: %s\n", explainSkip(out.SkipReason))
		return nil
	}
	fmt.Printf("%s: %d finding(s), risk %s, $%.4f, %s\n",
		out.Status, out.Findings, out.RiskLevel, out.CostUSD,
		(time.Duration(out.DurationMS) * time.Millisecond).Round(time.Millisecond))
	return nil
}

// explainSkip turns a skip reason into something an operator can act on.
//
// "skipped: head_moved" in particular reads like a failure and is not one: the
// review was bound to the SHA this command resolved, somebody pushed, and
// nothing was reviewed or recorded. Saying so — and saying that re-running is
// the fix — is the difference between a clear no-op and a silent one.
func explainSkip(reason domain.Reason) string {
	switch reason {
	case domain.ReasonHeadMoved:
		return string(reason) + " — the merge request was pushed to while this review was starting, " +
			"so nothing was reviewed. Re-run the command to review the new head."
	case domain.ReasonUpToDate:
		return string(reason) + " — this head SHA has already been reviewed; push a change or wait for a new head."
	case domain.ReasonDraft:
		return string(reason) + " — the merge request is a draft; mark it ready to have it reviewed."
	case domain.ReasonDisabled:
		return string(reason) + " — ai_review is switched off for the team that owns this repository."
	case domain.ReasonNotOpen:
		return string(reason) + " — the merge request is merged or closed."
	default:
		return string(reason)
	}
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
