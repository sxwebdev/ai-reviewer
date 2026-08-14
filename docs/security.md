# Security model

- **Go owns validation and positions, not the model.** The LLM supplies a file,
  a line and a side. Everything else — the file-in-diff check, the
  line→position mapping, the fallback ladder, severity filtering, dedupe,
  secret scrubbing, length and count caps — is deterministic Go code in
  `internal/review/validator.go` and `line_mapper.go`.
- **Findings only on changed lines.** A finding whose file is not in the MR's
  diff is dropped. The service never comments on pre-existing code.
- **Claude has no write access to anything.** `permission_mode: dontAsk` with a
  read-only tool allowlist; no `Edit`, no `Write`, no `git push`, no network
  access to GitLab. Publication is done by Go, after validation.
- **The tool allowlist is read-only *and* path-scoped**, and there are
  deliberately no `Bash` rules:

  ```yaml
  allowed_tools:
    ["Read(${worktree}/**)", "Grep(${worktree}/**)", "Glob(${worktree}/**)"]
  ```

  `${worktree}` is substituted per review with the detached checkout, which is
  what makes the worktree a boundary rather than just a working directory. The
  `Bash(git diff *)` / `Bash(git log *)` / `Bash(git show *)` rules this list
  used to carry were removed because an allow rule is a **prefix match** and
  every git subcommand that takes diff options accepts `--output=<path>` — an
  arbitrary-file write, enough to manufacture a clean verdict for the
  deterministic verifiers — and `--no-index <any file>`, an arbitrary-file read
  that steps straight around the path scope on `Read`. Both were reproduced
  against the real CLI with the shipped flags, denied nothing. History and the
  interdiff already reach the model through the prompt, assembled by Go from the
  mirror.

  **If you override `allowed_tools` in your own config, you override this
  default** — and an override is how a stale copy would put the grants back.
- **The `claude` subprocess inherits an allowlisted environment, never
  `os.Environ()`.** The service's own environment holds the GitLab PAT, the
  Slack token and the Postgres password (the reference deployment injects them
  with `envFrom`), and the subprocess reads merge requests an attacker may have
  authored, so inheritance is an allowlist: process basics, locale, terminal,
  XDG dirs, proxy/CA settings and claude's own switches, and nothing else. A
  deployment that needs more names them in `llm.claude.passthrough_env` (inherit
  the parent's value) or `llm.claude.extra_env` (set one). `doctor` prints the
  resolved shape, e.g. `env=allowlist(38 names + 1 prefixes)`.
- **Binary, vendored and generated files never reach the LLM**, and neither do
  paths matching `review.ignore_globs` — enforced in `parseDiffs`, the one
  chokepoint that also removes the file from the finding-eligible set, so an
  excluded path cannot receive a comment either. Both sides of a rename are
  checked: moving a file out of an excluded directory in the MR that changes it
  does not sidestep the rule.
- **Verifiers that execute repository code are opt-in.** The default set
  (`go_build`, `go_vet`, `py_syntax`) never runs the reviewed repository's code
  — `py_syntax` is a pure `ast.parse`. `go_test`, `tsc` and
  `review.coverage.enabled` do run it, and on a shared host that is arbitrary
  code execution from every repository you watch. Enable them only where that is
  acceptable, and never for repositories you do not control.
- **No secrets in logs.** Resolved secrets are registered with a process-wide
  redactor wired into the logging core, so tokens, bearer headers, private-token
  headers and emails are masked in log messages, in error text, in doctor output
  and in the `claude` subprocess's output.
- **Secrets never sit in a config file by design.** `config.Secret` renders as
  `[redacted]` in `String()`, JSON and YAML; the real value is reachable only
  through `Unmask()`.
- **The git token is never in a URL, a config file or `argv`** — it is passed to
  git through `http.extraHeader` in the subprocess environment.

---

---

[← back to the README](../README.md)
