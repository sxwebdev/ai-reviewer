package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sxwebdev/ai-reviewer/internal/jobs"
	"github.com/sxwebdev/ai-reviewer/internal/store/storetest"
)

// mirrorLockKey duplicates internal/git's unexported lockKey. Spelling the
// format out is deliberate and safe in the direction that matters: if it ever
// changes, this test locks a key the cache does not take, the cache proceeds,
// and the "must block" assertion below fails loudly.
func mirrorLockKey(projectPath string) string {
	return "git-mirror:gitlab.test/" + projectPath
}

// uniqueProject keeps every run on a lock key of its own. Advisory locks are
// database-wide and outlive a killed test binary by however long Postgres takes
// to notice the dead socket, so a fixed key makes one crashed run wedge every
// later one — and two test binaries sharing the dev database would serialize on
// each other for no reason.
func uniqueProject(t *testing.T) string {
	t.Helper()
	return "group/repo-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
}

// secondPool stands in for the other replica: its own pool means its own
// Postgres sessions, which is what makes an advisory lock taken here genuinely
// cross-process rather than a second checkout of the same one.
func secondPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), storetest.DSN(t))
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func initSourceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "Tester")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "init")
	return dir
}

// TestGitCacheTakesTheCrossProcessMirrorLock proves the wiring, not the locker.
//
// internal/git exposes WithLocker so a shared workdir is safe across replicas,
// and the in-process semaphore covers nothing beyond this process. An
// unimplemented seam looks identical to a wired one at every call site, and the
// failure it prevents is silent: the loser of a concurrent `git fetch` gets
// `cannot lock ref` and degrades to a diff-only review — no agent mode, no
// coverage, no interdiff — visible only as a Warn line.
//
// So: another replica holds the mirror lock, and a cache built the way Runtime
// builds it must wait for it rather than clone underneath it.
func TestGitCacheTakesTheCrossProcessMirrorLock(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	pool := storetest.Pool(t)
	src := initSourceRepo(t)
	proj := uniqueProject(t)

	a := &App{Config: testConfig(), Log: quietLogger()}
	a.Config.Review.WorkDir = t.TempDir()
	cache := a.gitCache(pool)

	// The other replica takes the mirror lock and holds it.
	release, err := jobs.NewLocker(secondPool(t)).Lock(t.Context(), mirrorLockKey(proj))
	if err != nil {
		t.Fatalf("hold the mirror lock: %v", err)
	}
	// Deferred as well as released explicitly below (releasing twice is a
	// no-op): the lock pins a checked-out connection, and pgxpool.Close waits
	// for it, so a t.Fatal between here and the release would hang the run
	// instead of failing it.
	defer release()

	blocked, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := cache.EnsureMirror(blocked, src, "https://gitlab.test", proj, ""); err == nil {
		t.Fatal("EnsureMirror cloned while another replica held the mirror lock")
	}

	// And it waited before touching the filesystem, not after: a cold clone that
	// had already started would leave the directory behind.
	bare := cache.BareDir("https://gitlab.test", proj)
	if _, err := os.Stat(bare); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the mirror was created anyway (stat %s: %v)", bare, err)
	}

	// Released, the same operation goes through — the lock serializes, it does
	// not refuse.
	release()
	if _, err := cache.EnsureMirror(t.Context(), src, "https://gitlab.test", proj, ""); err != nil {
		t.Fatalf("EnsureMirror after the lock was released: %v", err)
	}
	if _, err := os.Stat(bare); err != nil {
		t.Errorf("mirror missing after a successful EnsureMirror: %v", err)
	}

	// Nothing left holding: a mirror lock the cache forgot to release would
	// stall every later fetch of that repository, in this process and the other
	// replica alike, until the connection was recycled an hour later.
	assertNoAdvisoryLocks(t, pool)
}

// TestGitCacheSerializesTwoReplicasOnOneMirror is the case the fix exists for,
// end to end: two caches over one shared workdir, each with its own pool, both
// fetching the same mirror at once. Both must succeed. Without the injected
// locker the in-process semaphores are independent and the fetches race —
// which is how `cannot lock ref` was measured at 5 losers in 6.
func TestGitCacheSerializesTwoReplicasOnOneMirror(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	src := initSourceRepo(t)
	proj := uniqueProject(t)
	shared := t.TempDir() // one volume, two replicas

	newReplica := func(pool *pgxpool.Pool) *App {
		a := &App{Config: testConfig(), Log: quietLogger()}
		a.Config.Review.WorkDir = shared
		return a
	}
	poolA, poolB := storetest.Pool(t), secondPool(t)
	cacheA := newReplica(poolA).gitCache(poolA)
	cacheB := newReplica(poolB).gitCache(poolB)

	// Prime the mirror so both goroutines take the fetch path — the one that
	// actually contends on refs.
	if _, err := cacheA.EnsureMirror(t.Context(), src, "https://gitlab.test", proj, ""); err != nil {
		t.Fatalf("prime: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		cache := cacheA
		if i%2 == 1 {
			cache = cacheB
		}
		wg.Go(func() {
			_, errs[i] = cache.EnsureMirror(t.Context(), src, "https://gitlab.test", proj, "")
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent fetch %d failed: %v", i, err)
		}
	}
	assertNoAdvisoryLocks(t, poolA)
}
