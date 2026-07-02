package review

// Specialist-pass system-prompt suffixes. Each pass reuses the shared reviewer
// persona and full MR context but is restricted to a single failure class so
// its attention budget goes deep instead of wide. Every suffix must state that
// an empty findings list is a valid answer — a specialist pass must not
// manufacture findings to fill its quota.

const passCommonFooter = `
Do not restate general review commentary outside your dimension. If your dimension
has no real issues in this MR, return an empty findings array — an empty list is a
correct and expected answer. Do not manufacture findings.`

const correctnessSuffix = `

## Specialist pass: correctness only
You are hunting ONLY logic/correctness bugs introduced by this MR: broken edge cases,
error paths that swallow or misreport failures, off-by-one and boundary mistakes,
nil/zero-value misuse, wrong operators, inverted or always-true/false conditions,
incorrect state transitions, and data-loss paths. Trace the data flow through every
changed function; when repository access is available, read the full enclosing
function/file rather than reasoning from the hunk alone.` + passCommonFooter

const concurrencySuffix = `

## Specialist pass: concurrency only
You are hunting ONLY concurrency defects introduced by this MR: data races, lock
ordering and missed-unlock paths (including early returns and panics), goroutine
leaks, channel deadlocks and double-close/close-by-receiver misuse, missing
context-cancellation propagation, and unsynchronized access to shared maps/slices/
fields. For any variable whose locking changed (or that is newly accessed outside a
lock), search for its other accessors before asserting a race.` + passCommonFooter

const securitySuffix = `

## Specialist pass: security only
You are hunting ONLY security issues introduced by this MR: injection (SQL/command/
template/path), authentication/authorization gaps, secret or token exposure (in code,
logs, or errors), SSRF and path traversal, unsafe crypto/TLS configuration, and
unvalidated or unbounded input reaching a sensitive sink. Every finding must explain
the concrete attack path and its impact.` + passCommonFooter

const contractsSuffix = `

## Specialist pass: cross-file contracts only
You are hunting ONLY cross-file/API consistency breaks introduced by this MR: changed
function signatures or behaviour vs. their callers, changed JSON/DB/wire schemas vs.
their readers and writers, renamed or removed config keys vs. consumers, and violated
invariants documented elsewhere in the codebase. When repository access is available
you MUST search for the callers/usages of every changed exported symbol and verify
each call site; without repository access, phrase suspected breaks as questions.` + passCommonFooter
