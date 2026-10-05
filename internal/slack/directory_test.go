package slack_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/slack"
)

// fakeLister is a UserLister that counts loads, so tests can assert that the
// directory really shares one users.list instead of re-fetching.
type fakeLister struct {
	mu    sync.Mutex
	calls int
	users []slack.User
	err   error
	// before runs at the start of every load, inside the singleflight.
	before func()
}

func (f *fakeLister) ListUsers(context.Context) ([]slack.User, error) {
	if f.before != nil {
		f.before()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.users, nil
}

func (f *fakeLister) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func user(id, handle, display, real, email string) slack.User {
	return slack.User{
		ID:       id,
		Name:     handle,
		RealName: real,
		Profile:  slack.Profile{Email: email, DisplayName: display, RealName: real},
	}
}

func TestDirectoryLoadsOnceWithinTTL(t *testing.T) {
	t.Parallel()

	lister := &fakeLister{users: []slack.User{user("U1", "ann", "Ann", "Ann Lee", "ann@acme.io")}}
	dir := slack.NewDirectory(lister, slack.DirectoryConfig{})

	for range 5 {
		if _, err := dir.Snapshot(t.Context()); err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
	}
	if got := lister.count(); got != 1 {
		t.Fatalf("users.list calls = %d, want 1", got)
	}
}

func TestDirectoryReloadsAfterTTL(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	lister := &fakeLister{users: []slack.User{user("U1", "ann", "Ann", "Ann Lee", "ann@acme.io")}}
	dir := slack.NewDirectory(lister, slack.DirectoryConfig{
		TTL: 15 * time.Minute,
		Now: func() time.Time { return now },
	})

	first, err := dir.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !first.LoadedAt.Equal(now) {
		t.Errorf("LoadedAt = %v, want %v", first.LoadedAt, now)
	}

	// Just short of the TTL the cached snapshot is still served.
	now = now.Add(15*time.Minute - time.Nanosecond)
	second, err := dir.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if second != first {
		t.Error("snapshot was reloaded before the TTL expired")
	}
	if got := lister.count(); got != 1 {
		t.Fatalf("users.list calls = %d, want 1", got)
	}

	// At the TTL it reloads.
	now = now.Add(time.Nanosecond)
	third, err := dir.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if third == first {
		t.Error("snapshot was not reloaded after the TTL expired")
	}
	if got := lister.count(); got != 2 {
		t.Fatalf("users.list calls = %d, want 2", got)
	}
}

func TestDirectoryInvalidateForcesReload(t *testing.T) {
	t.Parallel()

	lister := &fakeLister{users: []slack.User{user("U1", "ann", "Ann", "Ann Lee", "ann@acme.io")}}
	dir := slack.NewDirectory(lister, slack.DirectoryConfig{})
	if _, err := dir.Snapshot(t.Context()); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	dir.Invalidate()
	if _, err := dir.Snapshot(t.Context()); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := lister.count(); got != 2 {
		t.Fatalf("users.list calls = %d, want 2", got)
	}
}

func TestDirectoryLoadErrorIsNotCached(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	lister := &fakeLister{err: wantErr}
	dir := slack.NewDirectory(lister, slack.DirectoryConfig{})

	if _, err := dir.Snapshot(t.Context()); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	lister.err = nil
	lister.users = []slack.User{user("U1", "ann", "Ann", "Ann Lee", "ann@acme.io")}
	snap, err := dir.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("Snapshot after recovery: %v", err)
	}
	if len(snap.Users) != 1 {
		t.Fatalf("users = %d, want 1 (a failed load must not poison the cache)", len(snap.Users))
	}
}

// blockingLister is a realistic lister for the context tests: it honours the
// context it is handed, and otherwise waits to be released.
type blockingLister struct {
	started chan struct{} // closed by the first load
	release chan struct{}
	users   []slack.User
	calls   atomic.Int64
	once    sync.Once
}

