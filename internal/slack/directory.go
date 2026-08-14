package slack

import (
	"context"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// defaultDirectoryTTL is how long a loaded workspace directory is reused.
const defaultDirectoryTTL = 15 * time.Minute

// defaultDirectoryLoadTimeout bounds one users.list load. The load is detached
// from every caller's context (see Snapshot), so it needs a deadline of its
// own; without one a hung Slack connection would leave the flight — and every
// caller queued behind it — waiting forever. Generous, because a large
// workspace legitimately pages: 200 pages at a 30s per-request timeout is the
// client's own worst case, and this is the backstop, not the budget.
const defaultDirectoryLoadTimeout = 2 * time.Minute

// UserLister is the slice of the Slack client the Directory needs. *Client
// satisfies it; tests substitute a fake.
type UserLister interface {
	ListUsers(ctx context.Context) ([]User, error)
}

// DirectoryConfig configures the workspace directory cache.
type DirectoryConfig struct {
	// TTL is how long a snapshot is served before it is reloaded
	// (slack.directory_ttl, default 15m).
	TTL time.Duration
	// Now is the clock. Nil means time.Now; tests inject their own so TTL
	// expiry can be exercised without sleeping.
	Now func() time.Time
	// LoadTimeout bounds one shared users.list load (default 2m). Zero means
	// the default.
	LoadTimeout time.Duration
}

// Directory is an in-process cache of the Slack workspace user list.
//
// Nothing here is persisted, and that is deliberate (plan §5.2): a
// gitlab-user → slack-user table would be derived data, fully rebuildable from
// users.list in seconds and needed twice a day, bought at the price of
// staleness (someone renames themselves or is deactivated and the row keeps
// tagging the wrong person), invalidation and a migration. After a restart the
// cache is simply empty. It looks like a missing feature; it is not.
//
// Loads are collapsed through singleflight so the digest jobs of several teams
// firing at 09:00 share one users.list call instead of one each — users.list is
// a Tier 2 method (~20 requests/minute).
type Directory struct {
	lister      UserLister
	ttl         time.Duration
	loadTimeout time.Duration
	now         func() time.Time

	sf singleflight.Group

	mu   sync.RWMutex
	snap *Snapshot
}

// NewDirectory builds a directory over lister.
func NewDirectory(lister UserLister, cfg DirectoryConfig) *Directory {
	if cfg.TTL <= 0 {
		cfg.TTL = defaultDirectoryTTL
	}
	if cfg.LoadTimeout <= 0 {
		cfg.LoadTimeout = defaultDirectoryLoadTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Directory{lister: lister, ttl: cfg.TTL, loadTimeout: cfg.LoadTimeout, now: cfg.Now}
}

// Snapshot returns the current workspace index, loading or refreshing it when
// the cached one is missing or older than the TTL.
//
// The shared load deliberately does not run under the caller's context. A
// singleflight worker belongs to whichever goroutine won the race, so a load
// started on that caller's context carries its deadline and its cancellation to
// every goroutine that joins the flight: both teams' digests fire at 09:00 on
// one replica, and team A being nine minutes into its ten-minute budget would
// hand team B a DeadlineExceeded that was never B's — degrading everyone in B's
// digest to a plain name for it. So the load gets a deadline of its own, and
// each caller waits on it only until *its own* context is done.
func (d *Directory) Snapshot(ctx context.Context) (*Snapshot, error) {
	if s := d.fresh(); s != nil {
		return s, nil
	}
	ch := d.sf.DoChan("users.list", func() (any, error) {
		// A flight we queued behind may have just refilled the cache.
		if s := d.fresh(); s != nil {
			return s, nil
		}
		// WithoutCancel keeps the caller's values (trace ids and the like) while
		// dropping its cancellation; the timeout is what bounds the load instead.
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.loadTimeout)
		defer cancel()

		users, err := d.lister.ListUsers(loadCtx)
		if err != nil {
			return nil, err
		}
		s := newSnapshot(users, d.now())
		d.mu.Lock()
		d.snap = s
		d.mu.Unlock()
		return s, nil
	})

	select {
	case <-ctx.Done():
		// The flight keeps running for whoever else is waiting, and its result
		// still lands in the cache.
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.(*Snapshot), nil
	}
}

// Invalidate drops the cached snapshot so the next Snapshot reloads.
func (d *Directory) Invalidate() {
	d.mu.Lock()
	d.snap = nil
	d.mu.Unlock()
}

// fresh returns the cached snapshot if it is still within the TTL.
func (d *Directory) fresh() *Snapshot {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.snap == nil {
		return nil
	}
	if d.now().Sub(d.snap.LoadedAt) >= d.ttl {
		return nil
	}
	return d.snap
}

// Snapshot is an immutable index of the workspace at LoadedAt. Lookups are
// keyed by the normalised form of each field (trimmed, lowercased, internal
// whitespace collapsed) so callers may pass raw GitLab values.
type Snapshot struct {
	LoadedAt time.Time
	Users    []User

	byID      map[string]User
	byEmail   map[string]User
	byHandle  map[string][]User
	byDisplay map[string][]User
	byReal    map[string][]User
}

// newSnapshot indexes users.
//
// Deactivated accounts and bots are skipped: mentioning them helps nobody, and
// a deleted account that still owns a display name would shadow the live human
// who inherited it.
func newSnapshot(users []User, loadedAt time.Time) *Snapshot {
	s := &Snapshot{
		LoadedAt:  loadedAt,
		byID:      make(map[string]User, len(users)),
		byEmail:   make(map[string]User, len(users)),
		byHandle:  make(map[string][]User, len(users)),
		byDisplay: make(map[string][]User, len(users)),
		byReal:    make(map[string][]User, len(users)),
	}
	for _, u := range users {
		if u.ID == "" || u.Deleted || u.IsBot || u.IsAppUser {
			continue
		}
		s.Users = append(s.Users, u)
		s.byID[u.ID] = u
		if k := NormalizeKey(u.Profile.Email); k != "" {
			// Emails are unique per workspace; first writer wins if Slack
			// ever disagrees, which keeps the index deterministic.
			if _, dup := s.byEmail[k]; !dup {
				s.byEmail[k] = u
			}
		}
		if k := NormalizeKey(u.Name); k != "" {
			s.byHandle[k] = append(s.byHandle[k], u)
		}
		if k := NormalizeKey(u.Profile.DisplayName); k != "" {
			s.byDisplay[k] = append(s.byDisplay[k], u)
		}
		if k := NormalizeKey(u.realName()); k != "" {
			s.byReal[k] = append(s.byReal[k], u)
		}
	}
	return s
}

// ByID returns the member with the given Slack user ID.
func (s *Snapshot) ByID(id string) (User, bool) {
	u, ok := s.byID[strings.TrimSpace(id)]
	return u, ok
}

// ByEmail returns the member with the given email.
func (s *Snapshot) ByEmail(email string) (User, bool) {
	u, ok := s.byEmail[NormalizeKey(email)]
	return u, ok
}

// ByHandle returns every member whose @handle equals name.
func (s *Snapshot) ByHandle(name string) []User { return s.byHandle[NormalizeKey(name)] }

// ByDisplayName returns every member whose display name equals name.
func (s *Snapshot) ByDisplayName(name string) []User { return s.byDisplay[NormalizeKey(name)] }

// ByRealName returns every member whose real name equals name.
func (s *Snapshot) ByRealName(name string) []User { return s.byReal[NormalizeKey(name)] }

// NormalizeKey is the single normalisation used for every directory key and
// every lookup: trim, collapse internal whitespace, lowercase. Comparison is
// exact after that — no fuzzy matching, ever, because a wrong mention is worse
// than a missing one.
func NormalizeKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}
