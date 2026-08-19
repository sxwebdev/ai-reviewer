package linear

import (
	"cmp"
	"fmt"
	"strings"
)

// Stage is where one workflow state sits relative to the In Review state of its
// own team. It exists because the digest asks the board exactly one question —
// "has the author handed this work over yet?" — and that question has to be
// answerable for state names this service has never seen.
type Stage int

const (
	// StageUnknown means the ordering could not be established: the team's
	// workflow was unreadable, it has no single In Review state, the column is not
	// one the board reported, Linear returned a state type this build does not
	// know, or two columns tie for the same position. Every caller must fail
	// *open* on it — the board may not silence a merge request on the strength of
	// a comparison that did not happen.
	//
	// It is deliberately the zero value, which is what makes Workflow's map lookup
	// fail open by construction rather than by remembering to write a branch.
	StageUnknown Stage = iota
	// StageBeforeReview is a state the author still owns: the work has not been
	// offered for review, whether it never was or was handed back.
	StageBeforeReview
	// StageReviewOrLater is In Review itself and everything past it.
	StageReviewOrLater
)

// String renders the stage for logs and test failures.
func (s Stage) String() string {
	switch s {
	case StageBeforeReview:
		return "before_review"
	case StageReviewOrLater:
		return "review_or_later"
	default:
		return "unknown"
	}
}

// typeRank maps a Linear state type onto its position in Linear's own fixed
// progression. The order is Linear's, not a team's, which is what makes it safe
// to hard-code: a team can rename and reorder its columns freely but cannot make
// `backlog` come after `started`.
//
// An unrecognised type reports false rather than a guessed rank. Linear adding a
// seventh type must degrade to StageUnknown (and therefore to today's behaviour),
// never to "before review" — that would stop notifying reviewers across every
// team on the day of an upstream release.
func typeRank(t string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "triage":
		return 0, true
	case "backlog":
		return 1, true
	case "unstarted":
		return 2, true
	case "started":
		return 3, true
	case "completed":
		return 4, true
	case "canceled":
		return canceledRank, true
	}
	return 0, false
}

// canceledRank is the rank Linear's ordering gives `canceled`, kept as a name
// because classifyState has to single it out — see the comment there.
const canceledRank = 5

// CompareStates orders two workflow states the way the team's board does: by
// Linear's fixed type progression first, then by Position *within* one type.
//
// Position alone is not board order — it only ranks columns sharing a type — so
// anything that prints a board (doctor) has to use this rather than sorting on
// the float. States whose type this build cannot rank sort last, together, so
// they are visible as a group instead of scattered through the real columns.
func CompareStates(a, b WorkflowState) int {
	ra, aok := typeRank(a.Type)
	rb, bok := typeRank(b.Type)
	switch {
	case !aok && !bok:
		return cmp.Compare(a.Position, b.Position)
	case !aok:
		return 1
	case !bok:
		return -1
	}
	if c := cmp.Compare(ra, rb); c != 0 {
		return c
	}
	return cmp.Compare(a.Position, b.Position)
}

// statesPageSize is how many workflow states teamQuery asks for. It is Linear's
// documented maximum page size, and the query does not paginate: a board with
// more columns than this is not representable, so NewWorkflow says so rather than
// letting the operator read "found 0" as "somebody renamed the column".
const statesPageSize = 250

// Workflow is one Linear team's board, reduced to the single question the digest
// asks of it, pre-answered for every column the board reported.
//
// **The answer is precomputed per column id, and that is the whole point.** An
// issue carries its own copy of its state, from a different query, and the
// ordering must not depend on the fields of that copy: `Position` is a bare
// float64, so a `null` or an omitted `position` decodes to 0.0 with no error at
// all, and comparing that 0 against In Review's position grades every column
// *after* In Review as "never offered for review" — silencing every reviewer on
// every linked merge request of that team. That is the exact inversion of the
// fail-open rule above, and it cannot be fixed by validating the issue's copy,
// because 0 is a legitimate position. So the issue contributes only its state
// **id**, which is always present, and the board — one document, fetched with the
// ordering fields explicitly selected — contributes the order.
//
// The zero Workflow answers StageUnknown for everything, because a read from a
// nil map yields the zero Stage. A caller that could not load a team therefore
// needs no branch.
type Workflow struct {
	stageByID map[string]Stage
}