func (l *blockingLister) ListUsers(ctx context.Context) ([]slack.User, error) {
	l.calls.Add(1)
	l.once.Do(func() { close(l.started) })
	select {
	case <-l.release:
		return l.users, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestDirectorySnapshotDoesNotShareTheFirstCallersDeadline is the
// context-propagation guard. A singleflight worker belongs to whichever
// goroutine won the race, so a load run on that caller's context cancels every
// goroutine that joined the flight. Both teams' digests fire at 09:00 on one
// replica; team A nine minutes into its ten-minute budget would hand team B a
// DeadlineExceeded that was never B's, and every person in B's digest would
// degrade to a plain unmentioned name.
//
// Two assertions, because the moment B joins the flight is not observable: if
// the load is bound to A, either B is inside the flight and inherits A's
// cancellation, or B arrives after it collapsed and starts a second
// users.list — a Tier-2 method. Both are failures, and one of them always
// happens.
func TestDirectorySnapshotDoesNotShareTheFirstCallersDeadline(t *testing.T) {
	t.Parallel()

	lister := &blockingLister{
		started: make(chan struct{}),
		release: make(chan struct{}),
		users:   []slack.User{user("U1", "ann", "Ann", "Ann Lee", "ann@acme.io")},
	}
	dir := slack.NewDirectory(lister, slack.DirectoryConfig{})

	ctxA, cancelA := context.WithCancel(t.Context())
	go func() { _, _ = dir.Snapshot(ctxA) }() // wins the flight
	<-lister.started

	type result struct {
		snap *slack.Snapshot
		err  error
	}
	done := make(chan result, 1)
	joined := make(chan struct{})
	go func() {
		close(joined)
		s, err := dir.Snapshot(t.Context()) // team B: its own, healthy budget
		done <- result{s, err}
	}()
	<-joined

	cancelA()
	close(lister.release)

	got := <-done
	if got.err != nil {
		t.Fatalf("Snapshot: %v — a second caller inherited the first caller's cancellation", got.err)
	}
	if _, ok := got.snap.ByID("U1"); !ok {
		t.Error("the joined caller got an empty snapshot")
	}
	if n := lister.calls.Load(); n != 1 {
		t.Errorf("users.list called %d times, want 1: the flight collapsed instead of being shared", n)
	}
}

// The flip side: detaching the load must not make a caller uncancellable. A
// digest whose own deadline expires has to return, not sit on a worker slot
// until users.list finishes.
func TestDirectorySnapshotHonoursItsOwnContext(t *testing.T) {
	t.Parallel()

	lister := &blockingLister{started: make(chan struct{}), release: make(chan struct{})}
	dir := slack.NewDirectory(lister, slack.DirectoryConfig{})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := dir.Snapshot(ctx)
		done <- err
	}()
	<-lister.started
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Snapshot err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Snapshot ignored its own cancellation and waited for the shared load")
	}
	close(lister.release)
}

// TestDirectoryConcurrentLoadsShareOneCall is the singleflight guarantee: the
// digest jobs of several teams firing at 09:00 must produce one users.list.
func TestDirectoryConcurrentLoadsShareOneCall(t *testing.T) {
	t.Parallel()

	const goroutines = 8

	var hits atomic.Int64
	// The handler waits until every caller is at the door, so the test
	// exercises the collapsing of concurrent loads rather than the cache.
	var arrived sync.WaitGroup
	arrived.Add(goroutines)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		arrived.Wait()
		writeJSON(t, w, `{"ok":true,"members":[{"id":"U1","name":"ann"}]}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, slack.Config{})
	dir := slack.NewDirectory(c, slack.DirectoryConfig{})

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		snaps []*slack.Snapshot
	)
	for range goroutines {
		wg.Go(func() {
			arrived.Done()
			s, err := dir.Snapshot(t.Context())
			if err != nil {
				t.Errorf("Snapshot: %v", err)
				return
			}
			mu.Lock()
			snaps = append(snaps, s)
			mu.Unlock()
		})
	}
	wg.Wait()

	if got := hits.Load(); got != 1 {
		t.Fatalf("users.list requests = %d, want exactly 1", got)
	}
	if len(snaps) != goroutines {
		t.Fatalf("snapshots = %d, want %d", len(snaps), goroutines)
	}
	for _, s := range snaps[1:] {
		if s != snaps[0] {
			t.Fatal("callers got different snapshots from one load")
		}
	}
}

func TestSnapshotIndexes(t *testing.T) {
	t.Parallel()

	users := []slack.User{
		user("U1", "ann", "Ann", "Ann Lee", "Ann@Acme.io"),
		user("U2", "bob", "Bob", "Bob  Ray", "bob@acme.io"),
		user("U3", "bobby", "Bob", "Robert Ray", "bobby@acme.io"),
		{ID: "U4", Name: "gone", Deleted: true, Profile: slack.Profile{Email: "gone@acme.io"}},
		{ID: "U5", Name: "botty", IsBot: true, Profile: slack.Profile{Email: "bot@acme.io"}},
		{ID: "U6", Name: "app", IsAppUser: true},
		{Name: "no-id"},
	}
	lister := &fakeLister{users: users}
	dir := slack.NewDirectory(lister, slack.DirectoryConfig{})
	snap, err := dir.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if len(snap.Users) != 3 {
		t.Fatalf("indexed users = %d, want 3 (deleted, bots, app users and id-less rows are skipped)", len(snap.Users))
	}
	for _, id := range []string{"U4", "U5", "U6"} {
		if _, ok := snap.ByID(id); ok {
			t.Errorf("%s should not be indexed", id)
		}
	}

	// Every valid spelling of one key resolves to the same person.
	for _, key := range []string{"ann@acme.io", "ANN@ACME.IO", "  Ann@Acme.io  "} {
		u, ok := snap.ByEmail(key)
		if !ok || u.ID != "U1" {
			t.Errorf("ByEmail(%q) = %+v, %v; want U1", key, u, ok)
		}
	}
	if _, ok := snap.ByEmail("nobody@acme.io"); ok {
		t.Error("ByEmail matched an unknown address")
	}

	if got := snap.ByHandle("ANN"); len(got) != 1 || got[0].ID != "U1" {
		t.Errorf("ByHandle(ANN) = %+v", got)
	}
	// Two people share a display name: the index keeps both so the matcher
	// can report Ambiguous instead of guessing.
	if got := snap.ByDisplayName("bob"); len(got) != 2 {
		t.Errorf("ByDisplayName(bob) = %+v, want 2 candidates", got)
	}
	// Internal whitespace is collapsed on both sides.
	if got := snap.ByRealName("bob ray"); len(got) != 1 || got[0].ID != "U2" {
		t.Errorf("ByRealName(bob ray) = %+v", got)
	}
	if got := snap.ByRealName("nobody at all"); len(got) != 0 {
		t.Errorf("ByRealName(nobody at all) = %+v, want none", got)
	}
	if u, ok := snap.ByID(" U2 "); !ok || u.Name != "bob" {
		t.Errorf("ByID = %+v, %v", u, ok)
	}
}

func TestSnapshotEmailIndexKeepsFirstWinner(t *testing.T) {
	t.Parallel()

	lister := &fakeLister{users: []slack.User{
		user("U1", "ann", "Ann", "Ann Lee", "shared@acme.io"),
		user("U2", "ann2", "Ann Two", "Ann Two", "SHARED@acme.io"),
	}}
	dir := slack.NewDirectory(lister, slack.DirectoryConfig{})
	snap, err := dir.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	u, ok := snap.ByEmail("shared@acme.io")
	if !ok || u.ID != "U1" {
		t.Fatalf("ByEmail = %+v, %v; want the first indexed user", u, ok)
	}
}

func TestUserDisplayNameFallbacks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		user slack.User
		want string
	}{
		{"display name wins", user("U1", "ann", "Ann", "Ann Lee", ""), "Ann"},
		{"profile real name", slack.User{Name: "ann", Profile: slack.Profile{RealName: "Ann Lee"}}, "Ann Lee"},
		{"top-level real name", slack.User{Name: "ann", RealName: "Ann Lee"}, "Ann Lee"},
		{"handle last", slack.User{Name: "ann"}, "ann"},
		{"nothing at all", slack.User{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.user.DisplayName(); got != tt.want {
				t.Errorf("DisplayName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeKeyIsIdempotent(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"  Ann  Lee ", "ANN LEE", "ann\tlee", "ann lee"} {
		got := slack.NormalizeKey(in)
		if got != "ann lee" {
			t.Errorf("NormalizeKey(%q) = %q, want %q", in, got, "ann lee")
		}
		if again := slack.NormalizeKey(got); again != got {
			t.Errorf("NormalizeKey is not idempotent: %q → %q", got, again)
		}
	}
}