// NewWorkflow locates the team's In Review state and pre-answers Stage for every
// column on the board.
//
// Exactly one match is required, matched case-insensitively — the same rule
// doctor applies, deliberately, because a team with two states both named
// "in review" has no single answer to "is this before review" and a team with
// none has no answer at all. Both are configuration problems the operator has to
// see, so they are errors here rather than a silent StageUnknown.
func NewWorkflow(states []WorkflowState) (Workflow, error) {
	var review WorkflowState
	found := 0
	for _, s := range states {
		if isInReview(s.Name) {
			found++
			review = s
		}
	}
	if found != 1 {
		err := fmt.Errorf("expected exactly one %q workflow state, found %d", InReviewState, found)
		if len(states) >= statesPageSize {
			// Distinguishing this matters: without it a board too large to read
			// reports the same words as a renamed column, and the operator goes
			// looking for a rename that never happened.
			err = fmt.Errorf("%w (the board was read up to %d states and may be truncated)", err, statesPageSize)
		}
		return Workflow{}, err
	}
	reviewRank, ok := typeRank(review.Type)
	if !ok {
		return Workflow{}, fmt.Errorf("workflow state %q has unknown type %q", review.Name, review.Type)
	}
	// Every comparison is relative to this column, and it is addressed by id, so a
	// board that reports it without one cannot be ordered at all.
	if strings.TrimSpace(review.ID) == "" {
		return Workflow{}, fmt.Errorf("workflow state %q has no id", review.Name)
	}

	byID := make(map[string]Stage, len(states))
	for _, s := range states {
		id := strings.TrimSpace(s.ID)
		if id == "" {
			// Unaddressable, so it simply is not in the map and any card sitting on
			// it comes back StageUnknown.
			continue
		}
		byID[id] = classifyState(s, review, reviewRank)
	}
	return Workflow{stageByID: byID}, nil
}

// Stage classifies the column a card sits on. Only state.ID is read: see the
// Workflow doc comment for why the rest of the caller's copy is not trusted.
//
// A column the board did not report — a page truncated below it, a column created
// since the board was fetched — is StageUnknown, which fails open.
func (w Workflow) Stage(state WorkflowState) Stage {
	return w.stageByID[strings.TrimSpace(state.ID)]
}

func classifyState(state, review WorkflowState, reviewRank int) Stage {
	// Identity first, and by id rather than by name: this is the one column whose
	// answer must never depend on a float comparison.
	if strings.TrimSpace(state.ID) == strings.TrimSpace(review.ID) {
		return StageReviewOrLater
	}
	rank, ok := typeRank(state.Type)
	if !ok {
		return StageUnknown
	}
	// `canceled` is the one type whose rank lies about it. Linear puts it last so
	// that it sorts to the end of a board, but a canceled card is not work that
	// has progressed past review — it is work that stopped. An open merge request
	// on a canceled card needs its author, not three reviewers, so it is graded
	// as "not offered for review" and the author gets the row. Nothing is hidden:
	// StageBeforeReview always produces an author action (see the partition rule
	// in internal/service), which is the whole reason this may be decided on
	// board state at all.
	if rank == canceledRank {
		return StageBeforeReview
	}
	switch {
	case rank < reviewRank:
		return StageBeforeReview
	case rank > reviewRank:
		return StageReviewOrLater
	}
	// Same type as In Review, so only the team's own ordering separates them. An
	// exact tie is not resolvable: Linear reshuffles these floats and two columns
	// sharing a value have no order at all, so the comparison is refused rather
	// than decided by rounding. This is also what makes a teamQuery that stopped
	// selecting `position` degrade to "unknown" instead of to a wrong answer.
	switch {
	case state.Position < review.Position:
		return StageBeforeReview
	case state.Position > review.Position:
		return StageReviewOrLater
	default:
		return StageUnknown
	}
}
